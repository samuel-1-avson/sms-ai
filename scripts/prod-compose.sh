#!/bin/sh
# docker compose for the pilot stack (deployments/compose.prod.yaml) with the
# operator's env file, from any working directory:
#   scripts/prod-compose.sh up -d --build
#   scripts/prod-compose.sh exec api /app/admin flag ai_enabled false
# SMSAI_ENV_FILE selects another env file (default deployments/.env.prod).
set -eu
# Git Bash on Windows: pass container paths such as /app/admin unchanged.
export MSYS_NO_PATHCONV=1

here=$(cd "$(dirname "$0")" && (pwd -W 2>/dev/null || pwd))
root=$(cd "$here/.." && (pwd -W 2>/dev/null || pwd))
env_file=${SMSAI_ENV_FILE:-$root/deployments/.env.prod}
case $env_file in
/* | [A-Za-z]:*) ;;
*) env_file="$(pwd -W 2>/dev/null || pwd)/$env_file" ;;
esac
if [ ! -f "$env_file" ]; then
	echo "prod-compose: $env_file not found; create it with scripts/init_env_prod.sh (docs/DEPLOYMENT.md)" >&2
	exit 1
fi
# compose.prod.yaml's env_file entries read the same file.
export SMSAI_ENV_FILE="$env_file"
exec docker compose -f "$root/deployments/compose.prod.yaml" --env-file "$env_file" "$@"
