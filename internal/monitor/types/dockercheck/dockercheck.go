// Package dockercheck implements the Docker container monitor type: it inspects
// a container through the Docker Engine API and reports UP when the container is
// in the required state. The same type serves both the local Unix socket
// (endpoint "unix:///var/run/docker.sock") and a remote, TLS-secured Docker host
// (endpoint "tcp://host:2376" with client certificates). It talks to the Engine
// HTTP API directly over the socket, so it adds no heavyweight client
// dependency. Connection failures, a missing container or a wrong state map to a
// DOWN CheckResult rather than a Go error.
package dockercheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "docker"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// containerState is the subset of the Engine inspect response the check needs.
type containerState struct {
	// status is State.Status: running, exited, paused, restarting, …
	status string
	// health is State.Health.Status: healthy, unhealthy, starting or "" when
	// the image defines no HEALTHCHECK.
	health string
}

// Checker implements registry.Checker for the Docker container family.
type Checker struct {
	// probe inspects the container. nil uses the real Engine-API
	// implementation; tests inject a fake to exercise result mapping without a
	// daemon.
	probe func(ctx context.Context, c Config) (containerState, error)
}

func init() {
	registry.Register(&Checker{})
}

// Type implements registry.Checker.
func (*Checker) Type() string { return TypeName }

// DefaultTimeout implements registry.TimeoutDefaulter.
func (*Checker) DefaultTimeout() time.Duration { return defaultTimeout }

// Validate implements registry.Checker.
func (*Checker) Validate(cfg json.RawMessage) error {
	c, err := parseConfig(cfg)
	if err != nil {
		return err
	}
	return c.validate()
}

// Check implements registry.Checker. Connection failures, a missing container
// and a wrong state are DOWN results; only unusable configuration errors.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	probe := ch.probe
	if probe == nil {
		probe = liveInspect
	}

	start := time.Now()
	st, err := probe(ctx, c)
	latency := time.Since(start)
	if err != nil {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: latency,
			Message: err.Error(),
		}, nil
	}

	if msg := c.assess(st); msg != "" {
		return domain.CheckResult{Status: domain.StatusDown, Latency: latency, Message: msg}, nil
	}
	return domain.CheckResult{Status: domain.StatusUp, Latency: latency, Message: c.okMessage()}, nil
}

// assess applies the required-state assertion and returns a failure message, or
// "" when the container is in the required state.
func (c Config) assess(st containerState) string {
	if st.status != StateRunning {
		return fmt.Sprintf("container %q is %q, want running", c.Container, st.status)
	}
	if c.requiredState() == StateHealthy {
		switch st.health {
		case StateHealthy:
		case "":
			return fmt.Sprintf("container %q defines no health check but required_state is healthy", c.Container)
		default:
			return fmt.Sprintf("container %q health is %q, want healthy", c.Container, st.health)
		}
	}
	return ""
}

// okMessage describes a successful check.
func (c Config) okMessage() string {
	if c.requiredState() == StateHealthy {
		return fmt.Sprintf("container %q running and healthy", c.Container)
	}
	return fmt.Sprintf("container %q running", c.Container)
}

// engineInspect mirrors the fields of GET /containers/{id}/json this check uses.
type engineInspect struct {
	State struct {
		Status string `json:"Status"`
		Health *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

// liveInspect performs the real Engine-API inspect over the unix socket or TCP.
func liveInspect(ctx context.Context, c Config) (containerState, error) {
	tr, err := c.resolveTransport()
	if err != nil {
		return containerState{}, err
	}

	httpTransport := &http.Transport{}
	if tr.unix {
		socket := tr.unixPath
		httpTransport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
	}
	if tr.scheme == "https" {
		tlsCfg, err := c.tlsConfig()
		if err != nil {
			return containerState{}, err
		}
		httpTransport.TLSClientConfig = tlsCfg
	}
	client := &http.Client{
		Transport: httpTransport,
		Timeout:   time.Duration(c.connectTimeout()) * time.Second,
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tr.inspectURL(c.Container), nil)
	if err != nil {
		return containerState{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return containerState{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return containerState{}, fmt.Errorf("no such container: %s", c.Container)
	default:
		return containerState{}, fmt.Errorf("docker engine returned HTTP %d", resp.StatusCode)
	}

	var inspect engineInspect
	if err := json.NewDecoder(resp.Body).Decode(&inspect); err != nil {
		return containerState{}, fmt.Errorf("decode inspect response: %w", err)
	}
	st := containerState{status: inspect.State.Status}
	if inspect.State.Health != nil {
		st.health = inspect.State.Health.Status
	}
	return st, nil
}
