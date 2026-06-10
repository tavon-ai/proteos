#!/usr/bin/env bash
# 03 — Boot a microVM with a tap NIC + NAT and verify connectivity all three
# ways: host → guest (private IP), SSH with our generated key, and
# guest → internet (via NAT).
#
# Proves: the tap + private-IP model from the plan's Networking section. The
# guest gets its static IP from the kernel cmdline (no DHCP, no console pokes).

cd "$(dirname "${BASH_SOURCE[0]}")"
source ./env.sh
source ./lib.sh

[[ -f $KERNEL && -f $ROOTFS ]] || die "kernel/rootfs missing — run ./01-host-setup.sh first"

kill_vm
setup_network

start_firecracker
put_machine_config
put_boot_source "$NET_BOOT_ARGS"
put_rootfs
put_network # must happen before InstanceStart — NICs can't be added later
start_instance
wait_for_ssh

# The kernel ip= param doesn't set DNS; give the guest a resolver for the test.
guest_ssh "echo 'nameserver 1.1.1.1' > /etc/resolv.conf"

log "guest → internet (through NAT):"
guest_ssh "curl -sI --max-time 10 https://example.com | head -n1" ||
  die "guest cannot reach the internet — check iptables FORWARD/MASQUERADE rules"
ok "guest has internet egress"

log "host → guest:"
ping -c1 -W2 "$GUEST_IP" >/dev/null || die "host cannot ping the guest"
ok "host reaches guest at $GUEST_IP"

ok "network path proven: host ↔ tap ↔ guest, guest → NAT → internet"
log "shell into the guest: ssh -i $SSH_KEY root@$GUEST_IP"
log "next: ./04-attach-disk.sh"
