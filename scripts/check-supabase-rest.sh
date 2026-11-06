#!/usr/bin/env bash
# check-supabase-rest.sh is the actual, opt-in acceptance for the Supabase-style
# REST fixture. It starts its own PostgreSQL 18 + PostgREST 16.5 + Kong 3.9.3
# Compose project, proves the externally seeded rows and the raw HTTP auth/RLS
# premises through the owned Kong gateway, runs the source-tree integration
# tests, runs the separate public-module consumer pinned to the published SDK,
# and invokes the installed datatug CLI read-only.
#
# Ownership: one unique Compose project, one random loopback Kong port, one
# private runtime directory and one private log directory per run. PostgreSQL
# and PostgREST publish no host port; Kong publishes only its proxy on
# 127.0.0.1; its admin listener is off. Teardown removes exactly this run's
# project, volume and network and then proves they are gone. Nothing is pruned
# by prefix and no shared port is touched.
#
# Honesty about the CLI phase:
#   - Default: DATATUG_CLI_BIN unset, so the installed binary at
#     /Users/alex/.local/bin/datatug is required and its exact version, source
#     commit and SHA-256 are checked before any assumption. It is never updated.
#   - DATATUG_CLI_BIN set: that binary is treated as a source/CI build; the
#     installed-release version/SHA claim is skipped and said so explicitly.
#   - SUPABASE_SKIP_INSTALLED_CLI=1: the CLI phase is skipped, and the run
#     explicitly does not claim installed-release acceptance. The fixture and
#     the public-module consumer still run.
set -euo pipefail

cd "$(dirname "$0")/.."
root=$PWD
fixture_dir=$root/testdata/supabase
compose_file=$fixture_dir/compose.yml
consumer_src=$root/testdata/consumer

project=dalgo2postgrest-supabase-$(date +%s)-$$
work=$(mktemp -d "${TMPDIR:-/tmp}/dalgo2postgrest-supabase-work.XXXXXX")
logs=$(mktemp -d "${TMPDIR:-/tmp}/dalgo2postgrest-supabase-logs.XXXXXX")
chmod 700 "$logs"

api_key=sb_publishable_datatug_dalgo2postgrest_fixture
cli_default=/Users/alex/.local/bin/datatug
cli_bin=${DATATUG_CLI_BIN:-$cli_default}
cli_release_version=0.127.0
cli_release_source=4df7e9ce1a2319c01badeabd450703ea72f03993
cli_release_sha256=4aa2bb538ba890e4d0e4ac069f1e4323a9e56f88824366b26b80293662f3e603
skip_installed_cli=${SUPABASE_SKIP_INSTALLED_CLI:-0}

compose=(docker compose -p "$project" --file "$compose_file")
started=0

# shellcheck source=scripts/lib/supabase-lifecycle.sh
. "$root/scripts/lib/supabase-lifecycle.sh"
supabase_lifecycle_install

say() { printf '%s\n' "$*"; }
fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
say "Supabase-style REST fixture project: $project"
say "Logs (private, retained on failure): $logs"

command -v docker >/dev/null 2>&1 || fail "docker is required"
docker_version=$(docker version --format '{{.Server.Version}}') ||
	fail "the Docker daemon is not reachable; this stage needs an actual fixture"
compose_version=$(docker compose version --short) || fail "docker compose is required"
say "Docker server $docker_version, Compose $compose_version"

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v python3 >/dev/null 2>&1 || fail "python3 is required for bounded probes"
for file in "$compose_file" "$fixture_dir/kong.yml" "$fixture_dir/seed.sql" "$consumer_src/go.mod"; do
	[ -f "$file" ] || fail "fixture file is missing: $file"
done
if grep -q '^replace ' "$consumer_src/go.mod"; then
	fail "the public-module consumer must not carry a replace directive"
fi

kong_port=$(python3 - <<'PY'
import socket
sock = socket.socket()
sock.bind(("127.0.0.1", 0))
print(sock.getsockname()[1])
sock.close()
PY
) || fail "could not reserve a loopback port"
[ -n "$kong_port" ] || fail "could not reserve a loopback port"
export SUPABASE_KONG_PORT=$kong_port
SUPABASE_FIXTURE_JWT_SECRET=$(python3 -c 'import secrets; print(secrets.token_hex(32))')
export SUPABASE_FIXTURE_JWT_SECRET

kong_base="http://127.0.0.1:${kong_port}"

# ---------------------------------------------------------------------------
# Raw HTTP helpers. Every response body/header stays in the private log dir.
# ---------------------------------------------------------------------------
call_api() { # NAME METHOD URL [curl args...]
	local name=$1 method=$2 url=$3
	shift 3
	local status
	status=$(curl -sS --max-time 20 -X "$method" \
		-D "$logs/${name}.headers" -o "$logs/${name}.body" -w '%{http_code}' \
		"$@" "$url") || {
		printf 'probe %s: request failed\n' "$name" >&2
		return 1
	}
	printf '%s' "$status" >"$logs/${name}.status"
}

expect_status() { # NAME EXPECTED-SPACE-SEPARATED
	local name=$1 want=$2 got
	got=$(<"$logs/${name}.status")
	case " $want " in
		*" $got "*) return 0 ;;
	esac
	printf 'probe %s: HTTP %s, want one of [%s]\n' "$name" "$got" "$want" >&2
	return 1
}

expect_body() { # NAME PYTHON-SOURCE FILE-ARG...
	local name=$1 code=$2
	shift 2
	if ! python3 -c "$code" "$@"; then
		printf 'probe %s: response body assertion failed\n' "$name" >&2
		return 1
	fi
}

# ---------------------------------------------------------------------------
# Start the owned fixture
# ---------------------------------------------------------------------------
started=1
say "starting PostgreSQL 18 + PostgREST 16.5 + Kong 3.9.3 on loopback port $kong_port"
"${compose[@]}" up -d >"$logs/compose-up.log" 2>&1 || {
	sed -n '1,120p' "$logs/compose-up.log" >&2
	fail "docker compose up failed"
}

postgres_id=$("${compose[@]}" ps -q postgres) || fail "could not inspect the postgres container"
postgrest_id=$("${compose[@]}" ps -q postgrest) || fail "could not inspect the postgrest container"
kong_id=$("${compose[@]}" ps -q kong) || fail "could not inspect the kong container"
for id in "$postgres_id" "$postgrest_id" "$kong_id"; do
	[ -n "$id" ] || fail "the owned Compose project did not start every fixture service"
done

# Wait for the exact refusal this fixture is built on, then the relation. Both
# waits are bounded and fatal at the bound.
ready=0
for _ in $(seq 1 120); do
	status=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' \
		-H "apikey: $api_key" "$kong_base/rest/v1/") || status=000
	if [ "$status" = 403 ]; then
		ready=1
		break
	fi
	sleep 1
done
[ "$ready" = 1 ] || {
	"${compose[@]}" ps >"$logs/compose-ps.log" 2>&1 || true
	fail "Kong did not refuse the OpenAPI root with HTTP 403 within the bound; see $logs"
}

# ---------------------------------------------------------------------------
# Ownership: only Kong publishes a host port, only on loopback, admin off.
# ---------------------------------------------------------------------------
assert_no_host_ports() { # NAME ID
	local name=$1 id=$2 ports_json
	ports_json=$(docker inspect --format '{{json .NetworkSettings.Ports}}' "$id") ||
		fail "could not inspect $name port bindings"
	python3 "$root/scripts/lib/supabase-ports.py" none "$name" "$ports_json"
}

assert_no_host_ports "owned PostgreSQL" "$postgres_id"
assert_no_host_ports "owned PostgREST" "$postgrest_id"

kong_ports_json=$(docker inspect --format '{{json .NetworkSettings.Ports}}' "$kong_id") ||
	fail "could not inspect owned Kong port bindings"
kong_env_json=$(docker inspect --format '{{json .Config.Env}}' "$kong_id") ||
	fail "could not inspect owned Kong environment"
python3 "$root/scripts/lib/supabase-ports.py" kong "$kong_port" "$kong_ports_json" "$kong_env_json"

# The actual server versions come from the running containers, not a tag.
postgrest_version=$("${compose[@]}" exec -T postgrest postgrest --version 2>&1) ||
	fail "could not read the PostgREST server version"
case "$postgrest_version" in
	*16.5*) ;;
	*) fail "PostgREST server version is not 16.5: $postgrest_version" ;;
esac
kong_version=$("${compose[@]}" exec -T kong kong version 2>&1) || fail "could not read the Kong server version"
case "$kong_version" in
	*3.9.3*) ;;
	*) fail "Kong server version is not 3.9.3: $kong_version" ;;
esac
say "Servers: $postgrest_version; Kong $kong_version"

# ---------------------------------------------------------------------------
# Short-lived scoped tokens, minted with this run's ephemeral secret.
# ---------------------------------------------------------------------------
mint_token() { # ROLE TTL-SECONDS SUB
	python3 - "$1" "$2" "${3-}" <<'PY'
import base64
import hashlib
import hmac
import json
import os
import sys
import time

role, ttl, sub = sys.argv[1], int(sys.argv[2]), sys.argv[3]
secret = os.environ["SUPABASE_FIXTURE_JWT_SECRET"].encode()


def encode(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


now = int(time.time())
header = encode(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
claims = {"role": role, "iat": now - 60, "exp": now + ttl}
if sub:
    claims["sub"] = sub
payload = encode(json.dumps(claims, separators=(",", ":")).encode())
signature = encode(hmac.new(secret, f"{header}.{payload}".encode(), hashlib.sha256).digest())
print(f"{header}.{payload}.{signature}")
PY
}

mint_bad_signature_token() {
	python3 - "$1" "$2" <<'PY'
import base64
import hashlib
import hmac
import json
import sys
import time

role, sub = sys.argv[1], sys.argv[2]


def encode(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


now = int(time.time())
header = encode(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
payload = encode(
    json.dumps({"role": role, "iat": now - 60, "exp": now + 3600, "sub": sub}, separators=(",", ":")).encode()
)
signature = encode(hmac.new(b"a-different-signing-secret", f"{header}.{payload}".encode(), hashlib.sha256).digest())
print(f"{header}.{payload}.{signature}")
PY
}

bearer=$(mint_token authenticated 1800 fixture-user) || fail "could not mint the fixture-user token"
expired_bearer=$(mint_token authenticated -600 fixture-user) || fail "could not mint the expired token"
empty_sub_bearer=$(mint_token authenticated 1800 nobody) || fail "could not mint the empty-subject token"
noaccess_bearer=$(mint_token noaccess 1800 "") || fail "could not mint the noaccess token"
bad_signature_bearer=$(mint_bad_signature_token authenticated fixture-user) ||
	fail "could not mint the bad-signature token"
bad_api_key=sb_publishable_datatug_wrong_key

# ---------------------------------------------------------------------------
# Externally seeded rows and grants, checked in PostgreSQL itself.
# ---------------------------------------------------------------------------
psql_scalar() {
	"${compose[@]}" exec -T postgres psql -U postgres -d appdb -tAc "$1"
}
visible_rows=$(psql_scalar "select count(*) from app.records where owner_sub = 'fixture-user'") ||
	fail "could not count the fixture-user rows"
hidden_rows=$(psql_scalar "select count(*) from app.records where owner_sub = 'other-user'") ||
	fail "could not count the hidden rows"
[ "$visible_rows" = 250 ] || fail "expected 250 fixture-user rows, found $visible_rows"
[ "$hidden_rows" = 20 ] || fail "expected 20 hidden owner rows, found $hidden_rows"
select_grant=$(psql_scalar "select has_table_privilege('authenticated','app.records','SELECT')") ||
	fail "could not inspect the SELECT grant"
insert_grant=$(psql_scalar "select has_table_privilege('authenticated','app.records','INSERT')") ||
	fail "could not inspect the INSERT grant"
update_grant=$(psql_scalar "select has_table_privilege('authenticated','app.records','UPDATE')") ||
	fail "could not inspect the UPDATE grant"
delete_grant=$(psql_scalar "select has_table_privilege('authenticated','app.records','DELETE')") ||
	fail "could not inspect the DELETE grant"
restricted_grant=$(psql_scalar "select has_table_privilege('authenticated','app.restricted_notes','SELECT')") ||
	fail "could not inspect the restricted relation grant"
[ "$select_grant" = t ] || fail "authenticated lost SELECT on app.records"
[ "$insert_grant$update_grant$delete_grant" = fff ] ||
	fail "authenticated holds a write grant: insert=$insert_grant update=$update_grant delete=$delete_grant"
[ "$restricted_grant" = f ] || fail "authenticated unexpectedly holds SELECT on app.restricted_notes"
big_number=$(psql_scalar "select big_number from app.records where record_id = 1") ||
	fail "could not read the 2^53+1 fixture value"
[ "$big_number" = 9007199254740993 ] || fail "the 2^53+1 row is not seeded exactly: $big_number"
say "Externally seeded: 250 visible rows, 20 hidden owner rows; SELECT-only grants proven in PostgreSQL"

# ---------------------------------------------------------------------------
# Raw HTTP premises through Kong: the refusals are real, not inferred from SQL.
# ---------------------------------------------------------------------------
# Exact OpenAPI root: valid key reaches real key-auth + admin-only ACL and is
# refused. A keyless root is refused by key-auth itself.
call_api root-with-key GET "$kong_base/rest/v1/" -H "apikey: $api_key" || fail "root probe failed"
expect_status root-with-key 403 || fail "the OpenAPI root is not refused to the public fixture key"
call_api root-no-key GET "$kong_base/rest/v1/" || fail "keyless root probe failed"
expect_status root-no-key 401 || fail "the OpenAPI root is not refused without an apikey"
say "OpenAPI root: HTTP 403 with the public key, HTTP 401 without it (Kong key-auth + admin-only ACL)"

call_api relation-ok GET "$kong_base/rest/v1/records?select=record_id&limit=1" \
	-H "apikey: $api_key" -H "Authorization: Bearer $bearer" || fail "relation probe failed"
expect_status relation-ok 200 || fail "the known relation is not readable through Kong"
expect_body relation-ok '
import json, sys
rows = json.load(open(sys.argv[1]))
if rows != [{"record_id": 1}]:
    raise SystemExit("known relation read returned %r" % (rows,))
' "$logs/relation-ok.body" || fail "the known relation read did not return row 1"

call_api relation-no-bearer GET "$kong_base/rest/v1/records?select=record_id&limit=1" \
	-H "apikey: $api_key" || fail "no-bearer probe failed"
expect_status relation-no-bearer "401 403" || fail "a request without a bearer was not refused"

call_api relation-bad-apikey GET "$kong_base/rest/v1/records?select=record_id&limit=1" \
	-H "apikey: $bad_api_key" -H "Authorization: Bearer $bearer" || fail "bad-apikey probe failed"
expect_status relation-bad-apikey 401 || fail "a bad apikey was not refused"

call_api relation-missing-apikey GET "$kong_base/rest/v1/records?select=record_id&limit=1" \
	-H "Authorization: Bearer $bearer" || fail "missing-apikey probe failed"
expect_status relation-missing-apikey 401 || fail "a missing apikey was not refused"

call_api relation-expired GET "$kong_base/rest/v1/records?select=record_id&limit=1" \
	-H "apikey: $api_key" -H "Authorization: Bearer $expired_bearer" || fail "expired-token probe failed"
expect_status relation-expired 401 || fail "an expired token was not refused"

call_api relation-bad-signature GET "$kong_base/rest/v1/records?select=record_id&limit=1" \
	-H "apikey: $api_key" -H "Authorization: Bearer $bad_signature_bearer" || fail "bad-signature probe failed"
expect_status relation-bad-signature 401 || fail "a wrongly signed token was not refused"
say "Auth: missing/bad apikey 401; expired and wrongly signed bearer 401"

# RLS: the hidden row really exists (counted above) and is really invisible,
# while row 1 is visible. Empty-subject is a real empty array, not an error.
call_api hidden-row GET "$kong_base/rest/v1/records?select=record_id&record_id=eq.9001" \
	-H "apikey: $api_key" -H "Authorization: Bearer $bearer" || fail "hidden-row probe failed"
expect_status hidden-row 200 || fail "the hidden-row probe did not succeed"
expect_body hidden-row '
import json, sys
rows = json.load(open(sys.argv[1]))
if rows:
    raise SystemExit("RLS leaked another owner row: %r" % (rows,))
' "$logs/hidden-row.body" || fail "RLS leaked the other owner's row"

call_api empty-subject GET "$kong_base/rest/v1/records?select=record_id&limit=10" \
	-H "apikey: $api_key" -H "Authorization: Bearer $empty_sub_bearer" || fail "empty-subject probe failed"
expect_status empty-subject 200 || fail "the empty-subject read did not succeed"
expect_body empty-subject '
import json, sys
rows = json.load(open(sys.argv[1]))
if rows:
    raise SystemExit("the empty subject saw rows: %r" % (rows,))
' "$logs/empty-subject.body" || fail "the empty subject saw rows"
say "RLS: row 9001 is present but invisible; a scoped empty subject gets 200 []"

# Denied grants: a role with schema USAGE and no table grant is refused.
call_api noaccess GET "$kong_base/rest/v1/records?select=record_id&limit=1" \
	-H "apikey: $api_key" -H "Authorization: Bearer $noaccess_bearer" || fail "noaccess probe failed"
expect_status noaccess "401 403" || fail "a role without grants was not refused"
call_api restricted GET "$kong_base/rest/v1/restricted_notes?select=id&limit=1" \
	-H "apikey: $api_key" -H "Authorization: Bearer $bearer" || fail "restricted-relation probe failed"
expect_status restricted "401 403" || fail "the ungranted relation was not refused"

# A real write refusal, then a row-absence probe so a status alone cannot hide
# a silent insert.
call_api write-probe POST "$kong_base/rest/v1/records" \
	-H "apikey: $api_key" -H "Authorization: Bearer $bearer" \
	-H "Content-Type: application/json" -H "Prefer: return=minimal" \
	--data '{"record_id":990001,"title":"write probe","amount":1,"big_number":1,"owner_sub":"fixture-user","payload":{},"created_at":"2026-01-01T00:00:00Z"}' ||
	fail "write probe failed"
expect_status write-probe "401 403" || fail "the fixture accepted an authenticated write"
call_api write-absence GET "$kong_base/rest/v1/records?select=record_id&record_id=eq.990001" \
	-H "apikey: $api_key" -H "Authorization: Bearer $bearer" || fail "write-absence probe failed"
expect_status write-absence 200 || fail "the write-absence probe did not succeed"
expect_body write-absence '
import json, sys
rows = json.load(open(sys.argv[1]))
if rows:
    raise SystemExit("the refused write row is readable: %r" % (rows,))
' "$logs/write-absence.body" || fail "the refused write left a readable row"
write_count=$(psql_scalar "select count(*) from app.records where record_id = 990001") ||
	fail "could not check the write probe row externally"
[ "$write_count" = 0 ] || fail "the write probe row exists in PostgreSQL after the refusal"

# The server cap is real: limit=100 cannot return 100 rows from one PostgREST
# response when db-max-rows=37.
call_api capped GET "$kong_base/rest/v1/records?select=record_id&order=record_id.asc&limit=100" \
	-H "apikey: $api_key" -H "Authorization: Bearer $bearer" || fail "server-cap probe failed"
expect_status capped 200 || fail "the server-cap probe did not succeed"
expect_body capped '
import json, sys
rows = json.load(open(sys.argv[1]))
if len(rows) != 37:
    raise SystemExit("the server cap returned %d rows for limit=100, want 37" % len(rows))
' "$logs/capped.body" || fail "PostgREST did not apply the 37-row server cap"

# A selected column that does not exist is a native PostgREST 400.
call_api missing-column GET "$kong_base/rest/v1/missing_column_probe?select=record_id,absent_column&limit=1" \
	-H "apikey: $api_key" -H "Authorization: Bearer $bearer" || fail "missing-column probe failed"
expect_status missing-column 400 || fail "a missing projected column was not a native HTTP 400"
say "Relation reads: server cap 37, native 400 projection, refused write with row absence"

# Kong's own server header is forwarded (no request transformer).
if ! grep -qi '^server:.*kong/3.9.3' "$logs/relation-ok.headers"; then
	fail "the gateway response does not name Kong 3.9.3"
fi

# ---------------------------------------------------------------------------
# Source-tree integration tests against the owned gateway.
# ---------------------------------------------------------------------------
export SUPABASE_TEST_KONG_URL=$kong_base
export SUPABASE_TEST_API_KEY=$api_key
export SUPABASE_TEST_BEARER=$bearer
export SUPABASE_TEST_EXPIRED_BEARER=$expired_bearer
export SUPABASE_TEST_EMPTY_SUB_BEARER=$empty_sub_bearer
export SUPABASE_TEST_NOACCESS_BEARER=$noaccess_bearer
export SUPABASE_TEST_BAD_API_KEY=$bad_api_key
export SUPABASE_TEST_BAD_BEARER=$bad_signature_bearer

source_log=$logs/go-source.log
say "running TestSupabaseIntegration against the owned fixture (log: $source_log)"
# -v is what makes the --- PASS/--- SKIP lines below real: without it a passing
# or skipping test prints nothing the guard can see.
if ! GOTOOLCHAIN=local go test -race -v -count=1 -run '^TestSupabaseIntegration' ./... >"$source_log" 2>&1; then
	sed -n '1,200p' "$source_log" >&2
	fail "the source-tree Supabase integration tests failed"
fi
if ! grep -q -- '--- PASS: TestSupabaseIntegration_' "$source_log"; then
	sed -n '1,200p' "$source_log" >&2
	fail "no source-tree Supabase integration test passed; the fixture did not reach the tests"
fi
if grep -q -- '--- SKIP: TestSupabaseIntegration_' "$source_log"; then
	fail "a source-tree Supabase integration test skipped"
fi
say "source-tree integration tests passed"

# ---------------------------------------------------------------------------
# Public-module consumer: pinned published SDK, no replace, no working checkout.
# ---------------------------------------------------------------------------
consumer_log=$logs/go-consumer.log
consumer=$work/consumer
cp -R "$consumer_src" "$consumer"
say "running the public-module consumer (log: $consumer_log)"
if ! (
	cd "$consumer" &&
		GOTOOLCHAIN=local go test -race -v -count=1 ./... >"$consumer_log" 2>&1
); then
	sed -n '1,200p' "$consumer_log" >&2
	fail "the public-module consumer failed; if a module is missing from the read-only module cache, ask the supervisor for a prewarm"
fi
if ! grep -q -- '--- PASS: TestPublishedSDK' "$consumer_log"; then
	sed -n '1,200p' "$consumer_log" >&2
	fail "no public-module consumer test passed"
fi
say "public-module consumer passed against the published SDK pin"

# ---------------------------------------------------------------------------
# Installed CLI, read-only. Never updated.
# ---------------------------------------------------------------------------
cli_source="postgrest+http://127.0.0.1:${kong_port}/rest/v1?schema=app&id=record_id"
run_cli() { # NAME [env assignments...] -- ARGS...
	local name=$1
	shift
	local status=0
	DATATUG_POSTGREST_API_KEY=$api_key DATATUG_POSTGREST_BEARER=$bearer \
		"$cli_bin" "$@" >"$logs/cli-${name}.out" 2>"$logs/cli-${name}.err" || status=$?
	printf '%s' "$status" >"$logs/cli-${name}.status"
}

if [ "$skip_installed_cli" = 1 ]; then
	say "installed CLI acceptance: SKIPPED (SUPABASE_SKIP_INSTALLED_CLI=1)."
	say "  The owned fixture, the raw HTTP/RLS premises and the public-module consumer above DID run."
	say "  This run does NOT claim installed-release acceptance."
else
	[ -x "$cli_bin" ] || fail "installed CLI not found or not executable: $cli_bin (set DATATUG_CLI_BIN for a source build)"
	cli_version=$("$cli_bin" version 2>&1) || fail "the CLI version command failed"
	if [ -n "${DATATUG_CLI_BIN:-}" ]; then
		say "CLI acceptance uses a source/CI build at DATATUG_CLI_BIN: $cli_version"
		say "  This is not an installed-release claim."
	else
		case "$cli_version" in
			"datatug $cli_release_version ($cli_release_source)"*) ;;
			*) fail "installed CLI is not the reviewed release: $cli_version" ;;
		esac
		cli_sha=$(sha256_file "$cli_bin") || fail "could not hash the installed CLI"
		[ "$cli_sha" = "$cli_release_sha256" ] ||
			fail "installed CLI SHA-256 is $cli_sha, want $cli_release_sha256"
		say "installed CLI verified: $cli_version, SHA-256 $cli_sha"
	fi

	# A normal, positive-limit --from read through the configured source.
	run_cli from query run --db "$cli_source" --from records --limit 50 --format json --no-policies
	[ "$(<"$logs/cli-from.status")" = 0 ] || {
		sed -n '1,120p' "$logs/cli-from.err" >&2
		fail "the CLI could not read the known relation"
	}
	expect_body cli-from '
import json, sys
rows = json.load(open(sys.argv[1]))
if len(rows) != 50:
    raise SystemExit("CLI --from --limit 50 returned %d rows" % len(rows))
first = rows[0]
if first.get("title") != "Record 1" or first.get("record_id") != 1:
    raise SystemExit("first CLI row is not record 1: %r" % (first,))
if first.get("big_number") != 9007199254740993:
    raise SystemExit("CLI lost the 2^53+1 integer: %r" % (first.get("big_number"),))
' "$logs/cli-from.out" || fail "the CLI rows are not the native seeded rows"

	# The default bound is stated, not silent.
	run_cli from-bounded query run --db "$cli_source" --from records --format json --no-policies
	[ "$(<"$logs/cli-from-bounded.status")" = 0 ] || fail "the default-bounded CLI read failed"
	expect_body cli-from-bounded '
import json, sys
rows = json.load(open(sys.argv[1]))
if len(rows) != 100:
    raise SystemExit("the default-bounded CLI read returned %d rows, want 100" % len(rows))
' "$logs/cli-from-bounded.out" || fail "the default-bounded CLI read is not 100 rows"
	grep -q 'reading at most 100 rows' "$logs/cli-from-bounded.err" ||
		fail "the default-bounded CLI read did not state its bound"

	# TugQL projection: only the declared fields, identity kept for the key.
	cat >"$work/projection.yaml" <<'YAML'
from: {name: records}
columns:
  - {field: title}
  - {field: amount}
orderBy:
  - {field: record_id}
limit: 3
YAML
	run_cli projection query run --db "$cli_source" -f "$work/projection.yaml" --format json --no-policies
	[ "$(<"$logs/cli-projection.status")" = 0 ] || {
		sed -n '1,120p' "$logs/cli-projection.err" >&2
		fail "the CLI TugQL projection failed"
	}
	expect_body cli-projection '
import json, sys
rows = json.load(open(sys.argv[1]))
if len(rows) != 3:
    raise SystemExit("projection returned %d rows" % len(rows))
for row in rows:
    if set(row) != {"title", "amount"}:
        raise SystemExit("projection returned undeclared fields: %r" % (row,))
' "$logs/cli-projection.out" || fail "the CLI projection did not honor the declared columns"

	# A scoped empty subject is a real empty result, not an error.
	if ! DATATUG_POSTGREST_API_KEY=$api_key DATATUG_POSTGREST_BEARER=$empty_sub_bearer \
		"$cli_bin" query run --db "$cli_source" --from records --limit 10 --format json --no-policies \
		>"$logs/cli-empty.out" 2>"$logs/cli-empty.err"; then
		sed -n '1,120p' "$logs/cli-empty.err" >&2
		fail "the CLI empty-subject read failed instead of returning an empty result"
	fi
	expect_body cli-empty '
import json, sys
rows = json.load(open(sys.argv[1]))
if rows:
    raise SystemExit("the empty subject saw CLI rows: %r" % (rows,))
' "$logs/cli-empty.out" || fail "the empty subject saw CLI rows"

	# A role without grants is refused with a safe error and no rows.
	status=0
	DATATUG_POSTGREST_API_KEY=$api_key DATATUG_POSTGREST_BEARER=$noaccess_bearer \
		"$cli_bin" query run --db "$cli_source" --from records --limit 5 --format json --no-policies \
		>"$logs/cli-denied.out" 2>"$logs/cli-denied.err" || status=$?
	[ "$status" != 0 ] || fail "the CLI accepted a read with the noaccess role"
	[ ! -s "$logs/cli-denied.out" ] || fail "the refused CLI read printed rows"
	grep -Eq 'HTTP 40[13]' "$logs/cli-denied.err" || fail "the refused CLI read did not report HTTP 401/403"

	# An expired token is refused with neither the token nor the endpoint in the
	# error text.
	status=0
	DATATUG_POSTGREST_API_KEY=$api_key DATATUG_POSTGREST_BEARER=$expired_bearer \
		"$cli_bin" query run --db "$cli_source" --from records --limit 5 --format json --no-policies \
		>"$logs/cli-expired.out" 2>"$logs/cli-expired.err" || status=$?
	[ "$status" != 0 ] || fail "the CLI accepted an expired token"
	[ ! -s "$logs/cli-expired.out" ] || fail "the refused expired-token read printed rows"
	grep -q 'HTTP 401' "$logs/cli-expired.err" || fail "the expired-token refusal did not report HTTP 401"
	if grep -q -e "$expired_bearer" -e "127.0.0.1:$kong_port" "$logs/cli-expired.err"; then
		fail "the expired-token refusal leaked the token or the endpoint"
	fi

	# Schema refusal: the exact OpenAPI root is forbidden, so a scan must fail
	# instead of claiming an empty schema. The scan is authenticated with the
	# same synthetic key and scoped bearer the raw OpenAPI-root probe above
	# used, so the refusal it observes is the Kong ACL's 403 on the OpenAPI
	# document, not key-auth's missing-key 401.
	#
	# The reviewed CLI collapses the adapter's typed MetadataError into its
	# fixed catalog-read sentence, so the text is asserted as that metadata
	# failure and the credential path is asserted to be absent: the raw 403/401
	# pair above remains the HTTP-status evidence, and this is the CLI-level
	# authenticated metadata-refusal evidence. (The adapter's ListCollections
	# and DescribeCollection return MetadataError/ErrMetadataUnauthorized; the
	# CLI's --from pagingOrder currently swallows a metadata error and simply
	# omits the explicit order, which must not be mistaken for a schema
	# success.)
	export DATATUG_SUPABASE_TEST_URL=$cli_source
	status=0
	DATATUG_POSTGREST_API_KEY=$api_key DATATUG_POSTGREST_BEARER=$bearer \
		"$cli_bin" scan -d "$work/scanned" -D postgrest --dsn-env DATATUG_SUPABASE_TEST_URL \
		--db records --env local >"$logs/cli-scan.out" 2>"$logs/cli-scan.err" || status=$?
	[ "$status" != 0 ] || fail "the CLI scan accepted a forbidden OpenAPI root as a schema"
	grep -q 'failed to get dbCatalog metadata' "$logs/cli-scan.err" ||
		fail "the refused CLI scan did not report a catalog metadata failure"
	grep -q 'the catalog could not be read' "$logs/cli-scan.err" ||
		fail "the refused CLI scan did not report the metadata read refusal"
	if grep -q 'DATATUG_POSTGREST_API_KEY' "$logs/cli-scan.err"; then
		fail "the refused CLI scan failed on credential validation, not the metadata refusal"
	fi
	if find "$work/scanned" -name '*.db.json' -print 2>/dev/null | grep -q .; then
		fail "the refused CLI scan still wrote a catalog descriptor"
	fi
	if grep -q -e "$bearer" -e "127.0.0.1:$kong_port" "$logs/cli-scan.err"; then
		fail "the schema refusal leaked a token or the endpoint"
	fi
	say "installed CLI: --from, default bound, TugQL projection, empty/denied/expired and schema refusal passed"
fi

# ---------------------------------------------------------------------------
# Exact-owned teardown, then proof the resources are gone.
# ---------------------------------------------------------------------------
trap - EXIT INT TERM
say "tearing down Compose project $project"
if ! supabase_lifecycle_teardown; then
	fail "teardown failed; the owned resources may remain and are reported above"
fi
if ! supabase_resources_absent; then
	fail "teardown left containers, volumes or networks for project $project"
fi
if [ -d "$work" ]; then
	fail "the private work directory was not removed: $work"
fi
say "Teardown complete: no container, volume or network remains for project $project"
say "Logs retained at: $logs"
say "PASS: Supabase-style REST fixture acceptance completed"
