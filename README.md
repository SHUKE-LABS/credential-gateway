# credential-gateway

**A credential injection proxy for development.** credential-gateway sits between your app and upstream services, holding all credentials in a single root-owned config file outside every worktree. Your app connects to the gateway with no credentials; the gateway injects them before forwarding. The examples below listen on `localhost` — the simplest case — but each listener binds whatever `listen` address you configure (see [Network trust boundary](#network-trust-boundary)).

```
app / agent
  ├─ HTTP       → localhost:8080/openai/…   → api.openai.com      (Authorization header injected)
  ├─ MySQL      → localhost:3307 (no passwd) → real MySQL         (credentials injected at handshake — upstream account must use mysql_native_password)
  ├─ Redis      → localhost:6380 (no auth)   → real Redis         (AUTH command injected)
  ├─ PostgreSQL → localhost:5433 (no passwd) → real PostgreSQL    (MD5 / SCRAM-SHA-256 injected)
  └─ Oracle     → localhost:1522 (no passwd) → real Oracle DB     (TNS/TTC wire only — EXPERIMENTAL, does not authenticate to real Oracle)
```

`localhost` is the default shown here; `listen` accepts any address — see [Network trust boundary](#network-trust-boundary) for binding beyond loopback.

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

To check a config without starting the service, use `-validate`:

```bash
credential-gateway -validate                     # validate default config
credential-gateway -validate -config ~/my.yaml   # validate an explicit path
```

It exits `0` if the config is well-formed, correctly-permissioned, and safe to parse, or `1` with the error on stderr. It binds no listener port, so it is safe to run in CI, a pre-commit hook, or while the service is already running. Note this is static validation only — it does not dial upstreams or verify that credentials authenticate.

To print the build version and exit, use `-version`:

```bash
credential-gateway -version
```

It prints the version to stdout and exits `0` without loading config or binding a port. A binary built by `scripts/deploy.sh` reports its `git describe` version; a plain `go build` reports `dev`. The version is also logged once at startup.

### Logging

Logs are JSON on stderr at `info` level by default. Raise or lower the verbosity with the `-log-level` flag or the `CG_LOG_LEVEL` environment variable:

```bash
credential-gateway -log-level debug            # verbose, for field diagnosis
CG_LOG_LEVEL=warn credential-gateway           # quiet, for production
credential-gateway -log-level info -log-source # add source file:line to each line
```

Accepted levels are `debug`, `info`, `warn`, and `error` (case-insensitive). The `-log-level` flag overrides `CG_LOG_LEVEL` when both are set. An invalid value exits `1` with a message naming the accepted values. The optional `-log-source` flag adds the emitting `source` file:line to every log line.

At `debug`, each TCP proxy (mysql, redis, postgres, oracle) traces the connection lifecycle: an accept line with the client `RemoteAddr` and, on disconnect, a close line with the connection duration and bytes transferred each way. Both lines share a connection `id` for correlation. Credential values never appear in these lines.

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
- **Validation-gated restarts** — the unit's `ExecStartPre` runs `credential-gateway -validate` before every start/restart, so a bad config (deploy *or* a manual `sudo systemctl restart` after a hand-edit) fails with a clean `journalctl` error and never binds a port. On an **upgrade** deploy, a config that can't start now fails the deploy with a non-zero exit — the `systemctl status` diagnostic is still printed so you see exactly why. (A never-configured host is the same case: an all-commented config can't start, so re-deploying one without first filling it in fails the deploy.)

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

## Network trust boundary

Each proxy listener binds whatever `listen` address you put in the config — `net.Listen("tcp", listen)`. There is no loopback enforcement: `127.0.0.1:8080` and `100.111.44.50:8080` (a Tailscale IP) both bind fine. Config validation only checks that required fields are present and that no two listeners share an address.

The proxies perform **no inbound authentication**: any client that can reach the port gets credentials injected on its behalf. So **the network you bind to is the trust boundary** — whoever can reach a proxy port can use the upstream credentials.

- **Loopback (`127.0.0.1`)** — the default in every example. Only processes on the same host can connect. Simplest and safest.
- **Private network (Tailscale IP, VPN, trusted LAN)** — supported and a legitimate deployment. Set `listen` to the interface address (e.g. `listen: "100.111.44.50:8080"`) so other machines on that trusted network can use the gateway. You are accepting that anyone who can reach the port gets credential injection, so bind only to a network you trust (e.g. a private tailnet).
- **Public internet** — never bind a proxy to a public address without an authenticating layer (reverse proxy, mTLS, firewall) in front. There is no inbound auth to stop an attacker who reaches the port.

The admin UI is the one exception: it is always loopback-only (`127.0.0.1:8099`, hardcoded) regardless of config — see [Admin UI](#admin-ui-editing-config-over-the-web).

## Testing

```bash
go test ./...
```

Tests cover HTTP header injection, MySQL handshake, Redis AUTH injection, PostgreSQL MD5/SCRAM-SHA-256, and the Oracle TNS/TTC wire plumbing (the Oracle proxy is experimental — the tests exercise the wire flow and username injection, not real-Oracle authentication).

## Releases

Every push to `main` runs `.github/workflows/release.yml`, which tags the new `HEAD`, pushes the tag, regenerates `CHANGELOG.md`, and commits the changelog back as `github-actions[bot]` with `[skip ci]` (so it does not re-trigger CI or itself). The git tag **is** the version: `scripts/deploy.sh` bakes the `git describe --tags --always --dirty` version into the binary via `-ldflags -X main.version=…`, which `credential-gateway -version` then reports. A plain `go build` (including CI) leaves the version as `dev`.

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
