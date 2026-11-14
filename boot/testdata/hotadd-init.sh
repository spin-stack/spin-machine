#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# PID 1 until a second core has been attached, then systemd: a consumer's init that is given
# its cores before it hands over, with no udev running to see them arrive.
mount -t sysfs sysfs /sys
echo HOTADD_WAITING > /dev/console
until [ -e /sys/devices/system/cpu/cpu1 ]; do sleep 0.05; done
exec /sbin/init
