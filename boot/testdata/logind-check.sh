#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
set -eu
trap 'cat /run/logind-session-result 2>/dev/null || true; journalctl -b -u ssh.service -u systemd-logind.service --no-pager; echo LOGIND_CHECK_FAILED' EXIT

# Query PID1, not loginctl: asking login1 would itself activate logind.
test "$(systemctl show systemd-logind.service -p ActiveState --value)" = "$LOGIND_EXPECT"
systemctl is-active --quiet systemd-logind-varlink.socket
echo "LOGIND_INITIAL_STATE_OK $LOGIND_EXPECT"

# Every login is PAM, not only sshd's: the console getty and su go through the same stack.
# Asserted separately from the SSH logins below because a configuration that starts logind
# on sshd's behalf passes those and fails this one (2026-09-12).
test "$(su -l spin -c 'echo ${XDG_RUNTIME_DIR:-unset}' </dev/null | tr -d '\r')" = /run/user/1000

ip link set lo up
ssh-keygen -q -t ed25519 -N '' -f /run/logind-test-key
install -d -m 700 -o spin -g spin /home/spin/.ssh
install -m 600 -o spin -g spin /run/logind-test-key.pub /home/spin/.ssh/authorized_keys
systemctl start ssh.socket

# Test the first login and a reconnect, including a PTY session. Credentials and
# known_hosts exist only in this throwaway guest; no host networking is involved.
previous_session=
for tty in -T -tt; do
    ssh -F /dev/null "$tty" -i /run/logind-test-key \
        -o BatchMode=yes -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/run/logind-known-hosts \
        spin@127.0.0.1 /bin/sh /logind-session.sh > /run/logind-session-result
    cat /run/logind-session-result
    grep -q '^SESSION_OK ' /run/logind-session-result
    systemctl is-active --quiet systemd-logind.service
    session=$(sed -n 's/^SESSION_OK //p' /run/logind-session-result | tr -d '\r')
    test "$session" != "$previous_session"
    previous_session=$session
    # The session scope must finish after logout, even if the user's service
    # manager stays alive for its configured stop delay.
    for attempt in $(seq 50); do
        state=$(systemctl show "session-$session.scope" -p ActiveState --value)
        test "$state" != active && break
        sleep 0.1
    done
    test "$state" != active
done
trap - EXIT
echo LOGIND_CHECK_OK
