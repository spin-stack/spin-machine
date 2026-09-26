# qboot, stock and patched, for the firmware experiment in this directory.
#
# Not part of a release and not on the path to one. What it is for is answering whether a
# firmware other than SeaBIOS can run this machine at all, and then what it saves — see
# README.md beside this file, and `task boot:firmware`.
#
# ## Why this is a container and not a shell script
#
# The two binaries it produces are compared against each other in microseconds, so the
# compiler that produced them is part of the measurement. A script running on whatever gcc a
# developer happens to have makes "patched is 5 ms faster" a statement about two toolchains
# as much as about two firmwares. The same reason kernel/Dockerfile pins its Debian by digest
# and its archive by snapshot date applies here, and more sharply: firmware is 65 KiB of
# code where a single inlining decision is visible in the number.
#
# It also removes the freestanding-i386 prerequisite from the host. `gcc -m32 -ffreestanding`
# needs 32-bit code generation compiled into the compiler, which is not a thing every
# distribution's gcc has, and the failure is a wall of assembler errors rather than a missing
# package.
#
# The same pinned toolchain as the kernel, deliberately: one Debian to think about rather
# than two, and moving it is one decision. It does not have to be the kernel's — nothing
# here enters a fingerprint, because nothing here ships — but there is no reason for it to
# differ.
ARG DEBIAN_IMAGE=debian:trixie@sha256:f324c7ff54321e8d9c588493a20244965938ce0aa50bbd1022d38010e9ffc4b1
ARG DEBIAN_SNAPSHOT=20260914T000000Z

FROM ${DEBIAN_IMAGE} AS builder

ARG DEBIAN_SNAPSHOT
ENV DEBIAN_FRONTEND=noninteractive

RUN echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

# The archive as it stood on DEBIAN_SNAPSHOT rather than the image's live mirrors, so the
# compiler is the same one next month. Valid-Until is off because a snapshot's Release files
# expire by design; the signatures are still checked, and they are what makes plain http
# enough — it has to be http, since the image carries no CA certificates until
# ca-certificates is installed from this very archive.
RUN rm -f /etc/apt/sources.list.d/debian.sources && cat > /etc/apt/sources.list.d/snapshot.sources <<EOF
Types: deb
URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}
Suites: trixie trixie-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
Check-Valid-Until: no
EOF

RUN --mount=type=cache,sharing=locked,id=qboot-aptlib,target=/var/lib/apt \
    --mount=type=cache,sharing=locked,id=qboot-aptcache,target=/var/cache/apt \
    apt-get update && apt-get install -y --no-install-recommends \
        gcc binutils patch git ca-certificates

# The upstream commit, and the SHA-256 of the tar `git archive` writes for it.
#
# Two checks and not one, because they answer different questions. git verifies that the tree
# is the one this commit names — that is what a commit hash is — and the tarball checksum
# verifies that the bytes handed to the compiler are the bytes that were measured. The second
# is only stable because the git in this image is pinned: `git archive` output depends on the
# git version, which is the kind of thing that makes a checksum in a script on a developer's
# machine fail for no reason anybody can act on.
#
# qboot's last upstream commit is 2022-09-16. That is not an argument against measuring it
# and it is an argument against shipping it.
ARG QBOOT_COMMIT=8ca302e86d685fa05b16e2b208888243da319941
ARG QBOOT_ARCHIVE_SHA256=6fb3842d9be9c7488ffef33e5aa2cc430a5065337ad6239e3facf981b38748d2

WORKDIR /build
RUN git clone --quiet https://github.com/bonzini/qboot.git upstream && \
    git -C upstream archive "${QBOOT_COMMIT}" > upstream.tar && \
    echo "${QBOOT_ARCHIVE_SHA256}  upstream.tar" | sha256sum -c -

COPY qemu/qboot/write-pointer.patch /build/write-pointer.patch

# Both variants from one tar and one compiler. The flags are upstream's meson.build, with -Os
# fixed rather than taken from a build type: two firmwares compiled at different optimisation
# levels are not a comparison of two firmwares.
RUN set -eux; \
    for variant in stock patched; do \
        rm -rf "src-$variant"; mkdir -p "src-$variant" "out/$variant"; \
        tar -xf upstream.tar -C "src-$variant"; \
        if [ "$variant" = patched ]; then \
            patch -p1 -d "src-$variant" < write-pointer.patch; \
        fi; \
        cd "src-$variant"; \
        for f in *.c *.S; do \
            gcc -Os -m32 -march=i386 -mregparm=3 -fno-stack-protector \
                -fno-delete-null-pointer-checks -ffreestanding \
                -mstringop-strategy=rep_byte -minline-all-stringops -fno-pic \
                -Iinclude -c "$f" -o "/build/out/$variant/${f%.*}.o"; \
        done; \
        gcc -nostdlib -m32 -no-pie -Wl,--build-id=none -Wl,-Tflat.lds \
            "/build/out/$variant/"*.o -o "/build/out/$variant/bios.bin.elf"; \
        objcopy -O binary "/build/out/$variant/bios.bin.elf" "/build/out/$variant/bios.bin"; \
        cd /build; \
    done

# Asserted on what came out, not on the build having exited 0.
#
# A patch that applies to nothing still leaves a compilable tree, and two identical binaries
# would make the probe report a 0 ms difference and look like a finding. The size is checked
# because objcopy of a linker script gone wrong produces a file, and a firmware that is not
# the size the ROM window expects does not fail at build time — it fails as a guest that
# prints nothing, which is indistinguishable from the bug this experiment is about.
RUN set -eux; \
    for variant in stock patched; do \
        test -s "out/$variant/bios.bin"; \
        size=$(stat -c%s "out/$variant/bios.bin"); \
        test "$size" -eq 65536 || { echo "out/$variant/bios.bin is $size bytes, not 65536" >&2; exit 1; }; \
    done; \
    if cmp -s out/stock/bios.bin out/patched/bios.bin; then \
        echo "stock and patched are byte-identical: the patch changed nothing that compiled" >&2; \
        exit 1; \
    fi; \
    sha256sum out/stock/bios.bin out/patched/bios.bin

FROM scratch AS extract
COPY --from=builder /build/out/stock/bios.bin /stock/bios.bin
COPY --from=builder /build/out/patched/bios.bin /patched/bios.bin
