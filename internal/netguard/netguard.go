// Package netguard optionally refuses outbound connections to private,
// loopback and link-local addresses.
//
// Off by default, on purpose: watching services on the local network is most
// of what a self-hosted monitor is for. It exists for the other deployment —
// an instance shared by people who may edit monitors but should not be able to
// use the server as a probe into the network it sits in (the cloud metadata
// endpoint at 169.254.169.254 being the classic target). BLOCK_PRIVATE_TARGETS
// turns it on.
//
// Two layers, because neither is enough alone:
//
//   - Control is a net.Dialer hook. It sees the address actually being dialled,
//     after DNS resolution and after every HTTP redirect, so a name that
//     resolves to 127.0.0.1 or a 302 to the metadata endpoint cannot slip past
//     it. It only covers the checkers and clients that dial through a
//     net.Dialer this package configured.
//   - CheckConfig resolves the target fields of a monitor config before the
//     check runs. It covers every monitor type, including those whose client
//     library dials on its own, but it is a check-then-connect: a name that
//     changes its answer between the two is caught only by the first layer.
package netguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
)

// ErrBlocked reports a connection refused because its target is in a blocked
// range. Callers wrap it with the host they were asked to reach.
var ErrBlocked = errors.New("target is in a blocked address range (BLOCK_PRIVATE_TARGETS is on)")

var enabled atomic.Bool

// SetEnabled turns the guard on or off for the whole process. It is set once
// at startup from configuration; tests flip it around a case.
func SetEnabled(on bool) { enabled.Store(on) }

// Enabled reports whether the guard is on.
func Enabled() bool { return enabled.Load() }

var (
	cgnat    = netip.MustParsePrefix("100.64.0.0/10")
	thisNet  = netip.MustParsePrefix("0.0.0.0/8")
	nat64    = netip.MustParsePrefix("64:ff9b::/96")
	bcast    = netip.MustParseAddr("255.255.255.255")
	unixLike = []string{"unix", "unixgram", "unixpacket"}
)

// Blocked reports whether a is an address the guard refuses: loopback,
// private (RFC 1918, fc00::/7), link-local (which includes the cloud metadata
// address), carrier-grade NAT, "this network", unspecified, multicast and the
// broadcast address. IPv4-mapped and NAT64 forms are judged by the IPv4
// address they carry, so ::ffff:127.0.0.1 is loopback like 127.0.0.1.
func Blocked(a netip.Addr) bool {
	a = a.Unmap()
	if a.Is6() && nat64.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	switch {
	case !a.IsValid(),
		a.IsLoopback(),
		a.IsPrivate(),
		a.IsLinkLocalUnicast(),
		a.IsLinkLocalMulticast(),
		a.IsInterfaceLocalMulticast(),
		a.IsMulticast(),
		a.IsUnspecified(),
		a == bcast:
		return true
	case a.Is4() && (cgnat.Contains(a) || thisNet.Contains(a)):
		return true
	}
	return false
}

// Control is a net.Dialer.Control hook refusing blocked addresses while the
// guard is on. Unix sockets are refused too: they are the local machine.
func Control(network, address string, _ syscall.RawConn) error {
	if !Enabled() {
		return nil
	}
	for _, u := range unixLike {
		if network == u {
			return fmt.Errorf("unix socket %s: %w", address, ErrBlocked)
		}
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		// Dialers hand Control a resolved ip:port; anything else is not an
		// address this guard can vouch for, so it is refused rather than let
		// through.
		return fmt.Errorf("unparseable dial address %q: %w", address, ErrBlocked)
	}
	if Blocked(ap.Addr()) {
		return fmt.Errorf("%s: %w", ap.Addr(), ErrBlocked)
	}
	return nil
}

// Dialer returns d with Control installed, chained after any Control it
// already had. d is modified and returned for convenience.
func Dialer(d *net.Dialer) *net.Dialer {
	prev := d.Control
	d.Control = func(network, address string, c syscall.RawConn) error {
		if prev != nil {
			if err := prev(network, address, c); err != nil {
				return err
			}
		}
		return Control(network, address, c)
	}
	return d
}

// CheckHost resolves host and refuses it if any of its addresses is blocked.
// Any, not all: a name answering with one public and one private address
// would otherwise reach the private one whenever the dialer picks it.
func CheckHost(ctx context.Context, host string) error {
	if !Enabled() || host == "" {
		return nil
	}
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if Blocked(a) {
			return fmt.Errorf("%s: %w", host, ErrBlocked)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		// A name that does not resolve is the check's own failure to report;
		// it reaches nothing, blocked or not.
		return nil
	}
	for _, a := range addrs {
		if Blocked(a) {
			return fmt.Errorf("%s resolves to %s: %w", host, a, ErrBlocked)
		}
	}
	return nil
}

// targetKeys are the monitor-config fields that name where a check connects.
// It matches internal/api's list of target fields plus the docker socket.
var targetKeys = []string{
	"url", "host", "endpoint", "broker", "server", "target",
	"resolver", "proxy_url", "socket", "docker_host",
}

// CheckConfig applies CheckHost to every target field of a monitor config.
func CheckConfig(ctx context.Context, cfg json.RawMessage) error {
	if !Enabled() || len(cfg) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(cfg, &m) != nil {
		return nil
	}
	for _, k := range targetKeys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil || strings.TrimSpace(v) == "" {
			continue
		}
		host, local := targetHost(strings.TrimSpace(v))
		if local {
			return fmt.Errorf("%s %q is a local socket: %w", k, v, ErrBlocked)
		}
		if err := CheckHost(ctx, host); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// targetHost extracts the host from a URL, a host:port, a bare host or a
// gRPC-style target, and reports whether the value names a unix socket.
func targetHost(v string) (host string, local bool) {
	lower := strings.ToLower(v)
	if strings.HasPrefix(lower, "unix:") || strings.HasPrefix(v, "/") {
		return "", true
	}
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil {
			return "", false
		}
		if strings.HasPrefix(strings.ToLower(u.Scheme), "unix") {
			return "", true
		}
		return u.Hostname(), false
	}
	// gRPC's dns:///host:port form.
	v = strings.TrimPrefix(v, "dns:///")
	if h, _, err := net.SplitHostPort(v); err == nil {
		return h, false
	}
	return strings.Trim(v, "[]"), false
}
