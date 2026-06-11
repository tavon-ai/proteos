# Pinned versions + shared config for the Firecracker spike (plan Task 2.0).
# Every numbered script sources this file. Change values here, nowhere else.
# shellcheck disable=SC2034  # variables are consumed by the sourcing scripts

# --- pinned versions -------------------------------------------------------
FC_VERSION="v1.16.0"   # firecracker + jailer release tag (pinned 2026-06-10)
# CI artifact bucket prefix. NOTE: the CI kernel/rootfs bucket lags the binary
# release — there is no firecracker-ci/v1.16 line yet, so we pin to the newest
# published line (v1.15). Bump this only once the bucket publishes a match.
CI_VERSION="v1.15"
UBUNTU_VERSION="24.04" # rootfs flavor published by the Firecracker CI bucket

FC_RELEASE_URL="https://github.com/firecracker-microvm/firecracker/releases"
CI_BUCKET="https://s3.amazonaws.com/spec.ccfc.min"

ARCH="$(uname -m)" # the spike assumes x86_64 (matches the Proxmox cluster)

# --- host layout ------------------------------------------------------------
# Everything the spike creates lives under WORK_DIR, outside the repo, so a
# full cleanup is `07-teardown.sh --all` (or just deleting this directory).
WORK_DIR="${FC_SPIKE_WORK_DIR:-$HOME/fc-spike}"
BIN_DIR="$WORK_DIR/bin"
IMG_DIR="$WORK_DIR/images"
RUN_DIR="$WORK_DIR/run"
JAIL_DIR="$WORK_DIR/jail"

KERNEL="$IMG_DIR/vmlinux" # exact upstream key recorded in versions.lock by 01
ROOTFS="$IMG_DIR/ubuntu-$UBUNTU_VERSION.ext4"
SSH_KEY="$IMG_DIR/ubuntu-$UBUNTU_VERSION.id_rsa"
DATA_DISK="$IMG_DIR/data.ext4"

API_SOCK="$RUN_DIR/firecracker.sock"
VM_LOG="$RUN_DIR/vm-console.log"
SCREEN_SESSION="fc-spike"
SNAPSHOT_DIR="$RUN_DIR/snapshot"

# --- VM shape ----------------------------------------------------------------
VCPUS=2
MEM_MIB=1024

# --- networking (03 onward) ---------------------------------------------------
TAP_DEV="tap-spike0"
HOST_IP="172.16.0.1"
GUEST_IP="172.16.0.2"
NET_MASK="255.255.255.0"
NET_CIDR="172.16.0.0/24"
GUEST_MAC="06:00:AC:10:00:02"
# Kernel-cmdline static IP config — avoids needing console access or DHCP.
NET_BOOT_ARGS="ip=$GUEST_IP::$HOST_IP:$NET_MASK::eth0:off"

# --- jailer (06) ---------------------------------------------------------------
JAIL_ID="spike"
FC_USER="fc-spike"

# --- vsock (08) ----------------------------------------------------------------
# Fixed guest CID + port for every VM (Phase 3 decision #3): the host never uses
# AF_VSOCK, so a shared CID 3 is fine — each VM has its own host-side uds. The
# host connects to the uds and asks Firecracker's hybrid handshake to reach the
# guest port (CONNECT <port>\n → OK <port>\n).
GUEST_CID=3
VSOCK_PORT=1024
VSOCK_UDS="$RUN_DIR/v.sock" # plain-boot uds (06/jailer uses an in-chroot path)
