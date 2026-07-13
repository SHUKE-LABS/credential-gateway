# credential-gateway

**A local credential injection proxy for development.** credential-gateway sits between your app and upstream services, holding all credentials in a single root-owned config file outside every worktree. Your app connects to localhost with no credentials; the gateway injects them before forwarding.

```
app / agent
  ├─ HTTP       → localhost:8080/openai/…   → api.openai.com      (Authorization header injected)
  ├─ MySQL      → localhost:3307 (no passwd) → real MySQL         (credentials injected at handshake — upstream account must use mysql_native_password)
  ├─ Redis      → localhost:6380 (no auth)   → real Redis         (AUTH command injected)
  ├─ PostgreSQL → localhost:5433 (no passwd) → real PostgreSQL    (MD5 / SCRAM-SHA-256 injected)
  └─ Oracle     → localhost:1522 (no passwd) → real Oracle DB     (TNS/TTC wire only — EXPERIMENTAL, does not authenticate to real Oracle)
```

It solves three problems that `.env` files and secrets managers don't:

- **Credentials leak into worktrees** — `.env` files get committed, shared, or left behind in old branches
- **Per-project setup overhead** — every new worktree or teammate needs the same credentials wired up again
- **Rotating a key means touching every project** — rotate once in `config.yaml`, nothing else changes

One config file. One process. All projects on the machine share it.

## Prerequisites

- Go 1.22+

No other runtime dependencies. The binary uses only the standard library plus `gopkg.in/yaml.v3` for config parsing.

## Build

```bash
go build -o credential-gateway .
```

## Setup

```bash
mkdir -p ~/.config/credential-gateway
cp config.example.yaml ~/.config/credential-gateway/config.yaml
$EDITOR ~/.config/credential-gateway/config.yaml   # fill in real credentials
chmod 0600 ~/.config/credential-gateway/config.yaml
./credential-gateway
```

The gateway refuses to start if the config file is group- or world-readable. This is enforced at startup, not just advisory.

## Config

Config is YAML. All five proxy types are optional — include only the services you need. Multiple entries per section are supported.

```yaml
# HTTP reverse proxy — injects arbitrary headers
http:
  - name: openai
    listen: "127.0.0.1:8080"
    upstream: "https://api.openai.com"
    headers:
      Authorization: "Bearer sk-…"

# MySQL proxy — injects user/password/database at handshake
mysql:
  - listen: "127.0.0.1:3307"
    upstream: "real-db-host:3306"
    user: dbuser
    password: "…"
    database: mydb

# Redis proxy — sends AUTH before piping client traffic
redis:
  - listen: "127.0.0.1:6380"
    upstream: "real-redis-host:6379"
    password: "…"

# PostgreSQL proxy — supports MD5 and SCRAM-SHA-256 auth
postgres:
  - listen: "127.0.0.1:5433"
    upstream: "real-pg-host:5432"
    user: dbuser
    password: "…"
    database: mydb   # optional; falls through to client's requested database if omitted

# Oracle proxy — EXPERIMENTAL. Does not authenticate against real Oracle servers:
# the auth token is SHA1(password+salt), not real Oracle O5LGP, so a real listener
# rejects it. Only the TNS/TTC wire flow works. Not for production use.
oracle:
  - listen: "127.0.0.1:1522"
    upstream: "real-oracle-host:1521"
    user: appuser
    password: "…"
    service: ORCLPDB1   # Oracle service name in the TNS connect descriptor
```

**Config search order** (first found wins):

1. `~/.config/credential-gateway/config.yaml`
2. `/etc/credential-gateway/config.yaml`
3. Custom path: `credential-gateway -config /path/to/config.yaml`

## Running

```bash
credential-gateway                              # use default config search path
credential-gateway -config ~/my-config.yaml    # explicit path
```

Logs are written to stderr in JSON format (`log/slog`). Shutdown is graceful: send `SIGINT` or `SIGTERM` and all listeners drain within 10 seconds.

## Deployment (always-on systemd host)

To run the gateway as a managed service on a remote host, use the deploy script. It builds a static `linux/amd64` binary locally, ships it over ssh, and installs it via systemd — the target needs no Go toolchain, only `systemd` and `ssh`+`sudo`.

```bash
scripts/deploy.sh <ssh-host>        # e.g. scripts/deploy.sh e6420
CG_DEPLOY_HOST=e6420 scripts/deploy.sh
```

- **Idempotent** — re-run to upgrade the binary. An existing `/etc/credential-gateway/config.yaml` is never overwritten.
- **Staleness gate** — refuses to deploy when local `HEAD` is behind its upstream (so you don't silently ship a stale build). Override with `CG_DEPLOY_ALLOW_STALE=1` for rollbacks or deliberate old-commit deploys.

On a **fresh** install the script seeds `/etc/credential-gateway/config.yaml` (0600 root:root) as an all-commented template and enables the unit for boot **without starting it** — with no listener configured the gateway would refuse to start anyway. Fill it in, then start:

```bash
ssh <host>
sudo $EDITOR /etc/credential-gateway/config.yaml   # uncomment a section, add real credentials
sudo systemctl start credential-gateway
systemctl status credential-gateway                 # active/running
journalctl -u credential-gateway -f
```

The service runs as **root** (not `DynamicUser`) because it reads the `0600 root:root` config directly, and passes `-config` explicitly rather than relying on the `$HOME` search path. It keeps the standard systemd hardening otherwise (`NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, restricted address families, etc.); see `deploy/credential-gateway.service`.

## Admin UI (editing config over the web)

On a systemd host, `credential-gateway-admin` is a second, separate service that serves a minimal web page for viewing and editing `/etc/credential-gateway/config.yaml` — an alternative to `ssh` + `$EDITOR`. `scripts/deploy.sh` installs and starts it alongside the gateway.

It is deliberately narrow, because anything that can write that file can read and write every proxied credential:

- **Separate, unprivileged process.** Runs as a dedicated non-root user `cg-admin` (not the gateway's root), reaching only `config.yaml` via a POSIX ACL. The file stays `0600 root:root`; the gateway is unchanged.
- **Loopback only.** Binds `127.0.0.1:8099` and nothing else — reach it through an SSH tunnel:

  ```bash
  ssh -L 8099:127.0.0.1:8099 <host>     # e.g. ssh -L 8099:127.0.0.1:8099 e6420
  # then open http://127.0.0.1:8099 in your browser
  ```

- **Restart required — no hot-reload.** Saving validates and rewrites the file, but **does not** affect the running gateway. Apply changes yourself:

  ```bash
  sudo systemctl restart credential-gateway
  ```

  The UI states this on every page. There is no button that restarts, stops, or reloads the gateway — restart stays in your own SSH+sudo session.

- **Validated writes.** A submitted config is run through the same validation the gateway performs at startup; an invalid config is rejected with the gateway's own error text and the live file is left untouched.

## Architecture

```
main.go
  └─ Gateway
       ├─ []HTTPListener       (net/http/httputil.ReverseProxy, header injection via Director)
       ├─ []MySQLListener      (raw TCP, credentials injected at MySQL native auth handshake)
       ├─ []RedisListener      (raw TCP, AUTH command prepended before client traffic)
       ├─ []PostgreSQLListener (raw TCP, MD5 password or SCRAM-SHA-256 exchange handled)
       └─ []OracleListener     (raw TCP, TNS CONNECT + TTC O3LOG/O3AUTH exchange)
```

Each listener implements `Start() / Stop()`. `Gateway.Start()` launches all of them concurrently; `Gateway.Stop(ctx)` shuts them down in parallel with the provided deadline.

**Protocol depth per proxy:**

| Proxy | What the gateway handles |
|---|---|
| HTTP | Header injection via `Director`; streaming and chunked transfer preserved |
| MySQL | Full native auth handshake; client sends no password. **Upstream accounts must authenticate with `mysql_native_password`** (`ALTER USER <user> IDENTIFIED WITH mysql_native_password BY '<pw>'`). Against a `caching_sha2_password` account (the MySQL 8.0+ default) the server requests an auth-switch the proxy can't satisfy; it returns a clean ERR naming the plugin rather than dropping the connection. `caching_sha2_password` is not supported. |
| Redis | Pre-pipes `AUTH <password>` before forwarding client commands |
| PostgreSQL | SSLRequest negotiation (rejects SSL), MD5 password response, full SCRAM-SHA-256 (PBKDF2 + RFC 5802 proof) |
| Oracle | **EXPERIMENTAL** — TNS CONNECT/ACCEPT, NS negotiation, TTC O3LOG/O3AUTH wire flow. The SHA1-derived auth token is **not** real Oracle O5LGP, so this does **not** authenticate against real Oracle servers |

**Security notes:**

- Config file permissions are validated at startup (`0600` required; group- or world-readable rejected)
- Credential values are never logged — the HTTP Director explicitly avoids logging injected headers
- Credentials live only in the protected config file, never in environment variables or worktree files

## Testing

```bash
go test ./...
```

Tests cover HTTP header injection, MySQL handshake, Redis AUTH injection, PostgreSQL MD5/SCRAM-SHA-256, and the Oracle TNS/TTC wire plumbing (the Oracle proxy is experimental — the tests exercise the wire flow and username injection, not real-Oracle authentication).

## Releases

Every push to `main` runs `.github/workflows/release.yml`, which tags the new `HEAD`, pushes the tag, regenerates `CHANGELOG.md`, and commits the changelog back as `github-actions[bot]` with `[skip ci]` (so it does not re-trigger CI or itself). The git tag **is** the version — nothing is baked into the binary.

Tagging is driven by the `HEAD` commit subject:

- a `feat:` commit bumps the **minor** version and zeroes the patch (`v0.1.0` over `v0.0.1`);
- any other conventional-commit type bumps the **patch** (`v0.0.1` from a tag-less history).

The changelog buckets each commit under `### Features` / `### Fixes` / `### Refactors` / `### Performance` / `### Docs` / `### Other Changes` by its type. **Use conventional-commit prefixes** (`feat:`, `fix:`, `refactor:`, `perf:`, `docs:`, …) so the bump and bucketing classify the commit correctly. Commits with `[skip ci]` in their subject are omitted from the changelog.

Run the tooling locally (it is pure git, no Go involvement):

```bash
bash scripts/generate-changelog -   # print the changelog to stdout
bash scripts/generate-changelog     # write CHANGELOG.md in place
```

## Project structure

```
credential-gateway/
├── main.go                        # gateway entry point, signal handling, graceful shutdown
├── config.example.yaml            # annotated example config (no real credentials)
├── go.mod / go.sum
├── cmd/
│   └── credential-gateway-admin/
│       └── main.go                # admin UI entry point (loopback config editor)
└── internal/
    ├── admin/
    │   ├── server.go             # admin UI HTTP server (validate + in-place write)
    │   └── server_test.go
    ├── config/
    │   ├── config.go              # YAML loading, Parse (shared validation), permission check
    │   └── config_test.go
    └── gateway/
        ├── gateway.go             # orchestrator, Start/Stop lifecycle
        ├── http.go                # HTTP reverse proxy
        ├── http_test.go
        ├── mysql.go               # MySQL TCP proxy
        ├── mysql_test.go
        ├── redis.go               # Redis TCP proxy + AUTH injection
        ├── redis_test.go
        ├── postgres.go            # PostgreSQL proxy (MD5 + SCRAM-SHA-256)
        ├── postgres_test.go
        ├── oracle.go              # Oracle proxy (TNS wire protocol)
        ├── oracle_test.go
        └── pipe.go                # bidirectional TCP pipe shared by all TCP proxies
```
