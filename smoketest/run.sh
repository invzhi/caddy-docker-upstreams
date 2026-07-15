#!/usr/bin/env bash
#
# Automated smoke test:
# - build Caddy with the plugin,
# - bring up the Compose project,
# - assert every endpoint routes to the expected upstream,
# - tear down Caddy and containers on exit.
set -euo pipefail

cd "$(dirname "$0")"

log() {
	printf >&2 "\e[36;1m### $@\e[0m\n"
}

err() {
	printf >&2 "\e[31m### $@\e[0m\n"
}

caddy_pid=""
cleanup() {
	log "Stopping Caddy and services"
	if [[ -n "$caddy_pid" ]]; then
		kill "$caddy_pid" 2>/dev/null
	fi
	docker compose down --remove-orphans >/dev/null 2>&1 || true
	rm -f caddy.log
}
trap cleanup EXIT


log "Building Caddy with the plugin"
xcaddy build --with github.com/invzhi/caddy-docker-upstreams=.. --output ./caddy

log "Starting services and Caddy"
docker compose up -d
./caddy run --config Caddyfile >caddy.log 2>&1 &
caddy_pid=$!

log "Waiting for Caddy to serve"
for _ in $(seq 1 10); do
	curl -sf http://localhost:9001/ >/dev/null 2>&1 && break
	sleep 1
done

log "Checking routes"
failures=0
check() { # url expected-body
	local got
	got=$(curl -s --max-time 3 "$1" 2>/dev/null || true)
	if [[ "$got" == "$2" ]]; then
		printf '  ok   %-27s -> %s\n' "$1" "$got"
	else
		printf '  FAIL %-27s -> got %q, want %q\n' "$1" "$got" "$2"
		failures=$((failures + 1))
	fi
}

check http://localhost:9001/      "alpha"
check http://localhost:9002/      "beta"
check http://localhost:9003/      "multi api"
check http://localhost:9003/other "multi metrics"
check http://localhost:9004/      "multi metrics"
check http://localhost:9004/other "multi api"
check http://localhost:9005/      "web"

if [[ "$failures" -ne 0 ]]; then
	err "$failures route(s) failed"
	exit 1
fi
log "All routes OK"
