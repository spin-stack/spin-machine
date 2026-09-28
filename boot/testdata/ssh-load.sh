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

cpu_service() {
    # The load in a service of its own, beside sshd in system.slice: a container, a daemon.
    systemd-run --quiet --unit=sshload-hog sh -c "$hog"
    sleep 1
    systemctl is-active --quiet sshload-hog.service
    measure cpu-service
    systemctl kill -s KILL sshload-hog.service 2>/dev/null || true
}

cpu_session() {
    # The same load inside a login session, which is where a user's build runs.
    $SSH "nohup sh -c '$hog' >/dev/null 2>&1 &" </dev/null
    sleep 1
    measure cpu-session
    pkill -KILL -u spin || true
    sleep 1
}

tmp_full() {
    # /tmp filled until it refuses. It is a tmpfs, and what a tmpfs holds belongs to no process,
    # so the OOM killer cannot take it back: with /tmp at systemd's half of memory and MGLRU's
    # min_ttl_ms set, filling it had the OOM killer take the console's getty and this script
    # (2026-09-28). The image caps it at a quarter, and this is the check that the cap is what
    # stops the fill: "No space left on device", logins as usual, and nothing killed.
    kills=$(journalctl -k -b --no-pager | grep -c 'Out of memory: Killed' || true)
    if dd if=/dev/zero of=/tmp/sshload-fill bs=1M 2>/run/sshload-dd; then
        echo "tmp_full: /tmp took everything dd had and never filled" >&2
        return 1
    fi
    grep -q 'No space left on device' /run/sshload-dd
    measure tmp-full
    echo "SSHLOADPSI tmp-full-oom kills=$(( $(journalctl -k -b --no-pager | grep -c 'Out of memory: Killed' || true) - kills ))" \
        "tmp=$(df -m --output=size,used /tmp | tail -1 | tr -s ' ')MiB"
    rm -f /tmp/sshload-fill
}

# The logins while a process in spin's session takes memory until something stops it, then how
# long that took, who stopped it and whether the machine answers afterwards. Last in a run,
# because the guest may not come out of it.
grow() {
    label=$1 hog=$2
    $SSH "nohup sh -c '$hog' >/dev/null 2>&1 &" </dev/null
    start=$(date +%s%N)
    out= gone=
    while [ $(( ($(date +%s%N) - start) / 1000000000 )) -lt 90 ]; do
        out="$out $(login)"
        if ! pgrep -u spin -x tail >/dev/null; then
            gone=$(( ($(date +%s%N) - start) / 1000000 ))
            break
        fi
    done
    echo "SSHLOAD $label$out"
    after=$(login)
    echo "SSHLOADEXHAUST $label gone_after_ms=${gone:-never} login_after=$after" \
        "kernel_oom_kills=$(journalctl -k -b --no-pager | grep -c 'Out of memory: Killed')" \
        "min_ttl_ms=$(cat /sys/kernel/mm/lru_gen/min_ttl_ms 2>/dev/null || echo none)" \
        "swap_total=$(awk '/^SwapTotal:/ { print int($2 / 1024) }' /proc/meminfo)MiB" \
        "victims=$(journalctl -k -b --no-pager | sed -n 's/.*Killed process [0-9]* (\([^)]*\)).*/\1/p' | sort | uniq -c | tr -s ' ' | tr '\n' ',')"
    pkill -KILL -u spin || true
    sleep 2
}

# tail keeping a line that never ends, from a pipe: `tail /dev/zero` seeks to the end of the
# device, keeps nothing and measured nothing (2026-09-28). At full speed the kernel runs out of
# anything to reclaim within a second or two and kills it.
exhaust() {
    grow exhaust "cat /dev/zero | tail"
}

# The case that hangs a machine: memory taken slowly, ~80 MB/s, while the session reads its
# libraries over and over, as a build reads headers. Reclaim keeps finding page cache to evict
# and the reader keeps faulting it back, so the kernel is never out of options and the OOM
# killer is never called: the old LRU's thrash.
exhaust_slow() {
    grow exhaust-slow "(while :; do cat /usr/lib/x86_64-linux-gnu/*.so* >/dev/null 2>&1; done) & while :; do head -c 8M /dev/zero; sleep 0.1; done | tail"
}

for c in ${SSHLOAD_CASES:-idle cpu_service cpu_session tmp_full}; do
    case $c in
    idle) measure idle ;;
    *) "$c" ;;
    esac
done

trap - EXIT
echo SSHLOAD_DONE
