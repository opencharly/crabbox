#!/bin/sh
# Runs INSIDE the krun guest as a systemd oneshot. Its stdout is the guest
# console, which crun connects to the container's stdio, so every line below
# is what `nerdctl logs` returns on the host.
set -u

printf '%s\n' "KRUN_PROOF step=guest-boot"
printf '%s\n' "KRUN_PROOF pid1_comm=$(cat /proc/1/comm)"
printf '%s\n' "KRUN_PROOF pid1_cmdline=$(tr '\0' ' ' </proc/1/cmdline)"
printf '%s\n' "KRUN_PROOF uname=$(uname -a)"
printf '%s\n' "KRUN_PROOF kernel=$(uname -r)"
printf '%s\n' "KRUN_PROOF os=$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"')"
printf '%s\n' "KRUN_PROOF nofile_soft=$(ulimit -Sn) nofile_hard=$(ulimit -Hn)"
printf '%s\n' "KRUN_PROOF state=$(cat /proc/1/status | sed -n 's/^State:[[:space:]]*//p')"

# The stop is an init-level poweroff, never a signal from the host.
printf '%s\n' "KRUN_PROOF stop=systemctl-poweroff"
systemctl poweroff
