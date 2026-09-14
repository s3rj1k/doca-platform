#!/usr/bin/env bash
# Host readiness checks for DPF host-trusted provisioning over PCIe rshim.
# Read-only, mutates nothing. k0smotron-nv.yml sets these up, this only verifies.
set -uo pipefail

# Config comes from demo.env, source it from any location beforehand.
: "${DPU_PCI:?source demo.env first, for example  set -a && source demo.env && set +a}"

echo "== preflight"

# sysfs rather than parsing lspci. grep -q exits on first match, lspci takes
# SIGPIPE, and pipefail turns that into a false negative.
if [ ! -e "/sys/bus/pci/devices/0000:${DPU_PCI}/resource2" ]; then
  echo "PCIe BAR2 absent, rshim cannot attach." >&2
  echo "Run: sudo mstconfig -y -d ${DPU_PCI} set PF_BAR2_ENABLE=1 && sudo reboot" >&2
  exit 1
fi
echo "   BAR2 present"

# There is one BAR2, so a host rshim and DPF's rshim contend for it and neither
# reliably wins. Masked rather than disabled so an upgrade cannot re-enable it.
if systemctl is-active --quiet rshim; then
  echo "Host rshim.service is running and will contend with DPF." >&2
  echo "Run: sudo systemctl mask --now rshim" >&2
  exit 1
fi
if [ "$(systemctl is-enabled rshim 2>/dev/null)" = "enabled" ]; then
  echo "Host rshim.service is enabled and will start on next boot." >&2
  echo "Run: sudo systemctl mask rshim" >&2
  exit 1
fi
echo "   host rshim masked, DPF owns BAR2"

# rshim needs cuse and vfio_pci to publish /dev/rshim0. k0smotron-nv.yml loads
# them because DPF's sidecar mounts only /dev and cannot modprobe them itself.
for mod in cuse vfio_pci; do
  if ! grep -qE "^${mod} " /proc/modules; then
    echo "Kernel module ${mod} is not loaded, rshim cannot publish a device." >&2
    echo "Run: sudo modprobe ${mod}   (k0smotron-nv.yml loads it persistently)" >&2
    exit 1
  fi
done
echo "   cuse and vfio_pci loaded"

echo "host preflight OK"
