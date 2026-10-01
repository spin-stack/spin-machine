#!/usr/bin/env bash
set -euo pipefail

echo "Installing development tools..."

# Versions and sha256s from versions.yaml, through image/Dockerfile and mkosi.conf's Environment=.
: "${TASK_VERSION:?}" "${TASK_SHA256:?}"
: "${GIT_LFS_VERSION:?}" "${GIT_LFS_SHA256:?}"
: "${BUILDKIT_VERSION:?}" "${BUILDKIT_SHA256:?}"

download() {
    local url="$1" output="$2" sha256="$3"
    echo "Downloading ${url}..."
    curl -fsSL "${url}" -o "${output}"
    echo "${sha256}  ${output}" | sha256sum -c -
}

# -------------------------------------------------
# Install Task (Taskfile runner)
# -------------------------------------------------
echo "Installing Task v${TASK_VERSION}..."
download \
    "https://github.com/go-task/task/releases/download/v${TASK_VERSION}/task_linux_amd64.tar.gz" \
    "/tmp/task.tar.gz" \
    "${TASK_SHA256}"

tar -xzf /tmp/task.tar.gz -C /tmp
install -m 755 /tmp/task /usr/local/bin/task
task --version

# -------------------------------------------------
# Install Git LFS
# -------------------------------------------------
echo "Installing Git LFS v${GIT_LFS_VERSION}..."
download \
    "https://github.com/git-lfs/git-lfs/releases/download/v${GIT_LFS_VERSION}/git-lfs-linux-amd64-v${GIT_LFS_VERSION}.tar.gz" \
    "/tmp/git-lfs.tar.gz" \
    "${GIT_LFS_SHA256}"

tar -xzf /tmp/git-lfs.tar.gz -C /tmp
install -m 755 "/tmp/git-lfs-${GIT_LFS_VERSION}/git-lfs" /usr/local/bin/git-lfs
git-lfs version

# -------------------------------------------------
# Install BuildKit
# -------------------------------------------------
echo "Installing BuildKit v${BUILDKIT_VERSION}..."
download \
    "https://github.com/moby/buildkit/releases/download/v${BUILDKIT_VERSION}/buildkit-v${BUILDKIT_VERSION}.linux-amd64.tar.gz" \
    "/tmp/buildkit.tar.gz" \
    "${BUILDKIT_SHA256}"

tar -xzf /tmp/buildkit.tar.gz -C /usr/local
# Remove QEMU binaries we don't need
rm -rf /usr/local/bin/buildkit-qemu-* 2>/dev/null || true
buildctl --version

install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc

# Add the repository to Apt sources:
tee /etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: $(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")
Components: stable
Signed-By: /etc/apt/keyrings/docker.asc
EOF

apt-get update

apt-get install -y \
    docker-ce \
    docker-ce-cli \
    containerd.io \
    docker-buildx-plugin \
    docker-compose-plugin

# -------------------------------------------------
# Cleanup
# -------------------------------------------------
echo "Cleaning up temporary files..."
rm -rf /var/tmp/* /usr/local/share/doc /usr/local/share/man

apt-get clean

echo "✅ Development tools installation complete"
