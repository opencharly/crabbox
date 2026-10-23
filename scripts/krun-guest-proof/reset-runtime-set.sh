#!/usr/bin/env bash
# Restore the runner image's baseline for the krun proof.
#
# This fleet REUSES runner VMs across jobs: a job's installed krun runtime set
# (crun/krun, libkrun, libkrunfw and the containerd handler) was measured
# present in the NEXT job, on a different VM of the same tier. A proof whose
# R7 negative half asserts "the presence assertion fails before the install"
# is therefore only meaningful after a reset, and the venue is left clean for
# whatever job lands next.
set -euo pipefail

sudo rm -f /usr/bin/krun
sudo rm -f /usr/bin/crun
sudo rm -f /usr/local/lib64/libkrun.so*
sudo rm -f /usr/local/lib64/pkgconfig/libkrun.pc
sudo rm -f /usr/local/include/libkrun.h
sudo rm -f /usr/local/include/libkrun_display.h
sudo rm -f /usr/local/include/libkrun_input.h
sudo rm -f /usr/local/lib/x86_64-linux-gnu/libkrunfw.so*
sudo rm -f /etc/ld.so.conf.d/krun.conf
sudo ldconfig

# Remove only OUR config (the one declaring the krun handler); a config the
# image ships is left untouched.
if [ -f /etc/containerd/config.toml ] &&
   grep -q 'containerd\.runtimes\.krun' /etc/containerd/config.toml; then
  sudo rm -f /etc/containerd/config.toml
fi
sudo systemctl restart containerd
echo "krun-runtime-set=reset"
