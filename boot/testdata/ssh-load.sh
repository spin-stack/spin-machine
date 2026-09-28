#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Times SSH logins while the guest is loaded, in a disposable test guest. Runs once, as root.
set -eu
trap 'journalctl -b -u ssh.service --no-pager | tail -20; echo SSHLOAD_FAILED' EXIT

ip link set lo up
ssh-keygen -q -t ed25519 -N '' -f /run/sshload-key
install -d -m 700 -o spin -g spin /home/spin/.ssh
install -m 600 -o spin -g spin /run/sshload-key.pub /home/spin/.ssh/authorized_keys
systemctl start ssh.socket

SSH="ssh -F /dev/null -i /run/sshload-key -o BatchMode=yes -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null -o LogLevel=error -o ConnectTimeout=10 spin@127.0.0.1"

# One login, in ms, or FAIL. The client runs in a slice that outweighs everything else in the
# guest, so what is timed is the server: a client starved by the same load would be measured
# instead of sshd.
login() {
    systemd-run --scope --quiet --slice=sshload-probe.slice -p CPUWeight=10000 -p IOWeight=10000 \
        sh -c "t0=\$(date +%s%N)
               if timeout 20 $SSH true </dev/null; then echo \$(( (\$(date +%s%N) - t0) / 1000000 ))
               else echo FAIL; fi"
}

# The logins, and the pressure the guest was under while they ran: a case whose load did not
# happen would otherwise read as a load sshd shrugged off.
measure() {
    out=
    for _ in $(seq "${SSHLOAD_LOGINS:-10}"); do out="$out $(login)"; done
    echo "SSHLOAD $1$out"
    # Runnable tasks now, from /proc/loadavg: the 1-minute average is still near zero a few
    # seconds into a load, which is all a case lasts. PSI's avg10 besides, from a kernel that
    # has it: the share of the last ten seconds some task waited for a CPU, or every task for
    # memory.
    psi=
    if [ -r /proc/pressure/cpu ]; then
        psi=" cpu-some=$(awk '/^some/ { sub("avg10=", "", $2); print $2 }' /proc/pressure/cpu)%"
        psi="$psi memory-full=$(awk '/^full/ { sub("avg10=", "", $2); print $2 }' /proc/pressure/memory)%"
    fi
    echo "SSHLOADPSI $1 runnable=$(cut -d' ' -f4 /proc/loadavg | cut -d/ -f1)" \
        "available=$(awk '/^MemAvailable:/ { print int($2 / 1024) }' /proc/meminfo)MiB$psi"
}

# Eight busy loops per vCPU: more runnable threads than the machine has, which is what a
# parallel build looks like to the scheduler.
hog="i=0; while [ \$i -lt $(( $(nproc) * 8 )) ]; do (while :; do :; done) & i=\$((i+1)); done; wait"

measure idle

# The load in a service of its own, beside sshd in system.slice: a container, a daemon.
systemd-run --quiet --unit=sshload-hog sh -c "$hog"
sleep 1
systemctl is-active --quiet sshload-hog.service
measure cpu-service
systemctl kill -s KILL sshload-hog.service 2>/dev/null || true

# The same load inside a login session, which is where a user's build runs.
$SSH "nohup sh -c '$hog' >/dev/null 2>&1 &" </dev/null
sleep 1
measure cpu-session
pkill -KILL -u spin || true
sleep 1

# Memory: tmpfs filled until 64, then 16 MiB is left available. With no swap, what the kernel
# can still reclaim is page cache, the binaries' among it - sshd-session and sshd-auth are
# exec'd for every connection.
mkdir -p /run/sshload-fill
# Sized to all of memory: tmpfs defaults to half of it, which stopped the fill at 312 MiB left.
mount -t tmpfs -o size="$(awk '/^MemTotal:/ { print $2 }' /proc/meminfo)k" none /run/sshload-fill
n=0
for left in 64 16; do
    avail=$(awk '/^MemAvailable:/ { print int($2 / 1024) }' /proc/meminfo)
    n=$((n + 1))
    dd if=/dev/zero of=/run/sshload-fill/f$n bs=1M count=$((avail - left)) 2>/dev/null || true
    measure memory-$left
done

trap - EXIT
echo SSHLOAD_DONE
