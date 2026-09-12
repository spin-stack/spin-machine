#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Runs as spin, through sshd's real PAM stack, in a disposable test guest.
set -eux
test "$(id -u)" = 1000
test "${XDG_RUNTIME_DIR:-}" = /run/user/1000
test "$(stat -c '%u:%a' "$XDG_RUNTIME_DIR")" = 1000:700
test -n "${XDG_SESSION_ID:-}"
test "$(loginctl show-session "$XDG_SESSION_ID" -p Name --value)" = spin
systemctl --user list-units --no-pager >/dev/null
sudo -n true
printf 'SESSION_OK %s\n' "$XDG_SESSION_ID"
