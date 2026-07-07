#!/usr/bin/env bash
# Build credential-gateway and install/upgrade it on a remote systemd host over
# ssh. The target needs no Go toolchain — only systemd and ssh+sudo. Idempotent:
# re-run to upgrade the binary. The remote config file is never overwritten.
#
#   scripts/deploy.sh <ssh-host>        # e.g. scripts/deploy.sh e6420
#   CG_DEPLOY_HOST=e6420 scripts/deploy.sh
#
# The built version is whatever local HEAD points at, so the script refuses to
# deploy when HEAD is behind its upstream (avoids silently shipping a stale
# release). Set CG_DEPLOY_ALLOW_STALE=1 to override (rollbacks, deliberate
# old-commit deploys).
set -euo pipefail

HOST="${1:-${CG_DEPLOY_HOST:-}}"
if [[ -z "${HOST}" ]]; then
	echo "usage: $(basename "$0") <ssh-host>   (or set CG_DEPLOY_HOST)" >&2
	exit 2
fi

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Staleness gate: refuse to silently deploy a HEAD that's behind upstream.
# Behind => fail loud; ahead/diverged/detached/offline => warn and continue.
if [[ -z "${CG_DEPLOY_ALLOW_STALE:-}" ]]; then
	if git fetch --tags --quiet origin 2>/dev/null; then
		UPSTREAM="$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null || true)"
		if [[ -n "${UPSTREAM}" ]]; then
			HEAD_REV="$(git rev-parse HEAD)"
			UP_REV="$(git rev-parse "${UPSTREAM}")"
			if [[ "${HEAD_REV}" != "${UP_REV}" ]]; then
				if git merge-base --is-ancestor "${HEAD_REV}" "${UP_REV}"; then
					echo "ERROR: local HEAD ($(git describe --tags --always "${HEAD_REV}")) is behind ${UPSTREAM} ($(git describe --tags --always "${UP_REV}"))." >&2
					echo "       Run 'git pull --ff-only' to deploy the latest, or set CG_DEPLOY_ALLOW_STALE=1 to force-deploy the current HEAD." >&2
					exit 1
				else
					echo ">> note: local HEAD differs from ${UPSTREAM} (ahead/diverged); deploying local HEAD." >&2
				fi
			fi
		else
			echo ">> note: no upstream configured (detached HEAD?); skipping staleness check, deploying local HEAD." >&2
		fi
	else
		echo ">> note: 'git fetch' failed (offline?); skipping staleness check, deploying local HEAD." >&2
	fi
fi

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

echo ">> building credential-gateway ${VERSION} (linux/amd64, static)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
	-trimpath -ldflags "-s -w" \
	-o "${tmp}/credential-gateway" .

echo ">> uploading to ${HOST}"
scp -q \
	"${tmp}/credential-gateway" \
	deploy/credential-gateway.service \
	scripts/remote-install.sh \
	"${HOST}:/tmp/"

echo ">> installing on ${HOST} (sudo)"
ssh "${HOST}" "
	set -e
	chmod +x /tmp/remote-install.sh
	sudo /tmp/remote-install.sh \
		/tmp/credential-gateway /tmp/credential-gateway.service
	rm -f /tmp/credential-gateway /tmp/credential-gateway.service /tmp/remote-install.sh
"

echo ">> done. Config on ${HOST}: /etc/credential-gateway/config.yaml"
echo "   logs:   ssh ${HOST} journalctl -u credential-gateway -f"
echo "   status: ssh ${HOST} systemctl status credential-gateway"
