#!/bin/sh
# Create the pilot env file from deployments/env.prod.example with fresh
# secrets: encryption/HMAC keys and tokens from `admin genkey`, plus random
# Postgres and Grafana passwords and the mNotify USSD callback token. DOMAIN, the allowlist, the alert webhook
# and provider credentials are left for you to edit (lines marked EDIT).
#
#   scripts/init_env_prod.sh                  writes deployments/.env.prod
#   scripts/init_env_prod.sh /path/to/file    writes another file (dry runs)
#
# Builds the image first if it is missing. Nothing leaves the machine.
set -eu
export MSYS_NO_PATHCONV=1
root=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$root/deployments/.env.prod}
image=smsai-ghana:${SMSAI_IMAGE_TAG:-pilot}

if [ -e "$out" ]; then
	echo "init_env_prod: $out already exists; not overwriting it" >&2
	exit 1
fi
if ! docker image inspect "$image" >/dev/null 2>&1; then
	echo "init_env_prod: building $image" >&2
	docker build -q -f "$root/deployments/docker/Dockerfile" -t "$image" "$root" >/dev/null
fi

hex() { od -An -tx1 -N"$1" /dev/urandom | tr -d ' \n'; }

umask 077
values=$(mktemp)
trap 'rm -f "$values"' EXIT
docker run --rm --network none "$image" /app/admin genkey | tr -d '\r' >"$values"
{
	echo "POSTGRES_PASSWORD=$(hex 24)"
	echo "GRAFANA_ADMIN_PASSWORD=$(hex 18)"
	echo "MNOTIFY_USSD_TOKEN=$(hex 24)"
} >>"$values"

# Replace KEY=... lines of the template whose KEY has a generated value.
awk -F= 'NR == FNR { v[$1] = substr($0, length($1) + 2); next }
	/^[A-Z0-9_]+=/ && ($1 in v) { print $1 "=" v[$1]; next }
	{ print }' "$values" "$root/deployments/env.prod.example" >"$out"
chmod 600 "$out"

echo "Wrote $out with generated keys, tokens and passwords."
echo "Now edit the lines marked EDIT, and save a copy of ENCRYPTION_KEYS_V1 and HMAC_KEYS_V1 in your password manager."
echo "MNOTIFY_USSD_TOKEN is part of the callback URL you give mNotify (https://<DOMAIN>/webhooks/ussd/mnotify/session?token=...); treat it as a secret."
echo "Also set MNOTIFY_API_KEY (mNotify dashboard > Settings > Developer), SMS_OUTBOUND_SENDER_ID (an approved sender ID) and USSD_SERVICE_CODE."
echo "MTN, Hubtel and Arkesel are dormant; if you ever switch back to MTN, its tokens are not generated here (see the MTN comment block in the file)."
