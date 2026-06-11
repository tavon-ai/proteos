# Shared helpers for the Firecracker spike. Sourced after env.sh.

set -euo pipefail

log() { printf '\e[1;34m[spike]\e[0m %s\n' "$*"; }
ok() { printf '\e[1;32m[ ok ]\e[0m %s\n' "$*"; }
die() {
  printf '\e[1;31m[fail]\e[0m %s\n' "$*" >&2
  exit 1
}

require() {
  local cmd
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || die "missing command: $cmd (see 00-proxmox-vm.md for packages)"
  done
}

# wait_for <description> <timeout-seconds> <command...>
# Polls the command every 0.5s until it succeeds or the timeout expires.
wait_for() {
  local desc=$1 timeout=$2 i
  shift 2
  for ((i = 0; i < timeout * 2; i++)); do
    if "$@" >/dev/null 2>&1; then
      ok "$desc"
      return 0
    fi
    sleep 0.5
  done
  die "timed out (${timeout}s) waiting for: $desc"
}

# --- Firecracker process + API ----------------------------------------------

# fc_api <METHOD> <path> [json-body] — talk to the VMM over its unix socket.
# On an HTTP error we print Firecracker's JSON fault_message (which a bare
# `curl -f` would swallow, leaving only "curl: (22) ... error 400") and return
# non-zero so callers under `set -e` abort with a useful reason.
fc_api() {
  local method=$1 path=$2 reqbody=${3:-}
  local args=(--unix-socket "$API_SOCK" -sS -X "$method"
    "http://localhost$path" -H 'Content-Type: application/json' -w '\n%{http_code}')
  [[ -n $reqbody ]] && args+=(-d "$reqbody")
  local out status respbody
  out=$(curl "${args[@]}") || return 1
  status=${out##*$'\n'}   # trailing line is the http_code from -w
  respbody=${out%$'\n'*}  # everything before it is the response body
  if ((status < 200 || status >= 300)); then
    printf '\e[1;31m[fail]\e[0m %s %s → HTTP %s: %s\n' "$method" "$path" "$status" "$respbody" >&2
    return 1
  fi
  [[ -n $respbody ]] && printf '%s' "$respbody"
  return 0
}

fc_running() { [[ -S $API_SOCK ]] && fc_api GET / >/dev/null 2>&1; }
vm_exited() { ! fc_running; }
sock_exists() { [[ -S $API_SOCK ]]; }

# /dev/kvm access is granted by 01 via setfacl, but that ACL is per-boot and is
# wiped by reboots / udev re-applying its rules — so re-check it before every
# boot and fail with the fix instead of a cryptic 400 from InstanceStart.
require_kvm() {
  [[ -r /dev/kvm && -w /dev/kvm ]] && return 0
  die "/dev/kvm not read/writable as $USER (the setfacl grant is per-boot). Re-grant with:
    sudo setfacl -m u:$USER:rw /dev/kvm          # transient — just this boot
  or make it survive reboots:
    sudo usermod -aG kvm $USER && newgrp kvm     # then re-run from this shell"
}

# Start the VMM inside a detached screen session so the serial console stays
# attachable (`screen -r fc-spike`) and is also captured to $VM_LOG.
start_firecracker() {
  require_kvm
  mkdir -p "$RUN_DIR"
  rm -f "$API_SOCK" "$VM_LOG"
  screen -dmS "$SCREEN_SESSION" -L -Logfile "$VM_LOG" \
    "$BIN_DIR/firecracker" --api-sock "$API_SOCK"
  wait_for "Firecracker API socket" 10 sock_exists
}

kill_vm() {
  screen -S "$SCREEN_SESSION" -X quit >/dev/null 2>&1 || true
  pkill -f "firecracker --api-sock $API_SOCK" 2>/dev/null || true
  rm -f "$API_SOCK"
}

# --- VM configuration (pre-boot PUTs against the API) -------------------------

put_machine_config() {
  fc_api PUT /machine-config "{\"vcpu_count\": $VCPUS, \"mem_size_mib\": $MEM_MIB}"
}

# put_boot_source [extra-boot-args]
put_boot_source() {
  fc_api PUT /boot-source "{
    \"kernel_image_path\": \"$KERNEL\",
    \"boot_args\": \"console=ttyS0 reboot=k panic=1 pci=off ${1:-}\"
  }"
}

put_rootfs() {
  fc_api PUT /drives/rootfs "{
    \"drive_id\": \"rootfs\",
    \"path_on_host\": \"$ROOTFS\",
    \"is_root_device\": true,
    \"is_read_only\": false
  }"
}

put_data_disk() {
  fc_api PUT /drives/spikedata "{
    \"drive_id\": \"spikedata\",
    \"path_on_host\": \"$DATA_DISK\",
    \"is_root_device\": false,
    \"is_read_only\": false
  }"
}

put_network() {
  fc_api PUT /network-interfaces/eth0 "{
    \"iface_id\": \"eth0\",
    \"guest_mac\": \"$GUEST_MAC\",
    \"host_dev_name\": \"$TAP_DEV\"
  }"
}

# put_vsock <uds-path> — attach a virtio-vsock device (pre-boot, like NICs;
# Firecracker cannot hot-add). Firecracker creates <uds-path> and listens on it
# for host-initiated connects; guest-initiated connects land on <uds-path>_<port>.
put_vsock() {
  fc_api PUT /vsock "{
    \"guest_cid\": $GUEST_CID,
    \"uds_path\": \"${1:-$VSOCK_UDS}\"
  }"
}

start_instance() { fc_api PUT /actions '{"action_type": "InstanceStart"}'; }

# vsock_echo <uds-path> <message> — host→guest round trip over the hybrid
# handshake: connect the uds, send "CONNECT <port>\n", expect "OK <port>\n", then
# send <message> and read it back from the guest's echo listener. Prints the
# echoed bytes; returns non-zero on handshake or echo failure. Uses python3
# (always present on the CI rootfs and the Proxmox host) for precise framing.
vsock_echo() {
  local uds=$1 msg=$2
  UDS="$uds" PORT="$VSOCK_PORT" MSG="$msg" python3 - <<'PY'
import os, socket, sys
uds, port, msg = os.environ["UDS"], int(os.environ["PORT"]), os.environ["MSG"]
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(5)
s.connect(uds)
s.sendall(f"CONNECT {port}\n".encode())
# Read the "OK <port>\n" handshake line byte-by-byte (don't over-read into data).
line = b""
while not line.endswith(b"\n"):
    b = s.recv(1)
    if not b:
        sys.exit("vsock: connection closed during handshake")
    line += b
if not line.startswith(b"OK"):
    sys.exit(f"vsock: unexpected handshake reply: {line!r}")
s.sendall(msg.encode())
got = b""
while len(got) < len(msg):
    chunk = s.recv(len(msg) - len(got))
    if not chunk:
        break
    got += chunk
sys.stdout.write(got.decode(errors="replace"))
PY
}

console_has_login() { grep -q 'login:' "$VM_LOG"; }
wait_for_boot() { wait_for "guest boot (login prompt on serial console)" 30 console_has_login; }

# --- guest access over SSH ----------------------------------------------------

guest_ssh() {
  ssh -i "$SSH_KEY" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=5 -o LogLevel=ERROR \
    "root@$GUEST_IP" "$@"
}

guest_up() { guest_ssh true; }
wait_for_ssh() { wait_for "SSH to guest at $GUEST_IP" 30 guest_up; }

# --- host networking (tap + NAT), idempotent -----------------------------------

egress_dev() {
  ip route get 8.8.8.8 | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i + 1); exit}}'
}

setup_network() {
  local egress
  egress="$(egress_dev)"
  [[ -n $egress ]] || die "could not determine egress interface"

  if ! ip link show "$TAP_DEV" >/dev/null 2>&1; then
    sudo ip tuntap add "$TAP_DEV" mode tap user "$USER"
    sudo ip addr add "$HOST_IP/24" dev "$TAP_DEV"
    sudo ip link set "$TAP_DEV" up
    log "created $TAP_DEV ($HOST_IP/24)"
  fi
  sudo sysctl -wq net.ipv4.ip_forward=1

  # -C checks for the rule first so reruns don't stack duplicates.
  sudo iptables -t nat -C POSTROUTING -s "$NET_CIDR" -o "$egress" -j MASQUERADE 2>/dev/null ||
    sudo iptables -t nat -A POSTROUTING -s "$NET_CIDR" -o "$egress" -j MASQUERADE
  sudo iptables -C FORWARD -i "$TAP_DEV" -o "$egress" -j ACCEPT 2>/dev/null ||
    sudo iptables -A FORWARD -i "$TAP_DEV" -o "$egress" -j ACCEPT
  sudo iptables -C FORWARD -i "$egress" -o "$TAP_DEV" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null ||
    sudo iptables -A FORWARD -i "$egress" -o "$TAP_DEV" -m state --state RELATED,ESTABLISHED -j ACCEPT
}

teardown_network() {
  local egress
  egress="$(egress_dev)"
  sudo iptables -t nat -D POSTROUTING -s "$NET_CIDR" -o "$egress" -j MASQUERADE 2>/dev/null || true
  sudo iptables -D FORWARD -i "$TAP_DEV" -o "$egress" -j ACCEPT 2>/dev/null || true
  sudo iptables -D FORWARD -i "$egress" -o "$TAP_DEV" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
  sudo ip link del "$TAP_DEV" 2>/dev/null || true
}
