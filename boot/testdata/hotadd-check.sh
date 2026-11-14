#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
set -eu
trap 'echo HOTADD_CHECK_FAILED' EXIT

echo "present: $(cat /sys/devices/system/cpu/present) online: $(cat /sys/devices/system/cpu/online)"
test "$(cat /sys/devices/system/cpu/present)" = "0-1"
test "$(cat /sys/devices/system/cpu/online)" = "0-1"

trap - EXIT
echo HOTADD_CHECK_OK
