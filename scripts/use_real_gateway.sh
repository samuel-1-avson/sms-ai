#!/bin/sh
# Switch the local dev stack between the mock gateway and a real provider.
#
#   scripts/use_real_gateway.sh                      switch to mNotify (asks first)
#   scripts/use_real_gateway.sh --provider mtn       switch to a dormant provider: mnotify (default), mtn, hubtel or arkesel
#   scripts/use_real_gateway.sh --yes                switch without asking
#   scripts/use_real_gateway.sh --mock               switch back to the offline mock gateway
#
# mNotify is the chosen provider. Switching to it refuses to run until MNOTIFY_API_KEY and MNOTIFY_USSD_TOKEN are
# set in .env. mNotify documents no sandbox, so it always switches to SMS_ENVIRONMENT=live: every SMS the stack sends
# is real and billed (mNotify's free account starts with 10 SMS credits). MTN (dropped after two OAuth 401s and
# company-only production vetting), Hubtel and Arkesel stay in the repo, dormant: MTN follows MTN_BASE_URL
# (https://sandbox.api.mtn.com selects sandbox, any other host live), Hubtel has no sandbox and switches to live,
# and Arkesel switches to its sandbox.
# Secrets are never printed: only the names of missing settings are.
set -eu
export MSYS_NO_PATHCONV=1
root=$(cd "$(dirname "$0")/.." && pwd)
envfile="$root/.env"
compose="docker compose -f $root/deployments/compose.yaml"

provider=mnotify
assume_yes=""
mock=""
while [ $# -gt 0 ]; do
	case "$1" in
	--yes) assume_yes=1 ;;
	--mock) mock=1 ;;
	--provider)
		[ $# -ge 2 ] || { echo "use_real_gateway: --provider needs a value (mnotify, mtn, hubtel or arkesel)" >&2; exit 2; }
		provider=$2
		shift
		;;
	--provider=*) provider=${1#--provider=} ;;
	*) echo "use_real_gateway: unknown option $1 (see the header of this script)" >&2; exit 2 ;;
	esac
	shift
done
case "$provider" in
mnotify | mtn | hubtel | arkesel) ;;
*) echo "use_real_gateway: unknown provider '$provider' (mnotify, mtn, hubtel or arkesel)" >&2; exit 2 ;;
esac

[ -f "$envfile" ] || { echo "use_real_gateway: $envfile not found (copy .env.example first)" >&2; exit 1; }

get() { grep -E "^$1=" "$envfile" | tail -1 | cut -d= -f2-; }

# set_var KEY VALUE: replace the KEY= line, or append it.
set_var() {
	if grep -qE "^$1=" "$envfile"; then
		sed -i "s#^$1=.*#$1=$2#" "$envfile"
	else
		printf '%s=%s\n' "$1" "$2" >>"$envfile"
	fi
}

restart() {
	$compose up -d --force-recreate api worker 2>&1 | grep -E 'Recreated|Started|rror' || true
	i=0
	until curl -fs http://localhost:8088/readyz >/dev/null 2>&1; do
		i=$((i + 1))
		if [ "$i" -gt 20 ]; then
			echo "The stack did not become ready. Check: $compose logs --tail 30 api" >&2
			exit 1
		fi
		sleep 2
	done
	echo "Stack ready: $(curl -s http://localhost:8088/readyz)"
}

if [ -n "$mock" ]; then
	set_var PRIMARY_SMS_PROVIDER mock
	set_var PRIMARY_USSD_PROVIDER mock
	set_var SMS_ENVIRONMENT sandbox
	echo "Switched .env to the mock gateway (offline, development only)."
	restart
	exit 0
fi

# Variables the chosen provider cannot run without.
case "$provider" in
mnotify) required="MNOTIFY_API_KEY MNOTIFY_USSD_TOKEN" ;;
mtn) required="MTN_CLIENT_ID MTN_CLIENT_SECRET MTN_USSD_TOKEN MTN_DLR_TOKEN" ;;
hubtel) required="HUBTEL_CLIENT_ID HUBTEL_CLIENT_SECRET HUBTEL_USSD_TOKEN HUBTEL_DLR_TOKEN" ;;
arkesel) required="ARKESEL_API_KEY ARKESEL_USSD_TOKEN" ;;
esac
missing=""
for k in $required; do
	case "$(get "$k")" in
	"" | REPLACE_* | CHANGE_ME* | SET_AFTER*) missing="$missing $k" ;;
	esac
done
if [ -n "$missing" ]; then
	echo "Refusing to switch to $provider: not set in .env:$missing" >&2
	echo "Nothing was changed. Fill them in (see .env.example and docs/PROVIDER_VALIDATION.md), then run this again." >&2
	exit 1
fi

# The SMS environment each provider is switched to, and whether it needs the typed confirmation.
live=1
case "$provider" in
mnotify) env_value=live ;;
mtn)
	base=$(get MTN_BASE_URL)
	case "${base:-https://api.mtn.com}" in
	*://sandbox.api.mtn.com | *://sandbox.api.mtn.com/*) env_value=sandbox live="" ;;
	*) env_value=live ;;
	esac
	;;
hubtel) env_value=live ;;
arkesel) env_value=sandbox live="" ;;
esac

if [ -n "$live" ] && [ -z "$assume_yes" ]; then
	echo "SMS_ENVIRONMENT will be live: after this, every SMS the stack sends is REAL and BILLED to your $provider account."
	printf 'Type "live" to continue: '
	read -r answer || answer=""
	[ "$answer" = "live" ] || { echo "Cancelled. Nothing was changed."; exit 1; }
fi

set_var PRIMARY_SMS_PROVIDER "$provider"
set_var PRIMARY_USSD_PROVIDER "$provider"
set_var SMS_ENVIRONMENT "$env_value"
echo "Switched .env to $provider (SMS_ENVIRONMENT=$env_value)."
restart

case "$provider" in
mnotify)
	cat <<EOF

mNotify has no sandbox: SMS_ENVIRONMENT is live, so every SMS from here on is real and billed.
Check the account without sending anything (one read-only call each; the API key is never printed):
  $compose run --rm api /app/admin mnotify-status -check-credentials
  $compose run --rm api /app/admin mnotify-sender-status
SMS_OUTBOUND_SENDER_ID (max 11 characters) must be a sender ID mNotify has approved for your account.
mNotify must reach this service over public HTTPS (a laptop cannot receive callbacks without a tunnel or a
deployed host). It documents no callback authentication, so the callback URL carries MNOTIFY_USSD_TOKEN from your
.env. How a callback URL is registered is not published: ask mNotify (support@mnotify.com, +233 54 150 9394) or
use the dashboard, with this URL and the token copied from .env:
  https://<your-host>/webhooks/ussd/mnotify/session?token=<MNOTIFY_USSD_TOKEN>
Set USSD_SERVICE_CODE to the USSD code mNotify gives you. MNOTIFY_USSD_MODE=auto serves shared and dedicated codes.
mNotify documents no delivery reports, so messages stay 'submitted'. docs/PROVIDER_VALIDATION.md lists what is unconfirmed.
EOF
	;;
mtn)
	cat <<EOF

Next, MTN must reach this service over public HTTPS (a laptop cannot receive callbacks without a tunnel or a
deployed host). MTN authenticates nothing, so each callback URL carries a token from your .env. Set
MTN_CALLBACK_BASE_URL=https://<your-host> in .env, then register the callbacks (URLs are printed with the token redacted):
  $compose run --rm api /app/admin mtn-subscribe -dry-run
  $compose run --rm api /app/admin mtn-subscribe
Record the printed subscription ids as MTN_USSD_SUBSCRIPTION_ID / MTN_SMS_SUBSCRIPTION_ID in .env: MTN cannot list them.
Also set USSD_SERVICE_CODE and SMS_OUTBOUND_SENDER_ID in .env to what MTN assigns you.
Selected products: MTN USSD interface, SMS V2 and OAuth V1. SMS registers notifyUrl per sender address.
MTN_COUNTRY_CODE defaults to GH; verify the x-country-code format with MTN. MTN_SMS_SEND_API_KEY and
MTN_USSD_SEND_BEARER default to false; enable extra credentials only after MTN or sandbox confirmation.
Keep MTN_SMS_LOOKUP_BY_CLIENT_REF=false until sandbox proves lookup by our client reference.
Production access is MTN's decision, not the developer portal's: see docs/PROVIDER_VALIDATION.md before relying on this.
EOF
	;;
hubtel)
	cat <<'EOF'

Next, Hubtel must reach this service over public HTTPS (a laptop cannot receive callbacks without a tunnel or a
deployed host). Register these with Hubtel, using the tokens from your .env:
  USSD Service Interaction URL: https://<your-host>/webhooks/ussd/hubtel/session?token=<HUBTEL_USSD_TOKEN>
  Delivery reports (ask Hubtel):  https://<your-host>/webhooks/sms/hubtel/status?token=<HUBTEL_DLR_TOKEN>
Also set USSD_SERVICE_CODE and SMS_OUTBOUND_SENDER_ID in .env to what Hubtel assigns you.
EOF
	;;
arkesel)
	cat <<'EOF'

Arkesel is in sandbox mode: messages are accepted but neither delivered nor billed. Register the USSD endpoint as
  https://<your-host>/webhooks/ussd/arkesel/session?token=<ARKESEL_USSD_TOKEN>
and set SMS_ENVIRONMENT=live yourself when you want real sends.
EOF
	;;
esac
