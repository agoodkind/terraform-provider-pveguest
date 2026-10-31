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
readonly DPKG_LOCK_PATHS=(/var/lib/dpkg/lock-frontend /var/lib/dpkg/lock)
readonly PROC_LOCKS_PATH=/proc/locks
readonly POLL_SECONDS=1
readonly SIGNAL_STATUS_BASE=128
readonly APT_CONFIG_DIRECTORY=/run
readonly APT_CONFIG_PREFIX=pveguest-apt-policy.
readonly APT_CONFIG_SUFFIX=.conf

policy_path=$1
lock_path=$2
lock_wait_seconds=$3
marker_line=$4
shift 4

marker_text=${marker_line#"# "}
staged_prefix="${policy_path}.pveguest-staged."
staged_path="${staged_prefix}$$"
apt_config_path="$APT_CONFIG_DIRECTORY/$APT_CONFIG_PREFIX$$$APT_CONFIG_SUFFIX"

remove_policy() {
	rm -f -- "$policy_path"
}

# Delete the policy file if it is a hard link to the staged file.
# Delete the staged file and the temporary apt configuration.
cleanup() {
	local exit_status=$?
	if [[ $policy_path -ef $staged_path ]]; then
		if ((exit_status > SIGNAL_STATUS_BASE)); then
			wait_for_dpkg_locks
		fi
		remove_policy
	fi
	rm -f -- "$staged_path" "$apt_config_path"
}

# wait_for_dpkg_locks waits up to the lock wait limit after a signal exit.
# A surviving dpkg process owns the dpkg lock while its maintainer scripts run.
# The script deletes the policy file only after processes release the dpkg locks.
wait_for_dpkg_locks() {
	local deadline=$((SECONDS + lock_wait_seconds))
	while dpkg_lock_is_held; do
		if ((SECONDS >= deadline)); then
			return 0
		fi
		sleep "$POLL_SECONDS"
	done
}

is_marked_policy() {
	[[ -f $policy_path && ! -L $policy_path ]] && grep -qxF -- "$marker_line" "$policy_path"
}

# dpkg_lock_is_held reads /proc/locks and matches the lock file's
# hexadecimal device major and minor numbers and inode to check
# whether a process owns the lock.
dpkg_lock_is_held() {
	local lock_file device_major device_minor inode lock_key
	for lock_file in "${DPKG_LOCK_PATHS[@]}"; do
		[[ -e $lock_file ]] || continue
		read -r device_major device_minor inode < <(stat -c '%Hd %Ld %i' -- "$lock_file") || continue
		lock_key=$(printf '%02x:%02x:%d' "$device_major" "$device_minor" "$inode")
		if grep -qF -- " ${lock_key} " "$PROC_LOCKS_PATH"; then
			return 0
		fi
	done
	return 1
}

# After acquiring the install lock, remove marked staged and apt configuration
# files for process IDs that no longer run.
remove_stale_owned_files() {
	local file suffix
	for file in "${staged_prefix}"*; do
		suffix=${file#"$staged_prefix"}
		remove_if_stale "$file" "$suffix" "$marker_line"
	done
	for file in "$APT_CONFIG_DIRECTORY/$APT_CONFIG_PREFIX"*"$APT_CONFIG_SUFFIX"; do
		suffix=${file#"$APT_CONFIG_DIRECTORY/$APT_CONFIG_PREFIX"}
		suffix=${suffix%"$APT_CONFIG_SUFFIX"}
		remove_if_stale "$file" "$suffix" "// $marker_text"
	done
}

# files_in_use returns success for the current script or a pid with a /proc entry.
# It also returns success when an exclusive, non-blocking flock on that pid's
# apt configuration file fails.
# An apt-get process that survives a dead script keeps this lock until it ends.
files_in_use() {
	local pid=$1
	local config_file="$APT_CONFIG_DIRECTORY/$APT_CONFIG_PREFIX$pid$APT_CONFIG_SUFFIX"
	if [[ $pid -eq $$ || -d /proc/$pid ]]; then
		return 0
	fi
	if [[ ! -f $config_file || -L $config_file ]]; then
		return 1
	fi
	if flock -x -n "$config_file" true; then
		return 1
	fi
	return 0
}

# Skip links, names without digits, and files that files_in_use reports.
# Delete the remaining file only when it contains the expected marker line.
remove_if_stale() {
	local file=$1 pid=$2 expected_line=$3
	[[ $pid =~ ^[0-9]+$ ]] || return 0
	[[ -f $file && ! -L $file ]] || return 0
	if files_in_use "$pid"; then
		return 0
	fi
	if ! grep -qxF -- "$expected_line" "$file"; then
		printf 'File %s matches a pveguest file name but lacks the marker. The script leaves it in place.\n' "$file" >&2
		return 0
	fi
	rm -f -- "$file"
}

if ! exec 9>"$lock_path"; then
	exit "$EXIT_SETUP_FAILED"
fi

if ! flock -w "$lock_wait_seconds" 9; then
	printf 'The wait for lock %s timed out after %s seconds.\n' "$lock_path" "$lock_wait_seconds" >&2
	exit "$EXIT_LOCK_TIMEOUT"
fi

remove_stale_owned_files
if [[ -e $policy_path || -L $policy_path ]]; then
	if ! is_marked_policy; then
		printf '%s exists without the pveguest marker. The script will not change it.\n' "$policy_path" >&2
		exit "$EXIT_FOREIGN_POLICY"
	fi
	# Wait for both dpkg locks to become free before deleting the marked policy file.
	# An earlier install may still need the file.
	deadline=$((SECONDS + lock_wait_seconds))
	while dpkg_lock_is_held; do
		if ((SECONDS >= deadline)); then
			printf 'Timed out after %s seconds while waiting for the dpkg locks.\n' "$lock_wait_seconds" >&2
			exit "$EXIT_LOCK_TIMEOUT"
		fi
		sleep "$POLL_SECONDS"
	done
	remove_policy
fi

trap cleanup EXIT
trap 'exit 143' HUP INT TERM

if ! (
	set -C
	printf '%s\n' '#!/bin/sh' "$marker_line" 'exit 101' >"$staged_path"
); then
	printf 'The script could not create the policy file %s.\n' "$staged_path" >&2
	exit "$EXIT_SETUP_FAILED"
fi
if ! chmod "$POLICY_MODE" -- "$staged_path"; then
	printf 'The script could not create the policy file %s.\n' "$staged_path" >&2
	exit "$EXIT_SETUP_FAILED"
fi

# The pre-invoke hook links the staged file before dpkg runs.
# The link fails if the policy file exists, and apt-get stops before dpkg runs.
# The post-invoke hook deletes the policy file after dpkg finishes.
# apt-get owns both dpkg locks while it runs these hooks.
if ! printf '// %s\nDPkg::Pre-Invoke:: "ln -T -- %s %s";\nDPkg::Post-Invoke:: "rm -f -- %s";\n' \
	"$marker_text" "$staged_path" "$policy_path" "$policy_path" >"$apt_config_path"; then
	printf 'The script could not create the apt configuration %s.\n' "$apt_config_path" >&2
	exit "$EXIT_SETUP_FAILED"
fi
export APT_CONFIG="$apt_config_path"

# The script opens the apt configuration file on descriptor 8 and takes a shared flock.
# apt-get inherits descriptor 8 and keeps the lock while waiting for the dpkg lock.
# apt-get marks descriptors above 2 as close-on-exec before starting dpkg.
# dpkg and its maintainer scripts do not inherit descriptor 8.
if ! exec 8<"$apt_config_path" || ! flock -s -n 8; then
	printf 'The script could not create the apt configuration %s.\n' "$apt_config_path" >&2
	exit "$EXIT_SETUP_FAILED"
fi

# Close file descriptor 9 so a daemon started by a package script
# does not inherit the lock.
"$@" 9>&-
exit "$?"
