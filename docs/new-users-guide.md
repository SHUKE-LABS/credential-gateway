# New User's Guide

This guide gets you from **zero to a working credential-injecting proxy in about
five minutes**. It is a hands-on quick start — it does not replace the
[README](../README.md), which is the full reference for every config field, the
network trust boundary, systemd deployment, and the admin UI.

By the end you will have:

- built the gateway,
- configured one HTTP proxy,
- confirmed the credential is injected **without ever putting the secret in your
  app or your worktree**.

No real credentials or network access are needed for the walkthrough — a tiny
local server plays the upstream.

---

## What the gateway does (30 seconds)

Your app talks to `localhost` instead of the real service. The gateway holds the
secret in one protected config file and injects it before forwarding:

```
your app  ──►  credential-gateway  ──►  upstream service
(no secret)     (adds the secret)        api.openai.com, MySQL, Redis, …
```

The secret lives in **one** file (`~/.config/credential-gateway/config.yaml`,
mode `0600`) instead of a `.env` in every worktree. Rotate it there once and
every project picks it up on its next request.

---

## 1. Prerequisites

- **Go 1.22+** — check with `go version`.
- A terminal.
- *(Later)* the real service and credential you want to stop pasting around.

The gateway itself has no other runtime dependencies.

---

## 2. Build it

```bash
git clone https://github.com/SHUKE-LABS/credential-gateway.git
cd credential-gateway
go build -o credential-gateway .
```

A plain `go build` reports its version as `dev`:

```bash
./credential-gateway -version   # -> dev
```

---

## 3. Start a throwaway upstream (for this walkthrough)

Save this as `echo_upstream.py` **outside** the repository, then run it. It
answers every request with the `Authorization` header it received, which is how
we will see the injected credential.

```python
#!/usr/bin/env python3
"""Fake upstream: echoes back the Authorization header it was sent."""
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({
            "path": self.path,
            "authorization": self.headers.get("Authorization", ""),
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

HTTPServer(("127.0.0.1", 8081), Handler).serve_forever()
```

```bash
python3 echo_upstream.py
```

Leave it running in one terminal. Used directly (no gateway), it shows no auth
header:

```bash
curl http://127.0.0.1:8081/ping
# {"path": "/ping", "authorization": ""}
```

---

## 4. Write your first config

The gateway searches for a config in this order, first match wins:

1. `~/.config/credential-gateway/config.yaml`
2. `/etc/credential-gateway/config.yaml`
3. whatever you pass with `-config /path/to/config.yaml`

For the walkthrough, create it in the default location:

```bash
mkdir -p ~/.config/credential-gateway
cat > ~/.config/credential-gateway/config.yaml <<'EOF'
http:
  - name: echo                      # label used in logs and error messages
    listen: "127.0.0.1:8080"        # your app connects here
    upstream: "http://127.0.0.1:8081"   # the real service (echo here)
    headers:
      Authorization: "Bearer guide-demo-secret"   # injected on every request
EOF
chmod 0600 ~/.config/credential-gateway/config.yaml
```

| Field | Meaning |
|---|---|
| `name` | Optional label; shows up in startup logs and error messages. |
| `listen` | Address the proxy binds. `127.0.0.1:8080` = only this machine can reach it. |
| `upstream` | The real service. Full URL for HTTP; `host:port` for the database proxies. |
| `headers` | Headers the gateway sets on every request. Values here are your secrets. |

> **Keep secrets out of the repo.** `config.yaml` lives in `~/.config`, not your
> project. If you put a config inside a repo, note this project gitignores
> `*.yaml` (except `config.example.yaml`) — but outside the worktree is still the
> right place.

---

## 5. Validate before you start

`-validate` checks the file (and its permissions) and exits without binding a
port — safe to run in CI or while the gateway is already running:

```bash
./credential-gateway -validate
```

Expected output (logs are JSON on stderr):

```json
{"time":"…","level":"INFO","msg":"starting","version":"dev"}
{"time":"…","level":"INFO","msg":"config is valid"}
```

Exit code `0` means the config is well-formed, permission-safe, and parseable.
It is **static** validation only: it does not dial upstreams or test that the
credential authenticates.

---

## 6. Start the gateway

```bash
./credential-gateway
```

You should see one `listening` line per configured proxy:

```json
{"time":"…","level":"INFO","msg":"starting","version":"dev"}
{"time":"…","level":"INFO","msg":"listening","addr":"http:127.0.0.1:8080"}
```

Leave it running. Stop it with `Ctrl-C` (or `SIGTERM`) — it drains in-flight
connections gracefully within 10 seconds.

Watch more detail while you get set up with `-log-level debug`:

```bash
./credential-gateway -log-level debug
```

---

## 7. Use it — and confirm the injection

In a second terminal, make a request through the gateway. The client sends **no**
credential:

```bash
curl http://127.0.0.1:8080/hello
# {"path": "/hello", "authorization": "Bearer guide-demo-secret"}
```

That `"Bearer guide-demo-secret"` came from your config, injected by the gateway
on the way to the upstream. This is the whole idea in one line.

Point your app at the gateway the same way. For an SDK that reads a base URL from
the environment:

```bash
# OpenAI-style client: paths are passed through, the gateway adds the key
export OPENAI_BASE_URL="http://127.0.0.1:8080"
```

If the client *does* send its own `Authorization`, the gateway replaces it with
the configured value — so you can't accidentally send the wrong one.

### Database and cache proxies

The same pattern works for the TCP proxies. The client connects to the gateway
with **any** user/password (they are discarded); the gateway does the real
handshake using the configured credentials.

```bash
mysql  -h 127.0.0.1 -P 3307 -u anything mydb     # no password needed
redis-cli -p 6380                                # no AUTH needed
psql "host=127.0.0.1 port=5433 user=anything dbname=mydb"
```

Add them to the same config file — include only the services you use (see the
[README config section](../README.md#config) for the full schema):

```yaml
mysql:
  - listen: "127.0.0.1:3307"
    upstream: "real-db-host:3306"
    user: dbuser
    password: "…"
    database: mydb

redis:
  - listen: "127.0.0.1:6380"
    upstream: "real-redis-host:6379"
    password: "…"

postgres:
  - listen: "127.0.0.1:5433"
    upstream: "real-pg-host:5432"
    user: dbuser
    password: "…"
    database: mydb   # optional: falls back to the database the client asked for
```

> **MySQL accounts must use `mysql_native_password`.** A stock MySQL 8.0+ account
> defaults to `caching_sha2_password`, which this proxy does not support. Convert
> the account first:
>
> ```sql
> ALTER USER 'dbuser'@'%' IDENTIFIED WITH mysql_native_password BY '<password>';
> ```
>
> Connecting to a `caching_sha2_password` account returns a clean error naming the
> plugin instead of hanging.

---

## 8. Point it at the real service

When you are ready, replace the walkthrough upstream and secret in
`~/.config/credential-gateway/config.yaml` with real values:

```yaml
http:
  - name: openai
    listen: "127.0.0.1:8080"
    upstream: "https://api.openai.com"
    headers:
      Authorization: "Bearer sk-…"
```

Re-validate (`./credential-gateway -validate`), restart the gateway, and stop the
echo server. Nothing else changes — your app still talks to `127.0.0.1:8080`.

---

## 9. Run it always-on (optional)

To keep the gateway running on an always-on host and share it across machines on a
trusted network, use the deploy script — it builds a static Linux binary locally
and installs it as a systemd service over SSH:

```bash
scripts/deploy.sh <ssh-host>
```

On a **fresh** host it installs an all-commented `/etc/credential-gateway/config.yaml`
as a template and **does not start** the service (an empty config can't start).
Fill it in, then start it:

```bash
ssh <host>
sudo $EDITOR /etc/credential-gateway/config.yaml   # uncomment a section, add real credentials
sudo systemctl start credential-gateway
```

There is also a loopback-only web UI for editing that file from a browser over an
SSH tunnel — see the [README deployment](../README.md#deployment-always-on-systemd-host)
and [admin UI](../README.md#admin-ui-editing-config-over-the-web) sections.

> **The network you bind to is the trust boundary.** The proxies do no inbound
> authentication: anyone who can reach a port gets credentials injected for them.
> Loopback (`127.0.0.1`) is the safe default. Bind a private/Tailscale address
> only on a network you trust, and never a public address without an
> authenticating layer in front. Full details in the
> [README network trust boundary](../README.md#network-trust-boundary).

---

## Troubleshooting

Every message below is logged as JSON on stderr. The `err` field holds the text.

| What you see | What it means | Fix |
|---|---|---|
| `no config file found (searched […])` | No config at the default paths. | Create `~/.config/credential-gateway/config.yaml`, or pass `-config /path/to.yaml`. |
| `config file … has unsafe permissions 0644 (must be 0600 or stricter)` | The config (or its directory) is readable by others. | `chmod 0600 ~/.config/credential-gateway/config.yaml` |
| `parse config: EOF` | The file is empty or all-commented (e.g. the deploy template). | Uncomment and fill in at least one service. |
| `config defines no listeners` | Service sections are present but empty. | Add at least one listener. |
| `http[0]: missing required field 'upstream'` | A required key is absent. | Add the field; the `[0]` is the entry's index in that section. |
| `http[1]: duplicate listen address "…" (already used by http[0])` | Two entries share a `listen` address. | Give each listener a unique address. |
| `yaml: unmarshal errors: line N: field X not found in type config.HTTPService` | Unknown/misspelled key (unknown keys are rejected). | Fix the field name or indentation. |
| `invalid log level "loud": accepted values are debug, info, warn, error` | Bad `-log-level`/`CG_LOG_LEVEL` value. | Use one of the accepted values. |
| `failed to start gateway … bind: address already in use` | The port is taken. | Change `listen`, or find the process with `ss -ltnp \| grep 8080`. |
| Client gets `502 Bad Gateway` | The gateway could not reach `upstream`. | Check the URL and that the upstream is reachable from the gateway host; the log has an `http proxy upstream error` line. |
| MySQL auth error naming `caching_sha2_password` | The upstream account uses an unsupported auth plugin. | Convert it to `mysql_native_password` (see above). |
| Admin UI page won't load from another machine | It binds `127.0.0.1:8099` only, by design. | Use an SSH tunnel: `ssh -L 8099:127.0.0.1:8099 <host>`, then open `http://127.0.0.1:8099`. |
| Admin UI edit didn't take effect | Writes are validated but there is **no hot reload**. | `sudo systemctl restart credential-gateway`. |

Still stuck? Re-run with `./credential-gateway -log-level debug`, and see the
[README testing](../README.md#testing) and [architecture](../README.md#architecture)
sections.

---

## Cheat sheet

```bash
# Build
go build -o credential-gateway .

# Check a config without starting anything (exit 0 = good)
./credential-gateway -validate
./credential-gateway -validate -config ~/my.yaml

# Run (default search path, or an explicit file)
./credential-gateway
./credential-gateway -config ~/my.yaml -log-level debug

# Print the version and exit
./credential-gateway -version
```

Default config path `~/.config/credential-gateway/config.yaml` (mode `0600`); logs
are JSON on stderr; `Ctrl-C` shuts the gateway down gracefully.

---

## Next steps

- [README](../README.md) — the full reference: every config field, all five
  proxies, the [network trust boundary](../README.md#network-trust-boundary),
  and [logging](../README.md#logging).
- [Admin UI](../README.md#admin-ui-editing-config-over-the-web) — edit the config
  over the web on a deployment host.
- [Testing](../README.md#testing) — run the test suite with `go test ./...`.
