#!/usr/bin/env bash

set -euo pipefail

sudo mkdir -p /opt/proteos /etc/proteos
sudo cp -r /home/ivan/proteos /opt/proteos/src
sudo cp /home/ivan/node-agent.env /etc/proteos/node-agent.env
sudo cp /opt/proteos/src/deploy/node-agent/proteos-node-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now proteos-node-agent
journalctl -u proteos-node-agent -f
