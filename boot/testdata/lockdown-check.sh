#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
set -eu
trap 'echo LOCKDOWN_CHECK_FAILED' EXIT

# The LSMs the kernel runs, in its own words, and the lockdown level it enforces: the brackets
# mark the one in force.
echo "lsm: $(cat /sys/kernel/security/lsm)"
echo "lockdown: $(cat /sys/kernel/security/lockdown)"
# capability is always there, whatever CONFIG_LSM says: the kernel puts it first among the rest.
test "$(cat /sys/kernel/security/lsm)" = "lockdown,capability,bpf"
grep -q '\[confidentiality\]' /sys/kernel/security/lockdown

# Root cannot read the kernel's memory, nor lower the level.
test ! -e /dev/mem
test ! -e /proc/kcore
if echo none > /sys/kernel/security/lockdown 2>/dev/null; then
    echo "root lowered lockdown"
    exit 1
fi
grep -q '\[confidentiality\]' /sys/kernel/security/lockdown

trap - EXIT
echo LOCKDOWN_CHECK_OK
