#!/usr/bin/env bash
# test-supabase-lifecycle.sh regression-tests scripts/lib/supabase-lifecycle.sh,
# the teardown, trap and ownership code scripts/check-supabase-rest.sh actually
# uses. Every scenario runs the real functions with a docker stub, so it proves
# the shipped cleanup, not a copy:
#   - a failing body stays failed when teardown succeeds;
#   - a failing body keeps its own status when teardown also fails;
#   - a passing body fails when teardown fails;
#   - a passing body passes when teardown succeeds, removing its work dir;
#   - a body that never started a project invokes no docker command;
#   - a project outside the owned prefix is refused without a docker call;
#   - INT and TERM exit non-zero and still tear down exactly once;
#   - the ownership probe reports absence only for an exact-label miss.
set -euo pipefail
# work, started and compose are read by the lifecycle functions each scenario
# sources below, and that source path is this checkout's, not an input.
# shellcheck disable=SC2034,SC1091
cd "$(dirname "$0")"
helper=$PWD/lib/supabase-lifecycle.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/dalgo2postgrest-supabase-lifecycle-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

checks=0
failures=0
check() {
	checks=$((checks + 1))
	if ! "$@"; then
		failures=$((failures + 1))
	fi
}
expect_equal() {
	local name=$1 want=$2 got=$3
	if [ "$got" != "$want" ]; then
		echo "not ok: $name: got $got, want $want" >&2
		return 1
	fi
	echo "ok: $name"
}
expect_contains() {
	local name=$1 text=$2 haystack=$3
	case "$haystack" in
		*"$text"*) echo "ok: $name" ;;
		*) echo "not ok: $name: missing $text in $haystack" >&2; return 1 ;;
	esac
}

# run_cleanup_case NAME EXPECTED_STATUS DOCKER_STATUS STARTED BODY runs the real
# lifecycle in a subshell and leaves its docker log, stderr and work dir behind.
run_cleanup_case() {
	local case_name=$1 want=$2 docker_status=$3 started_flag=$4 body=$5
	local dir=$tmp/$case_name got=0
	mkdir -p "$dir/work"
	(
		set -euo pipefail
		work=$dir/work
		project=dalgo2postgrest-supabase-lifecycle-test
		started=$started_flag
		compose=(docker compose -p "$project")
		# The helper invokes this stub indirectly through compose.
		# shellcheck disable=SC2329,SC2317
		docker() {
			printf '%s\n' "$*" >>"$dir/docker.log"
			return "$docker_status"
		}
		# shellcheck source=scripts/lib/supabase-lifecycle.sh
		. "$helper"
		supabase_lifecycle_install
		eval "$body"
		exit 0
	) >/dev/null 2>"$dir/stderr" || got=$?
	check expect_equal "$case_name exits" "$want" "$got"
}

run_cleanup_case failing-body-kept 7 0 1 'exit 7'
check expect_equal failing-body-kept-teardown-ran 1 "$(wc -l <"$tmp/failing-body-kept/docker.log" | tr -d ' ')"
check expect_equal failing-body-kept-work-removed yes "$([ -e "$tmp/failing-body-kept/work" ] && echo no || echo yes)"

run_cleanup_case failing-body-beats-cleanup-failure 7 5 1 'exit 7'
check expect_contains failing-body-beats-cleanup-failure-message \
	"could not remove the owned Compose project dalgo2postgrest-supabase-lifecycle-test" \
	"$(cat "$tmp/failing-body-beats-cleanup-failure/stderr")"

run_cleanup_case passing-body-cleanup-failure 1 5 1 ':'

run_cleanup_case passing-body-cleanup-success 0 0 1 ':'
check expect_equal passing-body-cleanup-success-teardown-ran 1 "$(wc -l <"$tmp/passing-body-cleanup-success/docker.log" | tr -d ' ')"
check expect_contains passing-body-cleanup-success-exact-command \
	"compose -p dalgo2postgrest-supabase-lifecycle-test down -v --remove-orphans" \
	"$(cat "$tmp/passing-body-cleanup-success/docker.log")"
check expect_equal passing-body-cleanup-success-work-removed yes \
	"$([ -e "$tmp/passing-body-cleanup-success/work" ] && echo no || echo yes)"

run_cleanup_case never-started-removes-nothing 0 9 0 ':'
check expect_equal never-started-removes-nothing-no-docker yes \
	"$([ -e "$tmp/never-started-removes-nothing/docker.log" ] && echo no || echo yes)"

# A name outside the owned prefix must be refused without a docker invocation.
foreign=$tmp/foreign-project-refused
mkdir -p "$foreign/work"
got=0
(
	set -euo pipefail
	# This negative scenario deliberately owns its lifecycle state only in this
	# subshell. The parent must not inherit any work/started/compose mutation.
	# shellcheck disable=SC2030
	work=$foreign/work
	project=some-other-workers-project
	# shellcheck disable=SC2030
	started=1
	# shellcheck disable=SC2030
	compose=(docker compose -p "$project")
	# shellcheck disable=SC2329,SC2317
	docker() {
		printf '%s\n' "$*" >>"$foreign/docker.log"
		return 0
	}
	# shellcheck source=scripts/lib/supabase-lifecycle.sh
	. "$helper"
	supabase_lifecycle_install
	exit 0
) >/dev/null 2>"$foreign/stderr" || got=$?
check expect_equal foreign-project-refused-exits 1 "$got"
check expect_equal foreign-project-refused-no-docker yes "$([ -e "$foreign/docker.log" ] && echo no || echo yes)"
check expect_contains foreign-project-refused-message "outside this run's owned prefix" "$(cat "$foreign/stderr")"

# The ownership probe must distinguish "no exact-label resource" from a docker
# query failure, and must print what remains.
probe_case() {
	local case_name=$1 want=$2 fixture_containers=$3 fixture_volumes=$4 fixture_networks=$5 docker_status=$6
	local dir=$tmp/probe-$case_name got=0
	mkdir -p "$dir"
	(
		set -euo pipefail
		project=dalgo2postgrest-supabase-probe
		# shellcheck disable=SC2329
		docker() {
			[ "$docker_status" = 0 ] || return "$docker_status"
			case "$*" in
				*"ps -aq"*) printf '%s' "$fixture_containers" ;;
				*"volume ls -q"*) printf '%s' "$fixture_volumes" ;;
				*"network ls -q"*) printf '%s' "$fixture_networks" ;;
				*) return 9 ;;
			esac
		}
		# shellcheck source=scripts/lib/supabase-lifecycle.sh
		. "$helper"
		supabase_resources_absent
		exit "$?"
	) >/dev/null 2>"$dir/stderr" || got=$?
	check expect_equal "probe-$case_name exits" "$want" "$got"
}

probe_case absent 0 '' '' '' 0
probe_case container-remains 1 'abc123' '' '' 0
probe_case volume-remains 1 '' 'vol1' '' 0
probe_case network-remains 1 '' '' 'net1' 0
probe_case docker-failure 2 '' '' '' 7
check expect_contains probe-container-remains-message "containers=[abc123]" "$(cat "$tmp/probe-container-remains/stderr")"

# signal_case SIGNAL EXPECTED_STATUS runs the real lifecycle in a foreground
# child process (a background job ignores INT in a non-interactive shell), has
# that child signal itself once its traps are installed, and checks the exact
# status, the single teardown and the message.
signal_case() {
	local signal=$1 want=$2 case_name="interrupt-$1" dir=$tmp/"interrupt-$1"
	mkdir -p "$dir/work"
	cat >"$dir/case.sh" <<'CHILD'
# shellcheck disable=SC2034,SC1091,SC2329
set -euo pipefail
dir=$1 helper=$2 signal=$3
work=$dir/work
project=dalgo2postgrest-supabase-signal-test
started=1
compose=(docker compose -p "$project")
docker() {
	printf '%s\n' "$*" >>"$dir/docker.log"
	return 0
}
# shellcheck source=scripts/lib/supabase-lifecycle.sh
. "$helper"
supabase_lifecycle_install
( sleep 0.2; kill -"$signal" $$ ) &
wait "$!"
CHILD
	local got=0
	bash "$dir/case.sh" "$dir" "$helper" "$signal" >/dev/null 2>"$dir/stderr" || got=$?
	check expect_equal "$case_name exits" "$want" "$got"
	check expect_equal "$case_name-teardown-ran-once" 1 \
		"$([ -e "$dir/docker.log" ] && wc -l <"$dir/docker.log" | tr -d ' ' || echo 0)"
	check expect_contains "$case_name-message" "interrupted ($signal)" "$(cat "$dir/stderr")"
}

signal_case INT 130
signal_case TERM 143

echo "supabase fixture lifecycle: ${checks} checks, ${failures} failures"
if [ "$failures" != 0 ]; then
	exit 1
fi
