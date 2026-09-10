# The device set QEMU is built with, passed as `--with-devices-x86_64=spin`.
#
# There is one copy of this list, and there has to be. A device set is part of what a
# machine *is*: two of them is two machines that a template cannot be moved between, and
# nothing says so at run time.
#
# A file and not a `sed`/`echo >>` over the source tree, because that tree lives in a
# BuildKit cache mount keyed by version alone: an edit made there survives every later
# build, including ones meant to undo it, and — worse — it is applied inside the
# `if [ ! -f config.status ]` branch, so whether the tree being compiled is the tree
# upstream shipped depends on whether some earlier build happened to configure. This file
# is copied in and read on every build.
#
# ## An allowlist, since 2026-09-10
#
# This file used to include upstream's default.mak and switch off about forty symbols. That
# is a denylist, and it loses by construction: it can only remove what somebody thought to
# name, and every QEMU release adds devices that arrive switched on. The binary it produced
# could instantiate ich9-ahci, ide-hd, sb16, isa-fdc, isa-parallel, VGA, virtio-vga,
# vfio-pci, intel-iommu and amd-iommu — audited on the shipped binary with `-device help`,
# not read off this file.
#
# Now the build is configured `--without-default-devices`, which makes meson run
# scripts/minikconf.py with `--allnoconfig` instead of `--defconfig`: nothing is on unless
# it is named here or `select`ed by something that is. What that changed, resolved with
# minikconf before any build was attempted:
#
#     166 symbols -> 69
#     gone: USB (every class and every controller), VGA and virtio-vga, sb16, adlib, gus,
#           cs4231a, FDC and the floppy, isa-parallel, VFIO and vfio-user, intel-iommu and
#           amd-iommu, TPM, IPMI, NVDIMM, SEV, SGX, TDX, CXL, HPET, PCI expander bridges,
#           every vhost-user device, e1000/rtl8139/pcnet/vmxnet3/ne2k, MICROVM
#     kept because Q35 selects them: AHCI, AHCI_ICH9, IDE_BUS, IDE_CORE, IDE_DEV
#
# The last line is the honest limit of this approach. The ICH9 southbridge brings an AHCI
# controller and the IDE core behind it, and `select` is not negotiable from here — turning
# them off is a contradiction minikconf refuses. They stay compiled in and nothing attaches
# them, which is the same position the four ISA devices were in before and is why the
# machine string carries sata=off.
#
# MICROVM was kept here until today on the grounds that it "costs nothing and is the obvious
# next experiment for boot time". That experiment was run on 2026-09-10 and rejected:
# microvm has no ACPI CPU hotplug, which this machine requires. It goes with the rest.
#
# ## The rule for this file
#
# **One device per function, and the function's most compatible device.** For a Linux guest
# that is virtio without qualification: in mainline since 2.6.25 and in every distribution's
# kernel. e1000 and rtl8139 are "more compatible" only for a guest with no virtio drivers,
# which is not what this runs. Every extra model is a device a guest could be given by
# accident, a driver its kernel has to carry, and for a NIC an option ROM QEMU refuses to
# start without.
#
# What a VM actually gets is in the machine package (machine/machine.go): virtio-blk-pci,
# virtio-net-pci, virtio-rng-pci, virtio-balloon-pci, virtio-mem-pci, vhost-vsock-pci,
# vmgenid and pcie-root-port, on a q35 started with -nodefaults, with the console on the
# ISA serial port. Everything below is that list and what it needs.
#
# Adding a line here is adding a device to every machine this repository builds, so it wants
# the same scrutiny as adding one to machine.go. Check it first with:
#
#     python3 scripts/minikconf.py --allnoconfig out /dev/null \
#         configs/devices/x86_64-softmmu/spin.mak Kconfig \
#         CONFIG_PIXMAN=y CONFIG_FDT=y CONFIG_GNUTLS=y CONFIG_VHOST_USER=y \
#         CONFIG_VHOST_KERNEL=y CONFIG_LINUX=y CONFIG_KVM=y CONFIG_X86_64=y \
#         CONFIG_TARGET_BIG_ENDIAN=n
#
# which resolves the whole tree in under a second. A symbol that does not exist is
# `undefined symbol NAME` there, and a link failure twenty minutes into `make` otherwise.
# CONFIG_VMGENID is the name that does not exist; the device is CONFIG_ACPI_VMGENID.
#
# CONFIG_VHOST_NET is not here and is not missing: it comes from `--enable-vhost-net` at
# configure (meson.build sets it from have_vhost_net), not from the device tree.

# --- the board ---------------------------------------------------------------------------
# q35 is the only machine started here. It brings the ICH9 southbridge, the ISA bus, the
# ACPI PM object, the RTC, the IOAPIC and fw_cfg — none of which is separable from it.
CONFIG_Q35=y

# PCIe root ports, for devices that arrive while the machine runs. machine.go places
# Spec.HotplugPorts of them at SlotHotplugBase and names them rp0..rpN.
CONFIG_PCIE_PORT=y

# --- the devices a VM gets -----------------------------------------------------------------
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_BLK=y
CONFIG_VIRTIO_NET=y
CONFIG_VIRTIO_RNG=y
CONFIG_VIRTIO_BALLOON=y
# vhost-vsock-pci: the host↔guest channel. The kernel does the datapath.
CONFIG_VHOST_VSOCK=y
# virtio-mem-pci: how a machine grows past its boot memory. ACPI DIMM slots are not used,
# which is why the machine string carries no slots=.
CONFIG_VIRTIO_MEM=y
# vmgenid: how a restored guest learns it was restored, so its random pool is reseeded.
CONFIG_ACPI_VMGENID=y

# --- the console ---------------------------------------------------------------------------
# One 16550 on ttyS0, which is where the kernel prints and where a debug boot's login lives.
# virtio-serial is deliberately absent: nothing in this repository or in the code that drives
# it ever attached a virtconsole, and the build asserted one existed until today.
CONFIG_SERIAL_ISA=y
