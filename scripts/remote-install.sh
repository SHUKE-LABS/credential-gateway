#!/usr/bin/env bash
# Installs (or upgrades) credential-gateway on the local machine. Runs as root
# on the target — invoked by scripts/deploy.sh over ssh+sudo. Takes the uploaded
# binary and unit paths as args; all install destinations are fixed. Never
# overwrites an existing config file.
set -euo pipefail

BIN=credential-gateway
ADMIN_BIN=credential-gateway-admin
ADMIN_USER=cg-admin
USAGE="usage: remote-install.sh <binary> <unit> <admin-binary> <admin-unit>"
SRC_BIN="${1:?${USAGE}}"
SRC_UNIT="${2:?${USAGE}}"
SRC_ADMIN_BIN="${3:?${USAGE}}"
SRC_ADMIN_UNIT="${4:?${USAGE}}"
CONFIG_DIR=/etc/credential-gateway
CONFIG_FILE="${CONFIG_DIR}/config.yaml"

# install(1) is atomic (write-temp + rename) so an in-flight request never sees
# a half-written binary; the service is (re)started after.
install -m 0755 "${SRC_BIN}" "/usr/local/bin/${BIN}"
install -m 0644 "${SRC_UNIT}" "/etc/systemd/system/${BIN}.service"
install -m 0755 "${SRC_ADMIN_BIN}" "/usr/local/bin/${ADMIN_BIN}"
install -m 0644 "${SRC_ADMIN_UNIT}" "/etc/systemd/system/${ADMIN_BIN}.service"
install -d -m 0750 "${CONFIG_DIR}"

seeded=0
if [[ ! -e "${CONFIG_FILE}" ]]; then
	seeded=1
	# Seed an ALL-COMMENTED template: with no active listener, config.Load()
	# fails and the service refuses to start until the operator uncomments a
	# section and fills in real credentials. This is deliberate — the gateway
	# dials upstreams lazily, so a placeholder-cred config would otherwise come
	# up "active" and proxy nothing useful.
	# Keep the field structure in sync with config.example.yaml.
	umask 077
	cat >"${CONFIG_FILE}" <<'YAML'
# credential-gateway config — installed by remote-install.sh
#
# Every listener section below is commented out, so the gateway refuses to
# start until you enable at least one. Uncomment a section, fill in real
# credentials, then:
#   sudo systemctl start credential-gateway
#
# This file must stay 0600 root:root — the gateway rejects looser permissions.

# http:
#   - name: openai
#     listen: "127.0.0.1:8080"
#     upstream: "https://api.openai.com"
#     headers:
#       Authorization: "Bearer sk-REPLACE_ME"

# mysql:
#   - listen: "127.0.0.1:3307"
#     upstream: "real-db-host:3306"
#     user: dbuser
#     password: "REPLACE_ME"
#     database: mydb

# redis:
#   - listen: "127.0.0.1:6380"
#     upstream: "real-redis-host:6379"
#     password: "REPLACE_ME"

# postgres:
#   - listen: "127.0.0.1:5433"
#     upstream: "real-pg-host:5432"
#     user: dbuser
#     password: "REPLACE_ME"
#     database: mydb   # optional; falls through to client's requested database if omitted

# Oracle proxy — EXPERIMENTAL: does not authenticate against real Oracle servers
# (auth token is SHA1(password+salt), not real Oracle O5LGP). Not for production.
# oracle:
#   - listen: "127.0.0.1:1522"
#     upstream: "real-oracle-host:1521"
#     user: appuser
#     password: "REPLACE_ME"
#     service: ORCLPDB1   # Oracle service name sent in the TNS connect descriptor
YAML
	chmod 0600 "${CONFIG_FILE}"
	chown root:root "${CONFIG_FILE}"
	echo ">> created ${CONFIG_FILE} (template — every listener commented out)"
else
	echo ">> kept existing ${CONFIG_FILE}"
fi

# --- admin UI: dedicated non-root user + ACL-scoped file access -------------
# The admin UI runs as cg-admin (stable UID, not DynamicUser: the ACL grant
# below needs it) and reaches exactly config.yaml via a POSIX ACL, leaving the
# file 0600 root:root so the gateway's permission check needs no change.
if ! id -u "${ADMIN_USER}" >/dev/null 2>&1; then
	useradd --system --no-create-home --shell /usr/sbin/nologin "${ADMIN_USER}"
	echo ">> created system user ${ADMIN_USER}"
else
	echo ">> system user ${ADMIN_USER} already exists"
fi

# Directory grant is execute-only (traversal to a known filename), not read (no
# listing); file grant is read+write. setfacl re-applied every run — idempotent.
setfacl -m "u:${ADMIN_USER}:x"  "${CONFIG_DIR}"
setfacl -m "u:${ADMIN_USER}:rw" "${CONFIG_FILE}"

# Verify the grants actually applied. They silently don't on a noacl mount, a
# filesystem without ACL support, or a missing setfacl — leaving cg-admin unable
# to write. Fail loudly rather than tolerate that, consistent with the rest of
# this script.
if ! getfacl -pc "${CONFIG_DIR}"  2>/dev/null | grep -qx "user:${ADMIN_USER}:--x" ||
   ! getfacl -pc "${CONFIG_FILE}" 2>/dev/null | grep -qx "user:${ADMIN_USER}:rw-"; then
	echo "ERROR: setfacl grants for ${ADMIN_USER} did not apply on ${CONFIG_DIR}." >&2
	echo "       ACLs may be unsupported here (noacl mount option, filesystem" >&2
	echo "       without ACL support, or setfacl missing). The admin UI cannot" >&2
	echo "       write config.yaml without them — aborting." >&2
	exit 1
fi
echo ">> verified ${ADMIN_USER} ACL grants on ${CONFIG_FILE}"

systemctl daemon-reload

# The admin UI serves regardless of whether config.yaml is valid (it exists by
# now — seeded above or pre-existing), so start it on both fresh and upgrade
# installs; the operator edits config through it. Tolerate a start failure
# rather than aborting the deploy; status is shown below.
systemctl enable --now "${ADMIN_BIN}.service" >/dev/null 2>&1 || true
systemctl --no-pager --full --lines=0 status "${ADMIN_BIN}.service" || true

if [[ "${seeded}" == "1" ]]; then
	# Fresh seed can't start yet (no listener by design). Enable for boot only;
	# do NOT --now, and do NOT fail the deploy on it.
	systemctl enable "${BIN}.service" >/dev/null 2>&1 || true
	echo ">> ${BIN} enabled but NOT started — config needs real values first:"
	echo "     sudo \$EDITOR ${CONFIG_FILE}   # uncomment a section, fill in credentials"
	echo "     sudo systemctl start ${BIN}"
else
	# Upgrade path: an existing config is assumed valid. `restart` (not
	# `enable --now`) is required so an already-running service actually picks
	# up the freshly-installed binary — `--now` would no-op on a live unit and
	# leave the old binary running. Tolerate a start failure (e.g. an operator
	# who never filled in the seed) rather than aborting the whole deploy; the
	# status output below shows the real state.
	systemctl enable "${BIN}.service" >/dev/null 2>&1 || true
	systemctl restart "${BIN}.service" || true
	sleep 1
	systemctl --no-pager --full --lines=0 status "${BIN}.service" || true
fi
