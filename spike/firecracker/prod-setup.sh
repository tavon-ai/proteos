#!/usr/bin/env bash

sudo mkdir -p /var/lib/proteos/images /srv/jailer
sudo cp ~/fc-spike/images/vmlinux           /var/lib/proteos/images/vmlinux
sudo cp ~/fc-spike/images/ubuntu-24.04.ext4 /var/lib/proteos/images/ubuntu-24.04.ext4
sudo cp ~/fc-spike/bin/firecracker ~/fc-spike/bin/jailer /usr/local/bin/
