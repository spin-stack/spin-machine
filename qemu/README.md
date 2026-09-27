# QEMU

One build, **statically linked**, against musl on Alpine. `task qemu:build` builds it from
source (tens of minutes); `task qemu:fetch` unpacks the published build of the pinned
version (seconds). `devices.mak` is the device allowlist.

A dynamically linked QEMU is not one artefact — it is an artefact plus the libraries it was
compiled against. Extracted onto a host with different ones it dies at start-up
(`liburing.so.2: cannot open shared object file`), so a release had to ship either a
container to run it in or the loader and the libraries beside it, and a consumer had to know
which. Static ends the question: the binary runs wherever the kernel does. musl rather than
glibc because glibc links statically but does it badly — anything resolving a user or host
name still wants to `dlopen` an NSS module, and a static binary cannot.

Measured before adopting it, same host and machine line: process start to a QMP `quit`,
**44 ms glibc against 43 ms musl** over five runs. Not measured: a long-running guest under
load, where musl's allocator and its smaller default thread stack differ from glibc's.

Both the KVM binary and a TCG one for runners with no `/dev/kvm`: a host serving tenants
must fail at start-up rather than run a tenant's VM at a tenth of the speed, and the build
asserts both halves, since neither is visible in a file listing.

`--enable-tools` ships `qemu-img`, which is not a convenience: a container's writable layer
is a qcow2 overlay created by running it, so a release with the emulator and without it
cannot give a container anywhere to write. `qemu-nbd` comes out of the same flag and is
deliberately not shipped — it exports a disk over NBD, which nothing here does.

**PVH, not BIOS.** The kernel is an ELF `vmlinux` with Xen PVH notes and QEMU enters it
through `pvh.bin`. There is no bootloader and no UEFI: the same guest under UEFI + Secure
Boot was measured at +356 ms and rejected. Replacing SeaBIOS with qboot was measured too and
cannot run this machine; see [qboot/README.md](qboot/README.md).
