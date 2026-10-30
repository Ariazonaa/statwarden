# Reverse-proxy configuration

Statwarden serves the API, the embedded SPA and the WebSocket hub on a **single
plain-HTTP port** (`:3001` by default). It does **not** terminate TLS itself —
run it behind a reverse proxy that terminates HTTPS and forwards to the app.

Everything is one origin, so proxying is simple: forward all paths to
`127.0.0.1:3001`. The one thing you must not forget is the **WebSocket upgrade**
on `/ws` (live dashboard, monitor detail and public status-page updates). The
samples below all cover it.

Endpoints of note:

| Path            | Purpose                                        |
| --------------- | ---------------------------------------------- |
| `/`             | Embedded SvelteKit SPA                          |
| `/api/v1/...`   | REST API                                        |
| `/ws`           | WebSocket hub (**needs Upgrade/Connection**)    |
| `/healthz`      | Liveness (process up)                          |
| `/readyz`       | Readiness (process up **and** DB reachable)    |

Set `PUBLIC_BASE_URL` to the public HTTPS URL (e.g. `https://status.example.com`)
so generated links, badges and status-page URLs are correct.

---

## Caddy

Caddy terminates TLS automatically (Let's Encrypt) and proxies WebSockets
transparently — no special block needed.

```caddyfile
status.example.com {
    encode zstd gzip

    # Single upstream handles SPA, REST and /ws. Caddy passes the
    # Upgrade/Connection headers through automatically.
    reverse_proxy 127.0.0.1:3001 {
        health_uri /healthz
        health_interval 30s
        health_timeout 5s
    }
}
```

Run the app with `PUBLIC_BASE_URL=https://status.example.com`.

---

## Nginx

Nginx needs the explicit `Upgrade`/`Connection` dance for `/ws`. The
`map` promotes a normal keep-alive connection to `upgrade` only when the client
actually requests it.

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

upstream statwarden {
    server 127.0.0.1:3001;
    keepalive 32;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name status.example.com;

    ssl_certificate     /etc/letsencrypt/live/status.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/status.example.com/privkey.pem;

    # WebSocket hub — must come before the catch-all location.
    location /ws {
        proxy_pass         http://statwarden;
        proxy_http_version 1.1;
        proxy_set_header   Upgrade    $http_upgrade;
        proxy_set_header   Connection $connection_upgrade;
        proxy_set_header   Host       $host;
        proxy_set_header   X-Real-IP  $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        # Long-lived connections: don't cut idle sockets early.
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }

    # SPA + REST API.
    location / {
        proxy_pass       http://statwarden;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}

# Redirect plain HTTP to HTTPS.
server {
    listen 80;
    listen [::]:80;
    server_name status.example.com;
    return 301 https://$host$request_uri;
}
```

---

## Traefik

### Static/dynamic file provider (v3)

Point Traefik at the app as a single service; the router matches the host and
Traefik forwards WebSockets natively (no extra middleware required).

```yaml
# traefik-dynamic.yml — loaded via a file provider.
http:
  routers:
    statwarden:
      rule: "Host(`status.example.com`)"
      entryPoints:
        - websecure
      service: statwarden
      tls:
        certResolver: letsencrypt

  services:
    statwarden:
      loadBalancer:
        servers:
          - url: "http://127.0.0.1:3001"
        # Traefik follows the app's own liveness probe.
        healthCheck:
          path: /healthz
          interval: "30s"
          timeout: "5s"
```

### Docker labels (compose)

If you run Statwarden from `docker-compose.yml` behind Traefik, add these
labels to the `app` service. Traefik proxies `/ws` transparently.

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.http.routers.statwarden.rule=Host(`status.example.com`)"
  - "traefik.http.routers.statwarden.entrypoints=websecure"
  - "traefik.http.routers.statwarden.tls.certresolver=letsencrypt"
  - "traefik.http.services.statwarden.loadbalancer.server.port=3001"
  - "traefik.http.services.statwarden.loadbalancer.healthcheck.path=/healthz"
```

---

## Notes

- **Trusted proxies / client IP.** See the dedicated section below — this needs
  one configuration step, and getting it wrong costs you brute-force protection.
- **WebSocket idle timeouts.** The hub sends periodic pings, but proxies with
  aggressive idle timeouts can still drop connections. The Nginx sample raises
  `proxy_read_timeout`; Caddy and Traefik default to long-lived streams.
- **Compression.** The app already gzip-compresses responses; enabling
  compression at the proxy too is harmless but optional.

---

## Trusted proxies and the client IP

Statwarden attributes a request to a client IP and uses it for login rate limiting,
the public-endpoint rate limiter, account lockout and the audit log. Getting
that address right therefore matters for security, not just for pretty logs.

`X-Forwarded-For` is a request header, so **any client can send one**. Statwarden
only believes it when the connection's actual peer address is inside
`TRUSTED_PROXIES`; otherwise the peer address is used verbatim.

### `TRUSTED_PROXIES`

Comma-separated list of CIDR blocks. **Empty by default**, which is the safe
setting: with no entry, `X-Forwarded-For` is ignored entirely and an
unconfigured deployment cannot be spoofed. Invalid CIDRs are rejected at
startup with the rest of the configuration errors.

```bash
# Nginx/Caddy on the same host
TRUSTED_PROXIES=127.0.0.1/32,::1/128

# Traefik in the default compose network
TRUSTED_PROXIES=172.16.0.0/12

# A whole private range plus an IPv6 proxy
TRUSTED_PROXIES=10.0.0.0/8,2001:db8::/32
```

Bare addresses are not accepted — write `10.0.0.1/32`, not `10.0.0.1`.

### How the chain is resolved

Each proxy *appends* the peer it saw, so the header reads left to right as
"oldest claim first". The leftmost entry is whatever the original client wrote
and is worthless. Statwarden therefore walks the chain **from the right** and takes
the first address that is not itself a trusted proxy:

```
X-Forwarded-For: 1.2.3.4, 198.51.100.23, 10.0.0.9
                 └ forged  └ real client   └ edge proxy
RemoteAddr: 10.0.0.1  (inner proxy, trusted)
→ client IP = 198.51.100.23
```

With `TRUSTED_PROXIES` empty, the same request resolves to `10.0.0.1` — the
peer, and the only value nobody can forge.

### Rules of thumb

- **List every hop.** If two proxies sit in front of Statwarden, both their address
  ranges belong in `TRUSTED_PROXIES`, or the chain stops at the inner one.
- **Never list a range that isn't a proxy you control.** Trusting `0.0.0.0/0`
  hands every client a self-declared IP: fresh rate-limit and lockout buckets
  on demand, and a brute-force attack that never trips a throttle.
- **Bind Statwarden so the proxy is the only reachable path.** Keep it on
  `127.0.0.1` or an internal Docker network. If clients can also reach it
  directly, they connect from outside the trusted range and are handled safely,
  but they bypass anything the proxy enforces (TLS, WAF, its own rate limits).
- **Leave it empty if Statwarden is directly internet-facing.** That is the correct
  configuration, not a missing one.

### Verifying it works

Log in once and check the audit log (Settings → Audit log, or
`GET /api/v1/audit-log`). The recorded IP should be your real client address.
If it shows your proxy's address instead, `TRUSTED_PROXIES` is missing that
proxy's range. If it shows something you can change by sending your own
`X-Forwarded-For` header, the trusted list is too broad — narrow it.
