#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# The first SSH login into a guest that has just booted, for task boot:jobs: jobwatch records
# the jobs it sets off - sshd through ssh.socket, logind, the user's manager. Diagnostic, in a
# disposable test guest; runs once, as root, after spin-waits.service has written its dump.
set -eu

ssh-keygen -q -t ed25519 -N '' -f /run/first-login-key
install -d -m 700 -o spin -g spin /home/spin/.ssh
install -m 600 -o spin -g spin /run/first-login-key.pub /home/spin/.ssh/authorized_keys

# One login, in ms, or FAIL.
login() {
    t0=$(date +%s%N)
    if timeout 30 ssh -F /dev/null -i /run/first-login-key -o BatchMode=yes \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=error \
        spin@127.0.0.1 true </dev/null; then
        echo $(( ($(date +%s%N) - t0) / 1000000 ))
    else
        echo FAIL
    fi
}

# The first, a second right after it, and a third with the page cache dropped: the second is
# what any login costs, and how close the third comes back to the first says how much of the
# first was reading sshd, PAM and their libraries from disk for the first time.
first=$(login)
again=$(login)
sync
echo 3 >/proc/sys/vm/drop_caches
cold=$(login)
echo "SPIN-LOGIN $first again=$again cold=$cold" >/dev/ttyS0
