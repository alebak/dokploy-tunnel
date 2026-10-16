#!/usr/bin/env bash
#
# Runs the doktunnel CLI from this checkout against the Dokploy and the
# companion that deploy-services.sh set up, as a user would:
#   context add -> services -> forward (no --port) -> status -> psql and
#   HTTP through the .internal hostnames -> stop forward -> status empty ->
#   no repeater container left after the grace period.
#
# The API key lives in the OS keyring, so run this inside a D-Bus session:
#   dbus-run-session -- test/e2e/run-cli.sh
# forward elevates with sudo to write /etc/hosts, so it runs on a
# pseudo-terminal through script(1), as for a person; sudo must not prompt.

set -euo pipefail
# shellcheck source=test/e2e/lib.sh
source "$(dirname "$0")/lib.sh"

CONTEXT=e2e
# Longer than the repeater's grace period (internal/repeater DefaultGrace,
# 30 s) plus a cleanup pass.
REPEATER_GONE_TIMEOUT=90

ip="$(state_get runner-ip)"
out="$E2E_DIR/cli"
mkdir -p "$out"

# go-keyring talks to the Secret Service; gnome-keyring provides one.
start_keyring() {
	[[ -n ${DBUS_SESSION_BUS_ADDRESS:-} ]] || fail "no D-Bus session: run this under dbus-run-session"
	printf 'e2e' | gnome-keyring-daemon --unlock --components=secrets >/dev/null
}

build_cli() {
	mkdir -p "$E2E_DIR/bin"
	go build -o "$E2E_DIR/bin/doktunnel" ./cmd/doktunnel
	PATH="$E2E_DIR/bin:$PATH"
	doktunnel --version
}

# dt <name> <args...> runs doktunnel --json, keeping stdout and stderr in
# $out/<name>.json and $out/<name>.err for the diagnostics.
dt() {
	local name=$1
	shift
	if ! doktunnel "$@" --json >"$out/$name.json" 2>"$out/$name.err"; then
		cat "$out/$name.json" "$out/$name.err" >&2
		fail "doktunnel $* failed"
	fi
}

context_add() {
	DOKTUNNEL_API_KEY="$(state_get api-key)" dt context-add context add \
		--url "http://$ip:3000" --name "$CONTEXT" --companion-url "http://$ip/doktunnel"
	jq . "$out/context-add.json"
	jq -e --arg url "http://$ip/doktunnel" '.companion_url == $url' "$out/context-add.json" >/dev/null ||
		fail "context add did not store the companion URL"
	# Plain http:// is expected on an internal network; any other warning,
	# such as an unreachable companion, is not.
	if jq -e '(.warnings // []) | map(select(test("plain http://") | not)) | length > 0' "$out/context-add.json" >/dev/null; then
		fail "context add warned about something other than plain http://"
	fi
	pass "context add stored http://$ip/doktunnel and its /healthz probe answered"
}

services() {
	dt services services
	local all='[.projects[].environments[].services[]]'
	jq -e --arg name "$E2E_POSTGRES" "$all"' | any(.name == $name and .type == "postgres" and .kind == "service" and .default_port == 5432)' \
		"$out/services.json" >/dev/null || fail "services does not list postgres $E2E_POSTGRES with default port 5432"
	for svc in nginx whoami; do
		jq -e --arg name "$E2E_COMPOSE/$svc" "$all"' | any(.name == $name and .kind == "compose_service")' \
			"$out/services.json" >/dev/null || fail "services does not list compose service $E2E_COMPOSE/$svc"
	done
	pass "services lists $E2E_POSTGRES (postgres, default port 5432) and $E2E_COMPOSE/nginx, $E2E_COMPOSE/whoami"
}

# start_forward runs "forward" in the background on a pseudo-terminal. Its
# standard input is a FIFO held open on fd 3, so script(1) never sees an end
# of file and forward runs until it is signalled.
start_forward() {
	local cmd
	cmd="$(printf '%q ' "$E2E_DIR/bin/doktunnel" forward "$E2E_POSTGRES" "$E2E_COMPOSE/nginx")"
	rm -f "$E2E_DIR/forward.stdin"
	mkfifo "$E2E_DIR/forward.stdin"
	script -qefc "$cmd" "$E2E_DIR/forward.log" <"$E2E_DIR/forward.stdin" >/dev/null 2>&1 &
	script_pid=$!
	exec 3>"$E2E_DIR/forward.stdin"
}

forwards_listening() {
	kill -0 "$script_pid" 2>/dev/null || { cat "$E2E_DIR/forward.log" >&2; fail "forward exited early"; }
	doktunnel status --json >"$out/status.json" 2>"$out/status.err" &&
		jq -e '.forwards | length == 2' "$out/status.json" >/dev/null
}

# forward_field <target name> <jq field> reads a field of the running
# forward to <target name> from "status".
forward_field() {
	jq -er --arg name "$1" ".forwards[] | select(.target.name == \$name) | $2" "$out/status.json" ||
		fail "status has no $2 for $1"
}

pg_select_1() {
	PGPASSWORD="$PG_PASSWORD" psql -tA -c 'select 1' \
		"host=$pg_host port=$pg_port user=$PG_USER dbname=$PG_DATABASE connect_timeout=5" >"$out/psql.out" 2>&1
}

repeaters() { docker ps -aq --filter label=dev.doktunnel.repeater=1; }
no_repeaters() { [[ -z "$(repeaters)" ]]; }

start_keyring
build_cli
context_add
services

start_forward
wait_until 120 "forward to list both services in status" forwards_listening
jq . "$out/status.json"
pass "forward (no --port) listed $E2E_POSTGRES and $E2E_COMPOSE/nginx in status"

forward_pid="$(forward_field "$E2E_POSTGRES" .pid)"
pg_host="$(forward_field "$E2E_POSTGRES" .hostname)"
pg_port="$(forward_field "$E2E_POSTGRES" .port)"
web_host="$(forward_field "$E2E_COMPOSE/nginx" .hostname)"
web_port="$(forward_field "$E2E_COMPOSE/nginx" .port)"
web_service_port="$(forward_field "$E2E_COMPOSE/nginx" .service_port)"
[[ $pg_port == 5432 ]] || fail "postgres is forwarded on port $pg_port, want 5432"
[[ $web_port == 8080 && $web_service_port == 80 ]] ||
	fail "nginx is forwarded on $web_port -> $web_service_port, want 8080 -> 80"
for host in "$pg_host" "$web_host"; do
	[[ $host == *.internal ]] || fail "hostname $host does not end in .internal"
	getent hosts "$host" || fail "$host does not resolve through /etc/hosts"
done
pass "hostnames $pg_host:5432 and $web_host:8080 (container port 80) resolve"

wait_until 60 "psql through $pg_host" pg_select_1
[[ "$(tr -d '[:space:]' <"$out/psql.out")" == 1 ]] || fail "select 1 returned '$(<"$out/psql.out")'"
pass "psql host=$pg_host port=5432 -c 'select 1' returned 1"

code="$(curl -sS -o "$out/nginx.html" -w '%{http_code}' --max-time 20 "http://$web_host:$web_port/")"
[[ $code == 200 ]] || fail "GET http://$web_host:$web_port/ answered $code, want 200"
grep -qi nginx "$out/nginx.html" || fail "GET http://$web_host:$web_port/ is not nginx's page"
pass "curl http://$web_host:$web_port/ answered 200 from nginx inside the compose stack"

[[ -n "$(repeaters)" ]] || fail "no repeater container while tunnels were open"
pass "the companion created repeater containers for the open tunnels"

log "stopping forward (PID $forward_pid) with SIGINT"
kill -INT "$forward_pid"
status=0
wait "$script_pid" || status=$?
exec 3>&-
[[ $status == 0 ]] || { cat "$E2E_DIR/forward.log" >&2; fail "forward exited with status $status after SIGINT"; }
pass "forward exited 0 on SIGINT"

dt status-after status
jq -e '.forwards == []' "$out/status-after.json" >/dev/null || fail "status still lists forwards after forward stopped"
for host in "$pg_host" "$web_host"; do
	if grep -qw "$host" /etc/hosts; then
		fail "$host is still in /etc/hosts after forward stopped"
	fi
done
pass "status lists no forwards and /etc/hosts no longer holds the hostnames"

stopped=$SECONDS
wait_until "$REPEATER_GONE_TIMEOUT" "the repeater containers to be removed" no_repeaters
pass "no repeater container left $((SECONDS - stopped))s after forward stopped (grace 30 s)"
