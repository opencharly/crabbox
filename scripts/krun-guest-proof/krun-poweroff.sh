#!/bin/sh
# systemd invokes every /usr/lib/systemd/system-shutdown/* hook with the
# shutdown verb as $1. libkrun has no ACPI power button, so a plain poweroff
# never reaches the VMM; the libkrunfw kernel turns the forced reboot into a
# VM exit instead (CONTAINERD-IMAGES.md §2, pod-crabbox-systemd — the only
# clean krun stop).
case "${1:-}" in
  poweroff|halt) exec systemctl reboot -ff ;;
esac
