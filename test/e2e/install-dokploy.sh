#!/usr/bin/env bash
#
# Installs Dokploy with its official install.sh at a pinned version, creates
# the first admin (owner) and an API key for that admin's organization, as
# Dokploy's own .github/workflows/upgrade-integration-test.yml does.
#
# Inputs (environment): DOKPLOY_VERSION, a Dokploy release tag such as
# v0.30.8. Outputs in $E2E_DIR: cookies.txt (admin session), api-key,
# organization-id.

set -euo pipefail
# shellcheck source=test/e2e/lib.sh
source "$(dirname "$0")/lib.sh"

: "${DOKPLOY_VERSION:?set DOKPLOY_VERSION to a Dokploy release tag}"
[[ $DOKPLOY_VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
	fail "DOKPLOY_VERSION '$DOKPLOY_VERSION' is not a release tag such as v0.30.8"

# Dokploy pulls several images; free space only when the runner is short.
free_disk() {
	local avail_gb
	avail_gb="$(df --output=avail -BG / | tail -n 1 | tr -dc '0-9')"
	log "free disk space: ${avail_gb}G"
	if ((avail_gb < 10)); then
		log "freeing disk space"
		sudo rm -rf /usr/share/dotnet /usr/local/lib/android /opt/ghc /usr/local/share/boost
		df -h /
	fi
}

install_dokploy() {
	log "installing Dokploy $DOKPLOY_VERSION"
	curl -fsSL --max-time 60 https://dokploy.com/install.sh -o "$E2E_DIR/install.sh"
	# ADVERTISE_ADDR skips install.sh's lookup of the public IP. The log is
	# written as this user, on purpose.
	# shellcheck disable=SC2024
	sudo env DOKPLOY_VERSION="$DOKPLOY_VERSION" ADVERTISE_ADDR=127.0.0.1 \
		bash "$E2E_DIR/install.sh" >"$E2E_DIR/install.log" 2>&1 ||
		{ tail -n 50 "$E2E_DIR/install.log" >&2; fail "Dokploy's install.sh failed"; }
}

dokploy_service_ready() {
	docker service ls --filter name=dokploy --format '{{.Name}} {{.Replicas}}' | grep -q '^dokploy 1/1'
}

panel_up() { curl -fsS --max-time 10 -o /dev/null "$DOKPLOY_URL"; }

# better-auth only accepts a sign-up while no owner exists; the panel may
# answer before its API does, so it is retried.
sign_up() {
	: >"$COOKIE_JAR"
	chmod 600 "$COOKIE_JAR"
	curl -fsS --max-time 30 -o "$E2E_DIR/sign-up.json" -X POST "$DOKPLOY_URL/api/auth/sign-up/email" \
		-H 'Content-Type: application/json' -c "$COOKIE_JAR" -b "$COOKIE_JAR" \
		--data "$(jq -cn --arg email "$ADMIN_EMAIL" --arg password "$ADMIN_PASSWORD" \
			'{name: "E2E Admin", email: $email, password: $password}')" &&
		grep -q 'session_token' "$COOKIE_JAR"
}

create_api_key() {
	local org_id key_json key
	org_id="$(trpc GET organization.all null | jq -r '.[0].id // empty')"
	[[ -n $org_id ]] || fail "the admin has no organization"
	state_set organization-id "$org_id"

	# The panel's form sends rateLimitEnabled: false. Left out, better-auth's
	# default applies (10 requests a day), and Dokploy then answers 401.
	key_json="$(trpc POST user.createApiKey "$(jq -cn --arg org "$org_id" \
		'{name: "doktunnel-e2e", rateLimitEnabled: false, metadata: {organizationId: $org}}')")"
	key="$(jq -r '.key // empty' <<<"$key_json")"
	[[ -n $key ]] || fail "user.createApiKey returned no key"
	echo "::add-mask::$key"
	(umask 077 && printf '%s\n' "$key" >"$E2E_DIR/api-key")

	# The key alone, without the session, must read the organization.
	curl -fsS --max-time 30 -o /dev/null -H "x-api-key: $key" "$DOKPLOY_URL/api/trpc/project.all" ||
		fail "the new API key cannot call project.all"
	pass "API key created for organization $org_id and accepted by project.all"
}

free_disk
install_dokploy
wait_until 300 "the dokploy Swarm service to run" dokploy_service_ready
wait_until 180 "the Dokploy panel on $DOKPLOY_URL" panel_up
wait_until 120 "the admin sign-up" sign_up
pass "Dokploy $DOKPLOY_VERSION installed and the first admin signed up"
create_api_key
docker service ls
