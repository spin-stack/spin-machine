#!/usr/bin/env bash
set -euo pipefail

echo "Configuring system settings..."

# -------------------------------------------------
# 1. Create necessary directories
# -------------------------------------------------
echo "Creating system directories..."
mkdir -p \
    /run/sshd \
    /root/.ssh

chmod 700 /root/.ssh

# -------------------------------------------------
# 2. Configure SSH
# -------------------------------------------------
echo "Configuring SSH server..."
sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin yes/' /etc/ssh/sshd_config
sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config
sed -i 's/#PubkeyAuthentication yes/PubkeyAuthentication yes/' /etc/ssh/sshd_config

# -------------------------------------------------
# 4. Configure network interface naming
# -------------------------------------------------
echo "Configuring network interface naming..."
mkdir -p /etc/systemd/network/99-default.link.d
cat <<'EOF' > /etc/systemd/network/99-default.link.d/traditional-naming.conf
[Link]
NamePolicy=keep kernel
EOF

# -------------------------------------------------
# 5. An unprivileged user, and no passwords at all
# -------------------------------------------------
# This image had `echo 'root:spinbox' | chpasswd`, and it was the one credential baked into
# a filesystem that every VM boots read-only: the same root password on every machine that
# ever came from it, discoverable by reading a build script. It is gone.
#
# Nothing replaces it. Both accounts are locked, and the way into a machine is either the
# debug console — which autologins, because whoever can attach to it already has the disk —
# or a key placed by whoever provisions the VM, alongside the rest of its identity.
echo "Creating the workspace user..."
# The docker group exists by now: install-dev-tools.sh runs before this and the package
# creates it. A workspace user who has to sudo for every container is a workspace user who
# runs everything as root.
useradd --create-home --shell /bin/bash --uid 1000 --groups sudo,docker spin
# No password for either account: `!` in the shadow field is a locked account, which is
# what an image that hands out no credentials should have.
passwd --lock root
passwd --lock spin

# Sudo without a password, because there is no password. The boundary this machine relies
# on is the VM, not the difference between two accounts inside it — but a workload that
# expects not to be root by default gets what it expects.
install -m 0440 /dev/stdin /etc/sudoers.d/spin <<'EOF'
spin ALL=(ALL) NOPASSWD: ALL
EOF

# -------------------------------------------------
# 6. Disable MOTD
# -------------------------------------------------
echo "Disabling MOTD..."
rm -rf /etc/update-motd.d/*
truncate -s 0 /etc/motd
sed -i 's/^session.*pam_motd.so/#&/' /etc/pam.d/sshd


# -------------------------------------------------
# 7. The distribution's background maintenance
# -------------------------------------------------
# Ubuntu ships five timers enabled. Four of them have no business in a machine that is
# restored from a template, does one piece of work and is thrown away.
#
# apt-daily and apt-daily-upgrade are the two that produce a failure rather than merely
# spending time: they run `apt.systemd.daily update` and `install`, which take the apt and
# dpkg locks. A workload that installs a package of its own then gets
# `Could not get lock /var/lib/dpkg/lock-frontend` at a moment nothing in its own logs
# explains. They also reach the archive, which is egress this machine never asked for.
#
# motd-news is the other half of the job section 6 above started. Its ExecStart is
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
# All five carry Persistent=true, and that is what makes this a restore problem rather
# than only a boot-time one. The stamps live in /var/lib/systemd/timers, so they are the
# template's; a VM restored from a template that sat on disk for a week is a machine with
# a week of missed runs to catch up on, and it catches up at the moment it is handed a
# workload.
#
# Masked and not merely disabled: a symlink to /dev/null survives a package upgrade
# putting the unit's own [Install] symlink back. image/build.sh asserts each one.
echo "Masking the distribution's background maintenance timers..."
for unit in apt-daily.timer apt-daily-upgrade.timer motd-news.timer dpkg-db-backup.timer; do
    ln -sf /dev/null "/etc/systemd/system/$unit"
done

echo "✅ System configuration complete"
