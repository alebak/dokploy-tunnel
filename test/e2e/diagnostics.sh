#!/usr/bin/env bash
#
# Prints what helps to understand a failed e2e run: Docker state, the logs
# of Dokploy, Traefik, the companion and the socket proxy, and the CLI's
# outputs. Every command is allowed to fail.

set -uo pipefail
# shellcheck source=test/e2e/lib.sh
source "$(dirname "$0")/lib.sh"

section() { printf '\n::group::%s\n' "$*"; }
end() { echo '::endgroup::'; }

# compose_logs <compose service> prints the logs of that service's
# containers, whichever Dokploy compose project they belong to.
compose_logs() {
	local id
	for id in $(docker ps -aq --filter "label=com.docker.compose.service=$1"); do
		docker logs --tail 300 --timestamps "$id" 2>&1
	done
}

section "docker services and containers"
docker service ls
docker ps -a --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Labels}}'
docker network ls
df -h /
end

section "dokploy logs"
docker service ps dokploy --no-trunc
docker service logs dokploy --tail 300 --timestamps 2>&1
end

section "traefik logs"
docker logs dokploy-traefik --tail 200 --timestamps 2>&1
end

section "companion logs"
compose_logs companion
end

section "socket-proxy logs"
compose_logs socket-proxy
end

section "repeater containers"
docker ps -a --filter label=dev.doktunnel.repeater=1
end

section "Dokploy install and API"
tail -n 100 "$E2E_DIR/install.log"
echo "--- last API response"
head -c 4000 "$E2E_DIR/last-response.json"
echo
echo "--- companion compose file"
cat "$E2E_DIR/companion-compose.yml"
end

section "doktunnel CLI outputs"
for f in "$E2E_DIR"/cli/*; do
	echo "--- $f"
	cat "$f"
done
echo "--- forward (terminal log)"
cat "$E2E_DIR/forward.log"
echo "--- /etc/hosts"
cat /etc/hosts
end

exit 0
