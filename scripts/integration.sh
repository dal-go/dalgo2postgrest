#!/usr/bin/env bash
# Runs the opt-in integration tests against a disposable PostgreSQL 17 +
# PostgREST v16.5 project. Every run gets its own Compose project name and two
# free loopback ports, and tears down its own containers and volume.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PROJECT="datatug-postgrest-$(date +%s)-$$"
read -r POSTGRES_PORT POSTGREST_PORT < <(python3 - <<'PY'
import socket

def free_port():
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port

print(free_port(), free_port())
PY
)
export POSTGRES_PORT POSTGREST_PORT

COMPOSE=(docker compose -p "$PROJECT" -f "$ROOT/testdata/docker-compose.yml")
LOG="${TMPDIR:-/tmp}/datatug-postgrest-${PROJECT}.log"

cleanup() {
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

echo "PostgREST fixture project: $PROJECT"
echo "PostgreSQL port: $POSTGRES_PORT, PostgREST port: $POSTGREST_PORT"

"${COMPOSE[@]}" up -d

for attempt in $(seq 1 90); do
  if curl -fsS -o /dev/null "http://127.0.0.1:${POSTGREST_PORT}/" 2>/dev/null; then
    break
  fi
  if [ "$attempt" -eq 90 ]; then
    echo "PostgREST did not become ready" >&2
    "${COMPOSE[@]}" logs postgres postgrest >&2 || true
    exit 1
  fi
  sleep 1
done

server_header="$(curl -fsSI "http://127.0.0.1:${POSTGREST_PORT}/" | tr -d '\r' | awk 'tolower($1) == "server:" { print $2 }')"
case "$server_header" in
  postgrest/16.5*) ;;
  *)
    echo "unexpected PostgREST server header: ${server_header:-<missing>}" >&2
    exit 1
    ;;
esac
echo "PostgREST server: $server_header"

item_count="$("${COMPOSE[@]}" exec -T postgres psql -U postgres -d appdb -tAc 'select count(*) from app.items')"
label_count="$("${COMPOSE[@]}" exec -T postgres psql -U postgres -d appdb -tAc 'select count(*) from app.labels')"
if [ "$item_count" != "4" ] || [ "$label_count" != "6" ]; then
  echo "expected 4 items and 6 labels externally seeded, found ${item_count} and ${label_count}" >&2
  exit 1
fi
can_insert="$("${COMPOSE[@]}" exec -T postgres psql -U postgres -d appdb -tAc "select has_table_privilege('app_reader','app.items','INSERT')")"
can_select="$("${COMPOSE[@]}" exec -T postgres psql -U postgres -d appdb -tAc "select has_table_privilege('app_reader','app.items','SELECT')")"
if [ "$can_insert" != "f" ] || [ "$can_select" != "t" ]; then
  echo "fixture role privileges are wrong: select=${can_select} insert=${can_insert}" >&2
  exit 1
fi
echo "Externally seeded rows: items=${item_count}, labels=${label_count}; app_reader select=${can_select}, insert=${can_insert}"

set +e
POSTGREST_TEST_URL="http://127.0.0.1:${POSTGREST_PORT}" \
POSTGREST_TEST_JWT_SECRET="integration-test-secret-that-is-long-enough" \
POSTGREST_TEST_READER_ROLE="app_reader" \
POSTGREST_TEST_NOACCESS_ROLE="noaccess" \
go test -race -v -count=1 -run '^TestIntegration' ./... 2>&1 | tee "$LOG"
status=${PIPESTATUS[0]}
set -e

if [ "$status" -ne 0 ]; then
  echo "integration tests failed" >&2
  exit "$status"
fi
if grep -q -- '--- SKIP: TestIntegration_' "$LOG"; then
  echo "an integration test was skipped; the fixture environment did not reach the tests" >&2
  exit 1
fi
if ! grep -q -- '--- PASS: TestIntegration_' "$LOG"; then
  echo "no integration test passed" >&2
  exit 1
fi

cleanup
trap - EXIT INT TERM

if docker ps -aq --filter "label=com.docker.compose.project=${PROJECT}" | grep -q .; then
  echo "teardown left containers for project ${PROJECT}" >&2
  exit 1
fi
echo "Teardown complete: no containers remain for project ${PROJECT}"
