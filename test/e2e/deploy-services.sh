#!/usr/bin/env bash
#
# Creates, through Dokploy's API, what the CLI is tested against:
#   - project "e2e" with a Postgres database and a compose stack
#     (test/e2e/web-stack.yml);
#   - project "doktunnel" with the companion, deployed from
#     deploy/dokploy/docker-compose.yml in its internal-network variant
#     (docs/install-companion.md, "Other addresses"): Traefik's "web"
#     entrypoint on port 80, the runner's IP as PANEL_DOMAIN, no TLS.
# Then waits until all of them run and the companion answers /healthz.
#
# Needs install-dokploy.sh and build-companion.sh to have run.

set -euo pipefail
# shellcheck source=test/e2e/lib.sh
source "$(dirname "$0")/lib.sh"

# create_project <name> prints the ID of the project's default environment.
create_project() {
	local created env_id
	created="$(trpc POST project.create "$(jq -cn --arg name "$1" '{name: $name, description: ""}')")"
	env_id="$(jq -r '.environment.environmentId // empty' <<<"$created")"
	[[ -n $env_id ]] || fail "project.create returned no environment for project $1: $created"
	printf '%s\n' "$env_id"
}

# create_compose <environment ID> <name> <compose file> [<env>] creates a raw
# "Docker Compose" service, deploys it and prints its ID.
create_compose() {
	local env_id=$1 name=$2 file=$3 env=${4:-} created compose_id
	created="$(trpc POST compose.create "$(jq -cn --arg env "$env_id" --arg name "$name" --rawfile file "$file" \
		'{name: $name, appName: $name, description: "", environmentId: $env,
		  composeType: "docker-compose", sourceType: "raw", composeFile: $file}')")"
	compose_id="$(jq -r '.composeId // empty' <<<"$created")"
	[[ -n $compose_id ]] || fail "compose.create returned no composeId for $name: $created"
	if [[ -n $env ]]; then
		trpc POST compose.update "$(jq -cn --arg id "$compose_id" --arg env "$env" \
			'{composeId: $id, env: $env, createEnvFile: true}')" >/dev/null
	fi
	trpc POST compose.deploy "$(jq -cn --arg id "$compose_id" '{composeId: $id}')" >/dev/null
	log "compose $name ($compose_id) deploying"
	printf '%s\n' "$compose_id"
}

compose_done() {
	local status
	status="$(trpc GET compose.one "$(jq -cn --arg id "$1" '{composeId: $id}')" | jq -r '.composeStatus')"
	[[ $status == error ]] && fail "the deployment of compose $1 failed"
	[[ $status == "done" ]]
}

postgres_done() {
	local status
	status="$(trpc GET postgres.one "$(jq -cn --arg id "$1" '{postgresId: $id}')" | jq -r '.applicationStatus')"
	[[ $status == error ]] && fail "the deployment of postgres $1 failed"
	[[ $status == "done" ]]
}

postgres_ready() {
	local container
	container="$(swarm_container "$1")"
	[[ -n $container ]] && docker exec "$container" pg_isready -q -U "$PG_USER" -d "$PG_DATABASE"
}

companion_healthy() { curl -fsS --max-time 10 -o /dev/null "http://$ip/doktunnel/healthz"; }

# The internal-network variant of the companion's compose file.
companion_compose_file() {
	local file="$E2E_DIR/companion-compose.yml"
	sed -e 's/\.entrypoints=websecure$/.entrypoints=web/' -e '/\.tls\.certresolver=/d' \
		deploy/dokploy/docker-compose.yml >"$file"
	grep -q 'doktunnel-companion\.entrypoints=web$' "$file" || fail "could not switch the companion to the web entrypoint"
	if grep -q 'certresolver' "$file"; then
		fail "could not remove the companion's certresolver label"
	fi
	printf '%s\n' "$file"
}

ip="$(runner_ip)"
[[ -n $ip ]] || fail "could not find the runner's IP address"
state_set runner-ip "$ip"
log "runner IP: $ip"

# Targets.
e2e_env="$(create_project "$E2E_PROJECT")"
pg="$(trpc POST postgres.create "$(jq -cn --arg env "$e2e_env" --arg name "$E2E_POSTGRES" \
	--arg db "$PG_DATABASE" --arg user "$PG_USER" --arg password "$PG_PASSWORD" \
	'{name: $name, appName: $name, description: "", environmentId: $env, dockerImage: "postgres:16",
	  databaseName: $db, databaseUser: $user, databasePassword: $password}')")"
pg_id="$(jq -r '.postgresId // empty' <<<"$pg")"
pg_app="$(jq -r '.appName // empty' <<<"$pg")"
[[ -n $pg_id && -n $pg_app ]] || fail "postgres.create returned no postgresId or appName: $pg"
state_set postgres-app-name "$pg_app"
trpc POST postgres.deploy "$(jq -cn --arg id "$pg_id" '{postgresId: $id}')" >/dev/null
log "postgres $E2E_POSTGRES ($pg_id, $pg_app) deploying"

web_id="$(create_compose "$e2e_env" "$E2E_COMPOSE" test/e2e/web-stack.yml)"

# Companion, in its own project as the install guide asks.
companion_env="$(create_project "$COMPANION_PROJECT")"
companion_vars="DOKTUNNEL_VERSION=$COMPANION_VERSION
PANEL_DOMAIN=$ip
DOCKER_GID=$(stat -c %g /var/run/docker.sock)"
companion_id="$(create_compose "$companion_env" "$COMPANION_COMPOSE" "$(companion_compose_file)" "$companion_vars")"

wait_until 360 "postgres $E2E_POSTGRES to deploy" postgres_done "$pg_id"
wait_until 120 "postgres $E2E_POSTGRES to accept connections" postgres_ready "$pg_app"
pass "postgres $E2E_POSTGRES deployed through postgres.create + postgres.deploy and ready"
wait_until 300 "compose $E2E_COMPOSE to deploy" compose_done "$web_id"
pass "compose $E2E_COMPOSE deployed through compose.create (raw) + compose.deploy"
wait_until 300 "the companion compose to deploy" compose_done "$companion_id"
wait_until 120 "the companion on http://$ip/doktunnel/healthz" companion_healthy
pass "companion deployed from deploy/dokploy/docker-compose.yml (internal-network variant) answers http://$ip/doktunnel/healthz"

docker ps --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}'
