#!/usr/bin/env bash
# The arguments are the policy path, lock path, lock wait seconds,
# marker line, and command to run, in that order.
# The script exits with the command exit status.
# It exits with status 70 for a setup failure, 73 for a foreign
# policy file, and 75 for a lock timeout.

readonly EXIT_SETUP_FAILED=70
readonly EXIT_FOREIGN_POLICY=73
readonly EXIT_LOCK_TIMEOUT=75
readonly POLICY_MODE=0755

policy_path=$1
lock_path=$2
lock_wait_seconds=$3
marker_line=$4
shift 4

remove_policy() {
	rm -f -- "$policy_path"
}

is_marked_policy() {
	[[ -f $policy_path && ! -L $policy_path ]] && grep -qxF -- "$marker_line" "$policy_path"
}

if ! exec 9>"$lock_path"; then
	exit "$EXIT_SETUP_FAILED"
fi

if ! flock -w "$lock_wait_seconds" 9; then
	printf 'The wait for lock %s timed out after %s seconds.\n' "$lock_path" "$lock_wait_seconds" >&2
	exit "$EXIT_LOCK_TIMEOUT"
fi

if [[ -e $policy_path || -L $policy_path ]]; then
	if ! is_marked_policy; then
		printf '%s exists without the pveguest marker. The script will not change it.\n' "$policy_path" >&2
		exit "$EXIT_FOREIGN_POLICY"
	fi
	# The script owns the exclusive lock.
	# A running install does not own the policy-rc.d file with the pveguest marker.
	remove_policy
fi

if ! (
	set -C
	printf '%s\n' '#!/bin/sh' "$marker_line" 'exit 101' >"$policy_path"
); then
	printf 'The script could not create the policy file %s.\n' "$policy_path" >&2
	exit "$EXIT_SETUP_FAILED"
fi

# Delete the policy file when the script exits or receives a
# hangup, interrupt, or termination signal.
trap remove_policy EXIT
trap 'exit 143' HUP INT TERM

if ! chmod "$POLICY_MODE" -- "$policy_path"; then
	printf 'The script could not create the policy file %s.\n' "$policy_path" >&2
	exit "$EXIT_SETUP_FAILED"
fi

# Close file descriptor 9 so a daemon started by a package script
# does not inherit the lock.
"$@" 9>&-
exit "$?"
