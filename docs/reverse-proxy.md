# Reverse proxy

Sync speaks plain HTTP and knows nothing about TLS. Put it behind a reverse
proxy that terminates HTTPS; the two settings that matter are `APP_URL` and
`APP_TRUST_PROXY`.

## 1. What the proxy must do

| Requirement | Why |
|---|---|
| Terminate TLS and pass the original scheme/host in `X-Forwarded-Proto` / `X-Forwarded-Host` | `APP_URL` is what Sync *believes*; the headers are what it *logs* and uses for `c.ClientIP()` |
| `APP_TRUST_PROXY=true` in Sync **and** the forwarded headers enabled in the proxy | otherwise the login throttling counts the proxy address and one operator can lock out everybody |
| Forward cookies unchanged | the session is a signed cookie; rewriting or stripping it breaks the login |
| Do not buffer or delay `/api/*` | uploads/downloads run server side, but the request bodies (job payloads, channel configs) are small and the answers must not be delayed |
| Keep the URL at the root of a host | the SPA uses history mode with `base: '/'`; a sub-path deployment needs a Vite `base` change plus a rewrite (`index.html` already uses relative asset paths) |
| Allow a large `client_max_body_size` for `/api/**` if you post large configs | notification templates and job payloads are small, but a strict 1 MB default can bite when scripting the API |

`APP_URL` must match the public address exactly (scheme + host + optional port):

* OAuth redirect URIs are derived from it — a mismatch produces
  `redirect_uri_mismatch` / `AADSTS50011`;
* the session cookie gets `Secure` only when `APP_URL` starts with `https://`;
* notification links (`data.link`) are absolute URLs built from it.

## 2. Health checking

`GET /api/health` is public, cheap and answers `503` when the database ping
fails, so it is the right probe for the proxy and the orchestrator (the image
ships the same check as `HEALTHCHECK`). It is excluded from the access log.

```yaml
healthcheck:
  test: ["CMD", "wget", "-q", "-O-", "http://127.0.0.1:3000/api/health"]
  interval: 30s
  timeout: 5s
  retries: 3
```

## 3. nginx

```nginx
server {
    listen 443 ssl http2;
    server_name sync.example.com;

    ssl_certificate     /etc/letsencrypt/live/sync.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/sync.example.com/privkey.pem;

    location / {
        proxy_pass         http://127.0.0.1:3000;
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_set_header   X-Forwarded-Host  $host;
        proxy_read_timeout 120s;
    }
}

server {
    listen 80;
    server_name sync.example.com;
    return 301 https://$host$request_uri;
}
```

With that configuration set `APP_URL=https://sync.example.com` and
`APP_TRUST_PROXY=true`.

## 4. Caddy

```caddyfile
sync.example.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:3000
}
```

Caddy sets `X-Forwarded-Proto`/`-Host`/`-For` itself, so the same two Sync
settings apply.

## 5. Traefik (labels for the compose file)

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.http.routers.sync.rule=Host(`sync.example.com`)"
  - "traefik.http.routers.sync.entrypoints=websecure"
  - "traefik.http.routers.sync.tls.certresolver=letsencrypt"
  - "traefik.http.services.sync.loadbalancer.server.port=3000"
```

## 6. Headers Sync already sends

Every answer carries (`internal/handlers/middleware.go`):

```text
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
Referrer-Policy: same-origin
Content-Security-Policy: default-src 'self'; base-uri 'self'; frame-ancestors 'none';
  img-src 'self' data: https:; style-src 'self' 'unsafe-inline';
  script-src 'self' https://www.google.com https://www.gstatic.com;
  connect-src 'self'; frame-src https://www.google.com https://www.gstatic.com
```

Consequences for the proxy and for anything you add in front of it:

* the SPA may only talk to its own origin (`connect-src 'self'`), which is why
development uses the Vite proxy instead of CORS;
* `www.google.com`/`www.gstatic.com` are allowed because reCAPTCHA is optional
  but must work when enabled;
* do **not** add an HSTS header blindly: if you serve Sync over plain HTTP on an
  internal network, HSTS would lock browsers out;
* a proxy that injects a script (analytics, consent banners) needs the CSP
  adjusted — Sync itself never loads a third-party script.

## 7. Behind an authenticating proxy

If an upstream proxy already authenticates users (oauth2-proxy, Authelia, an
identity-aware proxy), configure `AUTH_METHOD=none` and let the proxy do the
work. Keep the proxy's own session cookie and make sure it does not strip the
`/api/*` paths, otherwise the SPA loads without data.

## 8. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| login loops back to `/login` | the cookie is not stored: HTTPS in the browser but `APP_URL` in `http://`, or a proxy that drops `Set-Cookie` | make `APP_URL` match the public scheme |
| `redirect_uri_mismatch` after a proxy change | the provider console holds the old URL | register `APP_URL/api/oauth/<provider>/callback` again |
| every log line shows the proxy IP | `APP_TRUST_PROXY=false` | set it to `true` **and** forward the headers |
| one operator blocks everybody's login | `APP_TRUST_PROXY=false` + throttling | fix the header trust, or raise the limits in `internal/services/auth.go` |
| `502` from the proxy | Sync is not listening on the expected port (see `APP_PORT`) | check `docker compose logs sync` and the published port |
| a deep link answers the API 404 | the proxy rewrote `/jobs/12` to `/api/…` | keep the path untouched; only `/api/**` is an API |
| sub-path deployment shows a blank page | Vite `base` is `/` | build with a matching `base` and rewrite the locations, or serve Sync on a dedicated host (recommended) |
