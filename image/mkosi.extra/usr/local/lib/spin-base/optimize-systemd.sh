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

    # Time is the host's: timedated would let a session set a clock it does not own.
    systemd-timedated.service

    systemd-journal-flush.service
    systemd-journal-catalog-update.service

    systemd-fsck-root.service
    systemd-update-done.service

    systemd-modules-load.service
    systemd-hibernate-clear.service
    systemd-pstore.service
    systemd-hwdb-update.service

    # First-boot setup (done at build time)
    systemd-firstboot.service
    systemd-machine-id-commit.service

    # Random seed (VM gets entropy from virtio-rng)
    systemd-random-seed.service

    # ldconfig, already pre-warmed at build time.
    ldconfig.service

    # sysusers, done at build time.
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
    # no hugepages are configured, and nothing uses POSIX message queues.
    dev-hugepages.mount
    dev-mqueue.mount
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
    # One that cannot do anything on this machine: the factory-reset generator runs on a
    # machine systemd itself logs as not booted in EFI mode.
    #
    # Worth, with the tpm2 generator that was here, 1.0 ms of the 26 ms systemd spends loading
    # units and building the initial transaction — measured 2026-09-26 with the unitload probe
    # (ad05544), which asks systemd for that number through `systemd --test` rather than reading
    # it out of a debug-logged boot. The same number read from a debug boot is 212 ms, and that
    # difference is the logging.
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

# The manager's timeouts. Its logging and status lines are systemd's own: LogLevel=err,
# LogTarget=journal, ShowStatus=no and StatusUnitFormat=name were here, with a kernel.printk
# sysctl of 3 3 3 3, and taken out together they moved the first login +2.4 [-2.3, +7.8] ms at
# 20 boots and -1.8 [-7.7, +3.2] at 40, spin's shape (labs 44818953987, 44822157367, 2026-11-21).
# The console stays quiet through the kernel command line's quiet and loglevel=3.
log "Configuring systemd manager timeouts..."
mkdir -p /etc/systemd/system.conf.d

cat <<'EOF' >/etc/systemd/system.conf.d/fast-boot.conf
[Manager]
DefaultTimeoutStartSec=15s
DefaultTimeoutStopSec=10s
DefaultDeviceTimeoutSec=10s

RuntimeWatchdogSec=0
ShutdownWatchdogSec=0
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
# The pressure probe's guest half, after the boot's work: its unit says why, and its [Install]
# says multi-user.target, which this is the enable of. image/build.sh installs the binary.
mkdir -p /etc/systemd/system/multi-user.target.wants
ln -sf /usr/lib/systemd/system/spin-machine-pressure.service \
    /etc/systemd/system/multi-user.target.wants/spin-machine-pressure.service

# logind stays in the boot transaction, as the package wants it
# (/usr/lib/systemd/system/multi-user.target.wants/systemd-logind.service), so that the first
# login does not start it. spin lingered when this was measured, and no longer does
# (configure-system.sh). Taking it out of the boot was worth 24 ms to a console prompt (2026-09-12) and moved logind,
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
