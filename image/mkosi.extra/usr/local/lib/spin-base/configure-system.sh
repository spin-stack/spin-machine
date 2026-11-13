#!/usr/bin/env bash
set -euo pipefail

echo "Configuring system settings..."

echo "Creating system directories..."
mkdir -p \
    /run/sshd \
    /root/.ssh

chmod 700 /root/.ssh

echo "Configuring SSH server..."
sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin yes/' /etc/ssh/sshd_config
sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config
sed -i 's/#PubkeyAuthentication yes/PubkeyAuthentication yes/' /etc/ssh/sshd_config

# Trust the certificate authority of whichever host this workspace ends up on.
#
# The path is named here and the file arrives later: TrustedUserCAKeys takes a path, and at
# image build time there is no host to ask. The supervisor writes it from the registration
# response, which is where it learns which host it is running on. sshd re-reads the file per
# authentication, so a workspace on a host with no CA simply has no file here and accepts no
# certificates.
#
# A certificate is the only way in. At every launch spin's handoff writes a drop-in read before
# this one (00-spin-owner.conf): no authorized_keys, no password, and the principal a
# certificate may log in as asked of the supervisor, which answers it only for a key the
# workspace owner's signed word lists. Without this line nothing logs in at all, which is the
# failure that locks people out rather than in.
mkdir -p /etc/ssh/sshd_config.d
cat <<'EOF' > /etc/ssh/sshd_config.d/10-spin-user-ca.conf
TrustedUserCAKeys /etc/ssh/spin_user_ca.pub
EOF

# One key exchange, mlkem768x25519-sha256, and a client that has no ML-KEM is refused rather
# than downgraded: "no matching key exchange method found". It is post-quantum and costs what
# curve25519 does - on the bench fleet a login with each took 40 and 39 ms (task
# boot:ssh-first, lab run 43479854381, 2026-11-13) - while sntrup761x25519, the other
# post-quantum exchange and what OpenSSH 8.5-9.8 picks, took 104, its key generation alone
# ~39 ms of the client's. A client needs OpenSSH 9.9 or later, or golang.org/x/crypto/ssh
# v0.38.0 or later built with Go 1.24 or later; docs/machine.md says who that leaves out.
#
# sshd takes the first value it reads for a keyword, and spin's handoff writes drop-ins
# named 00-*, read before this one: none of them may set KexAlgorithms.
cat <<'EOF' > /etc/ssh/sshd_config.d/10-spin-kex.conf
KexAlgorithms mlkem768x25519-sha256
EOF

# The drop-in only counts if sshd_config includes the directory. Asserted rather than
# assumed, because the way this fails is silent: sshd starts, key logins keep working, and
# only certificate authentication is dead.
grep -qE '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/\*\.conf' /etc/ssh/sshd_config || {
    echo "ERROR: sshd_config does not include sshd_config.d; the user CA would be ignored" >&2
    exit 1
}

echo "Configuring network interface naming..."
mkdir -p /etc/systemd/network/99-default.link.d
cat <<'EOF' > /etc/systemd/network/99-default.link.d/traditional-naming.conf
[Link]
NamePolicy=keep kernel
EOF

# An unprivileged user, and no passwords at all: a password baked into a filesystem every VM
# boots read-only is the same credential on every machine that ever came from it. Both accounts
# are locked, and the way into a machine is a certificate (above) placed by whoever provisions
# the VM, alongside the rest of its identity.
echo "Creating the workspace user..."
# The docker group exists by now: install-dev-tools.sh runs before this and the package
# creates it. A workspace user who has to sudo for every container is a workspace user who
# runs everything as root.
useradd --create-home --shell /bin/bash --uid 1000 --groups sudo,docker spin
passwd --lock root
passwd --lock spin

# spin lingers: logind starts spin's user manager (user@1000.service) during the boot rather
# than inside the first login, which is the one thing a workspace waits for. The first SSH
# login came 60 ms sooner for it; the numbers are in optimize-systemd.sh, beside logind being
# kept in the boot for the same reason. What `loginctl enable-linger` writes, without a logind
# to ask - this runs in mkosi's chroot. It names a user, not a machine: no identity.
install -d -m 0755 /var/lib/systemd/linger
touch /var/lib/systemd/linger/spin

# Sudo without a password, because there is no password. The boundary this machine relies
# on is the VM, not the difference between two accounts inside it — but a workload that
# expects not to be root by default gets what it expects.
install -m 0440 /dev/stdin /etc/sudoers.d/spin <<'EOF'
spin ALL=(ALL) NOPASSWD: ALL
EOF

# No MOTD, and no pam_motd to run one. The distribution's scripts check for updates, count
# reboots and advertise a subscription - network calls and package queries in a machine that
# is started, does one piece of work and is thrown away. This image's own replacement, a line
# saying how long the boot took, ran systemd-analyze inside every login, which is the one
# thing a workspace waits for; a boot time belongs to task boot:ssh-first and the release's report,
# which measure it on purpose, not to a greeting nobody acts on (removed 2026-11-12).
#
# pam_motd goes from every PAM stack that has it, not only ssh's: with it enabled, whatever a
# package upgrade restores into /etc/update-motd.d runs at the next login, and image/build.sh
# asserts the lines are off rather than that the directory is empty.
echo "Removing the MOTD..."
rm -rf /etc/update-motd.d/*
truncate -s 0 /etc/motd
sed -i 's/^\(session.*pam_motd\.so\)/#\1/' /etc/pam.d/sshd /etc/pam.d/login


# The distribution's background maintenance. Ubuntu ships five timers enabled. Four of them have no business in a machine that is
# started, does one piece of work and is thrown away.
#
# apt-daily and apt-daily-upgrade are the two that produce a failure rather than merely
# spending time: they run `apt.systemd.daily update` and `install`, which take the apt and
# dpkg locks. A workload that installs a package of its own then gets
# `Could not get lock /var/lib/dpkg/lock-frontend` at a moment nothing in its own logs
# explains. They also reach the archive, which is egress this machine never asked for.
#
# motd-news is the other half of the MOTD replacement above. Its ExecStart is
# /etc/update-motd.d/50-motd-news, and that directory was emptied there — so the unit can
# only fail 203/EXEC, twice a day, leaving a permanent entry in `systemctl --failed` for
# whoever looks at the machine next.
#
# dpkg-db-backup copies the dpkg database daily, in an image whose database is already
# written beside the release as packages.txt and whose root filesystem is read-only.
#
# fstrim is deliberately NOT masked. The drives are opened discard=unmap, so the guest's
# TRIM reaches the qcow2 overlay and gives its blocks back to the host: it is the one of
# the five that returns something.
#
# All five carry Persistent=true, so they catch up on what they missed: a VM resumed from
# a checkpoint that sat on disk for a week is a machine with a week of missed runs to catch
# up on, and it catches up at the moment it is handed a workload.
#
# Masked and not merely disabled: a symlink to /dev/null survives a package upgrade
# putting the unit's own [Install] symlink back. image/build.sh asserts each one.
echo "Masking the distribution's background maintenance timers..."
for unit in apt-daily.timer apt-daily-upgrade.timer motd-news.timer dpkg-db-backup.timer; do
    ln -sf /dev/null "/etc/systemd/system/$unit"
done

# fstrim, hourly rather than the distribution's weekly. What a guest deletes stays in its
# qcow2 overlay, allocated on the host's disk and held in the host's page cache, until a trim
# reaches it: a 512 MB file written, read and removed kept 657 MB of the overlay in both, and
# one fstrim took them to 21 MB and 20 MB (spin-machine lab run 36815822140, 2026-10-01). A
# week is a long time for a host to hold a workspace's deleted build. ext4 only trims the
# block groups that have freed something since their last trim, so a run with nothing to
# give back is cheap. RandomizedDelaySec keeps the machines on a host from trimming at once.
echo "Running fstrim hourly..."
mkdir -p /etc/systemd/system/fstrim.timer.d
cat > /etc/systemd/system/fstrim.timer.d/spin.conf <<'UNIT'
[Timer]
OnCalendar=
OnCalendar=hourly
RandomizedDelaySec=10min
UNIT

echo "✅ System configuration complete"
