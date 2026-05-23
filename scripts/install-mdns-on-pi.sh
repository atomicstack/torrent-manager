#!/usr/bin/env bash
# install-mdns-on-pi.sh — publish "tmgr.local" via avahi on this host.
#
# Run on a Raspberry Pi (or any debian-ish host with systemd + avahi). Idempotent.
# Requires sudo: the avahi-utils package install + the systemd unit write both
# need root.
set -euo pipefail

UNIT_NAME="mdns-tmgr"
UNIT_PATH="/etc/systemd/system/${UNIT_NAME}.service"
HOSTNAME_TO_PUBLISH="${HOSTNAME_TO_PUBLISH:-tmgr.local}"

# 1. ensure avahi-publish is on PATH
if ! command -v avahi-publish >/dev/null 2>&1; then
  echo "==> installing avahi-utils"
  sudo apt-get update -qq
  sudo apt-get install -y avahi-utils
fi

# 2. ensure avahi-daemon is running
if ! systemctl is-active --quiet avahi-daemon; then
  echo "==> starting avahi-daemon"
  sudo systemctl enable --now avahi-daemon
fi

# 3. resolve this host's primary LAN IPv4 (global scope; excludes 169.254/16)
host_ip="$(ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -1)"
if [ -z "${host_ip}" ]; then
  echo "ERROR: could not determine a global-scope ipv4 on this host" >&2
  exit 1
fi
echo "==> publishing ${HOSTNAME_TO_PUBLISH} -> ${host_ip}"

# 4. write the systemd unit if missing or outdated.
new_unit="$(cat <<UNIT
[Unit]
Description=avahi-publish for ${HOSTNAME_TO_PUBLISH}
After=avahi-daemon.service
Wants=avahi-daemon.service

[Service]
ExecStart=/usr/bin/avahi-publish -a -R ${HOSTNAME_TO_PUBLISH} ${host_ip}
Restart=always
RestartSec=5
# avahi-publish blocks; SIGTERM is enough to stop it.
KillMode=process

[Install]
WantedBy=multi-user.target
UNIT
)"

current_unit="$(sudo cat "${UNIT_PATH}" 2>/dev/null || true)"
if [ "${current_unit}" != "${new_unit}" ]; then
  echo "${new_unit}" | sudo tee "${UNIT_PATH}" >/dev/null
  sudo systemctl daemon-reload
fi

sudo systemctl enable --now "${UNIT_NAME}.service"

# 5. verify
echo "==> waiting for the record to settle"
sleep 2
if resolved="$(avahi-resolve -n "${HOSTNAME_TO_PUBLISH}" 2>/dev/null)"; then
  echo "==> ok: ${resolved}"
else
  echo "WARN: avahi-resolve could not look up ${HOSTNAME_TO_PUBLISH}; check journalctl -u ${UNIT_NAME}" >&2
fi

echo "==> done. status:"
systemctl status "${UNIT_NAME}.service" --no-pager --lines=5 || true
