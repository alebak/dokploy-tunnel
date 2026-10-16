# shellcheck shell=bash
#
# Shared helpers for the end-to-end scripts in test/e2e. Source it; do not
# run it. The scripts change the machine they run on (they install Dokploy,
# rewrite /etc/hosts through sudo and create containers), so they are meant
# for a disposable GitHub runner or VM only. See CONTRIBUTING.md.

# E2E_DIR holds the state the scripts hand to each other: the cookie jar,
# the API key, Dokploy IDs, CLI outputs and logs.
E2E_DIR="${E2E_DIR:-${RUNNER_TEMP:?set E2E_DIR or RUNNER_TEMP}/doktunnel-e2e}"
mkdir -p "$E2E_DIR"
chmod 700 "$E2E_DIR"

# DOKPLOY_URL is the panel as the setup scripts reach it, on the same
# machine.
DOKPLOY_URL="${DOKPLOY_URL:-http://127.0.0.1:3000}"
COOKIE_JAR="$E2E_DIR/cookies.txt"

# Names of what the run creates in Dokploy.
E2E_PROJECT=e2e
E2E_POSTGRES=e2e-pg
E2E_COMPOSE=e2e-web
COMPANION_PROJECT=doktunnel
COMPANION_COMPOSE=companion
COMPANION_IMAGE=ghcr.io/alebak/doktunnel-companion
COMPANION_VERSION=0.0.0-e2e

# Throwaway credentials, valid only on the disposable runner.
ADMIN_EMAIL=admin@example.com
ADMIN_PASSWORD=E2eAdmin-1234
PG_DATABASE=e2e
PG_USER=e2e
PG_PASSWORD=E2ePg-1234

log() { printf '==> %s\n' "$*" >&2; }

fail() {
	printf '::error::%s\n' "$*" >&2
	exit 1
}

# pass records an assertion that held, in the log and the job summary.
pass() {
	printf 'PASS: %s\n' "$*"
	if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
		printf -- '- PASS: %s\n' "$*" >>"$GITHUB_STEP_SUMMARY"
	fi
}

# wait_until <seconds> <what> <command...> runs the command every 3 seconds
# until it succeeds, and fails the script after <seconds>.
wait_until() {
	local timeout=$1 what=$2
	shift 2
	local deadline=$((SECONDS + timeout))
	log "waiting up to ${timeout}s for $what"
	until "$@"; do
		if ((SECONDS >= deadline)); then
			fail "timed out after ${timeout}s waiting for $what"
		fi
		sleep 3
	done
}

# state_set <name> <value> and state_get <name> pass values between steps.
state_set() { printf '%s\n' "$2" >"$E2E_DIR/$1"; }
state_get() {
	[[ -s "$E2E_DIR/$1" ]] || fail "missing state '$1': did an earlier step fail?"
	read -r value <"$E2E_DIR/$1"
	printf '%s\n' "$value"
}

# runner_ip is the machine's primary IPv4 address: the "server IP" of the
# internal-network install, which serves the companion on Traefik's port 80.
runner_ip() {
	ip -4 route get 1.1.1.1 | awk '{ for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit } }'
}

# trpc <GET|POST> <procedure> <input JSON> calls Dokploy's tRPC API with the
# admin session and prints the result (result.data.json). The response body
# is kept in $E2E_DIR/last-response.json for diagnostics.
trpc() {
	local method=$1 procedure=$2 input=$3 body status
	body="$(jq -cn --argjson input "$input" '{json: $input}')"
	if [[ $method == GET ]]; then
		status="$(curl -sS -G -o "$E2E_DIR/last-response.json" -w '%{http_code}' \
			-b "$COOKIE_JAR" -c "$COOKIE_JAR" \
			--data-urlencode "input=$body" "$DOKPLOY_URL/api/trpc/$procedure")"
	else
		status="$(curl -sS -X POST -o "$E2E_DIR/last-response.json" -w '%{http_code}' \
			-b "$COOKIE_JAR" -c "$COOKIE_JAR" -H 'Content-Type: application/json' \
			--data "$body" "$DOKPLOY_URL/api/trpc/$procedure")"
	fi
	if [[ $status != 200 ]]; then
		printf '%s %s answered HTTP %s:\n' "$method" "$procedure" "$status" >&2
		jq . "$E2E_DIR/last-response.json" >&2 2>/dev/null || head -c 2000 "$E2E_DIR/last-response.json" >&2
		fail "Dokploy API call $procedure failed"
	fi
	jq -c '.result.data.json' "$E2E_DIR/last-response.json"
}

# swarm_container <service name> prints the ID of the running container of
# a Swarm service.
swarm_container() {
	docker ps -q --filter "label=com.docker.swarm.service.name=$1" --filter status=running | head -n 1
}
