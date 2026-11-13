#!/usr/bin/env bash
# Runs in mkosi's chroot, where no systemd is running: units are masked and enabled with
# symlinks, not systemctl.
set -euo pipefail
IFS=$'\n\t'

log()   { printf '[%s] %s\n' "$(date +%T)" "$*"; }

MASK_DIR="/etc/systemd/system"
mkdir -p "$MASK_DIR"

mask_unit() {
    local unit=$1
    log "Masking $unit"
    ln -sf /dev/null "$MASK_DIR/$unit"
}

MASK_UNITS=(

    # udev is NOT masked, and was, for six months, to save boot time it does not cost.
    # Measured 2026-09-10 with `task boot:bench`, 15 boots of each on the real release under
    # KVM: as shipped, every boot reaches a login prompt; with these four units masked,
    # **fifteen out of fifteen never reach one at all**. The machine spends the full
    # ten-second dev-ttyS0.device timeout and then has no login.
    #
    # What masking cost was everything that needs a .device unit to exist.
    #
    #   * serial-getty@ttyS0 carries BindsTo=dev-%i.device and could not start.
    #   * A hot-plugged CPU never produced an add event, so nothing could act on one —
    #     see 40-spin-hotadd.rules, which is the thing that acts on it.

    # Time sync - handled by host
    systemd-timesyncd.service
    systemd-timedated.service

    systemd-journal-flush.service
    systemd-journal-catalog-update.service

    systemd-fsck-root.service
    systemd-update-done.service

    systemd-modules-load.service
    systemd-hibernate-clear.service
    systemd-pcrmachine.service
    systemd-pstore.service
    systemd-hwdb-update.service
    systemd-tpm2-setup.service
    systemd-tpm2-setup-early.service

    # First-boot setup (done at build time)
    systemd-firstboot.service
    systemd-machine-id-commit.service

    # Random seed (VM gets entropy from virtio-rng)
    systemd-random-seed.service

    systemd-boot-system-token.service

    avahi-daemon.service
    cups.service
    bluetooth.service

    smartd.service
    e2scrub_reap.service
    multipathd.service
    multipathd.socket
    lvm2-monitor.service

    e2scrub_all.timer

    # ldconfig, already pre-warmed at build time.
    ldconfig.service

    # sysusers, done at build time. systemd-sysctl was masked here too and was
    # removed from this list, because the same script writes /etc/sysctl.d/99-quiet-boot.conf
    # and masking the unit that applies it left the file inert.
    systemd-sysusers.service

    # Modprobe services: this kernel has CONFIG_MODULES off entirely, so there is nothing to
    # load and these can only fail.
    modprobe@configfs.service
    modprobe@drm.service
    modprobe@fuse.service
    modprobe@efi_pstore.service

    # Debug and tracing mounts. Masked, not absent: a probe that needs tracefs
    # mounts it by hand.
    sys-kernel-debug.mount
    sys-kernel-tracing.mount

    # tmpfiles-setup for /dev: /dev is devtmpfs with udev on top, and the nodes
    # these would create are already there. The one for the rest of the filesystem is not
    # masked, nor is tmp.mount - image/build.sh says why.
    systemd-tmpfiles-setup-dev.service
    systemd-tmpfiles-setup-dev-early.service

    # Mounts for hardware and interfaces this machine does not have:
    # no hugepages are configured, nothing uses POSIX message queues, and /run/lock has no
    # user on a machine that runs one workload.
    dev-hugepages.mount
    dev-mqueue.mount
    run-lock.mount
)

log "Masking unnecessary systemd units..."
for unit in "${MASK_UNITS[@]}"; do
    mask_unit "$unit"
done

log "Disabling unnecessary systemd generators..."
mkdir -p /etc/systemd/system-generators

DISABLE_GENERATORS=(
    systemd-fstab-generator
    systemd-debug-generator
    systemd-gpt-auto-generator
    systemd-sysv-generator
    systemd-rc-local-generator
    systemd-hibernate-resume-generator
    systemd-system-update-generator
    # Two that cannot do anything on this machine, and the boot log says so in both cases:
    # the tpm2 one reports that libtss2-esys.so.0 cannot be opened, and the factory-reset one
    # runs on a machine systemd itself logs as not booted in EFI mode.
    #
    # Worth 1.0 ms of the 26 ms systemd spends loading units and building the initial
    # transaction — measured 2026-09-26 with the unitload probe (ad05544), which asks systemd for that
    # number through `systemd --test` rather than reading it out of a debug-logged boot. The
    # same number read from a debug boot is 212 ms, and that difference is the logging.
    systemd-tpm2-generator
    systemd-factory-reset-generator
)

for gen in "${DISABLE_GENERATORS[@]}"; do
    ln -sf /dev/null "/etc/systemd/system-generators/$gen"
    log "Disabled generator: $gen"
done

log "Masking unnecessary tmpfiles rules..."
mkdir -p /etc/tmpfiles.d

MASK_TMPFILES=(
    systemd-pstore.conf
    systemd-network.conf
    x11.conf
)

for conf in "${MASK_TMPFILES[@]}"; do
    ln -sf /dev/null "/etc/tmpfiles.d/$conf"
done

log "Configuring systemd manager for quiet, fast boot..."
mkdir -p /etc/systemd/system.conf.d

cat <<'EOF' >/etc/systemd/system.conf.d/fast-boot.conf
[Manager]
DefaultTimeoutStartSec=15s
DefaultTimeoutStopSec=10s
DefaultDeviceTimeoutSec=10s

RuntimeWatchdogSec=0
ShutdownWatchdogSec=0

LogLevel=err
LogTarget=journal

ShowStatus=no
StatusUnitFormat=name
EOF

log "Configuring journald for minimal overhead..."
mkdir -p /etc/systemd/journald.conf.d

cat <<'EOF' >/etc/systemd/journald.conf.d/volatile.conf
[Journal]
Storage=volatile
RuntimeMaxUse=4M
RuntimeMaxFileSize=1M

Compress=no
Seal=no
SyncIntervalSec=0

RateLimitBurst=10000
RateLimitIntervalSec=30s

ForwardToConsole=no
ForwardToWall=no
ForwardToSyslog=no
ForwardToKMsg=no

SplitMode=none
Audit=no
LineMax=48K
EOF

log "Configuring quiet console..."

mkdir -p /etc/sysctl.d
cat <<'EOF' >/etc/sysctl.d/99-quiet-boot.conf
kernel.printk = 3 3 3 3
EOF

log "Configuring Docker and containerd for socket activation..."

rm -f /etc/systemd/system/multi-user.target.wants/docker.service 2>/dev/null || true
rm -f /etc/systemd/system/multi-user.target.wants/containerd.service 2>/dev/null || true

mkdir -p /etc/systemd/system/sockets.target.wants
ln -sf /etc/systemd/system/containerd.socket /etc/systemd/system/sockets.target.wants/containerd.socket
ln -sf /etc/systemd/system/docker.socket /etc/systemd/system/sockets.target.wants/docker.socket

log "Configuring SSH for socket activation..."
rm -f /etc/systemd/system/multi-user.target.wants/ssh.service 2>/dev/null || true
mkdir -p /etc/systemd/system/sockets.target.wants
ln -sf /lib/systemd/system/ssh.socket /etc/systemd/system/sockets.target.wants/ssh.socket
# The host keys from the boot's first dispatch rather than the first login's: the unit says why
# and what it was worth. Its [Install] says sysinit.target, and this is that enable. Dangling
# until mkosi.postinst.chroot copies the unit in, as the containerd and docker links above are.
mkdir -p /etc/systemd/system/sysinit.target.wants
ln -sf /usr/lib/systemd/system/spin-machine-sshd-keygen.service \
    /etc/systemd/system/sysinit.target.wants/spin-machine-sshd-keygen.service

# logind stays in the boot transaction, as the package wants it
# (/usr/lib/systemd/system/multi-user.target.wants/systemd-logind.service), because spin
# lingers - configure-system.sh - and it is logind that starts a lingering user's manager.
# Taking it out of the boot was worth 24 ms to a console prompt (2026-09-12) and moved logind,
# dbus-broker and user@1000.service into the first login instead, which is what a workspace
# waits for: with logind in the boot and spin lingering, the first SSH login over vsock came
# 60 ms sooner, 510/525 against 570/626 ms p50/p95, and sshd accepted no later (321 against
# 324), 10 boots of each interleaved on the bench fleet (task boot:ssh-first, lab run
# 43308825692, 2026-11-12). image/build.sh asserts the want is not overridden.

log "Setting default target to multi-user..."
ln -sf /lib/systemd/system/multi-user.target /etc/systemd/system/default.target

log "Optimizing nsswitch.conf..."
cat <<'EOF' >/etc/nsswitch.conf
passwd:         files
group:          files
shadow:         files
gshadow:        files
hosts:          files dns
networks:       files
protocols:      files
services:       files
ethers:         files
rpc:            files
EOF

log "Pre-warming caches..."
ldconfig || true
locale-gen en_US.UTF-8 2>/dev/null || true
update-locale LANG=en_US.UTF-8 2>/dev/null || true
iconvconfig 2>/dev/null || true
fc-cache -f 2>/dev/null || true

log "Running cleanup..."
rm -rf /usr/share/doc/* 2>/dev/null || true
rm -rf /usr/share/man/* 2>/dev/null || true
rm -rf /var/lib/apt/lists/* 2>/dev/null || true
rm -rf /var/log/journal/* 2>/dev/null || true

log "Boot optimization complete!"
