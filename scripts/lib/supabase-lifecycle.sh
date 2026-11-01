# shellcheck shell=bash
# supabase-lifecycle.sh owns the teardown, trap wiring and ownership proof of
# the disposable Supabase-style fixture. scripts/check-supabase-rest.sh sources
# it, and scripts/test-supabase-lifecycle.sh regression-tests these exact
# functions with a docker stub.
#
# The sourcing script sets, before calling supabase_lifecycle_install:
#   project   the exact Compose project name this run alone owns
#   work      the scratch directory this run alone created
#   compose   array: docker compose bound to that project and its compose file
#   started   0 until this run begins creating its project, 1 afterwards
#
# Only the project this run started is removed, by its exact name and label.
# Nothing here prunes by prefix, removes a shared port, kills another worker's
# container, or runs a global docker cleanup.

# The prefix is part of the ownership rule: a project name outside it is never
# removed even if a caller set `project` to it.
supabase_project_prefix=${supabase_project_prefix:-dalgo2postgrest-supabase-}

supabase_lifecycle_teardown() {
	local removed=0
	if [ "${started:-0}" = 1 ]; then
		case "${project:-}" in
			"${supabase_project_prefix}"*) ;;
			*)
				echo "refusing to remove Compose project '${project:-<unset>}': it is outside this run's owned prefix ${supabase_project_prefix}" >&2
				return 1
				;;
		esac
		# compose is the array the sourcing script assigned before installing.
		# shellcheck disable=SC2154
		if [ -n "${work:-}" ] && [ -d "${work}" ]; then
			"${compose[@]}" down -v --remove-orphans >"${work}/teardown.log" 2>&1
			removed=$?
		else
			"${compose[@]}" down -v --remove-orphans >/dev/null 2>&1
			removed=$?
		fi
		if [ "$removed" != 0 ]; then
			echo "could not remove the owned Compose project ${project:-<unset>} (docker compose down exited $removed); see ${work:-<no work dir>}/teardown.log" >&2
			return 1
		fi
	fi
	if [ -n "${work:-}" ]; then
		rm -rf "$work" >/dev/null 2>&1 || true
	fi
	return 0
}

# supabase_lifecycle_cleanup preserves the body's exit status. A teardown
# failure turns a passing body into a failure; it never overwrites a failing
# body's status, and the failure is still reported.
supabase_lifecycle_cleanup() {
	local status=$? torn=0
	trap - EXIT INT TERM
	set +e
	supabase_lifecycle_teardown
	torn=$?
	set -e
	if [ "$status" = 0 ] && [ "$torn" != 0 ]; then
		status=$torn
	fi
	exit "$status"
}

# supabase_lifecycle_signal exits with the conventional signal status; the EXIT
# trap still performs the one teardown.
supabase_lifecycle_signal() {
	local name=$1 code=$2
	trap - INT TERM
	echo "interrupted ($name); removing the owned fixture" >&2
	exit "$code"
}

supabase_lifecycle_install() {
	trap supabase_lifecycle_cleanup EXIT
	trap 'supabase_lifecycle_signal INT 130' INT
	trap 'supabase_lifecycle_signal TERM 143' TERM
}

# supabase_resources_absent proves that no container, volume or network with
# this exact Compose project's label remains. It queries the exact label value,
# never a prefix; a docker query failure returns 2 and is not reported as
# "absent".
supabase_resources_absent() {
	local label="label=com.docker.compose.project=${project:-}"
	local containers volumes networks
	containers=$(docker ps -aq --filter "$label") || return 2
	volumes=$(docker volume ls -q --filter "$label") || return 2
	networks=$(docker network ls -q --filter "$label") || return 2
	if [ -n "$containers" ] || [ -n "$volumes" ] || [ -n "$networks" ]; then
		echo "owned resources remain for Compose project ${project:-<unset>}: containers=[${containers}] volumes=[${volumes}] networks=[${networks}]" >&2
		return 1
	fi
	return 0
}
