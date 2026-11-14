#!/usr/bin/env bash
# One-time setup of a fresh Ubuntu 24.04 benchmark machine. Safe to run twice. Run it from the repository
# root as a normal user with sudo. It installs Docker and Go, switches off what would take CPU or disk during a
# run, builds the binaries and writes down what the machine looks like. It does not start the stores or run
# anything: scripts/vm/run-pinned.sh does that.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck disable=SC1091
. scripts/vm/env.sh
step() { printf '\n== %s\n' "$*"; }

go_version="$(awk '/^go /{print $2}' go.mod)"
facts=benchmark-scratch/vm-setup
mkdir -p "$facts"

step "packages (Docker Engine and Compose from the Ubuntu archive, Python for one checksum lookup)"
sudo apt-get update -q
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q docker.io docker-compose-v2 util-linux curl ca-certificates python3 sysstat jq
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER" # takes effect from the next login; every later ssh command is a new login

step "Go $go_version (checksum taken from go.dev and verified)"
if /usr/local/go/bin/go version 2>/dev/null | grep -q "go$go_version "; then
  echo "already installed"
else
  archive="go${go_version}.linux-amd64.tar.gz"
  want="$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' | python3 -c '
import json, sys
ver, name = sys.argv[1], sys.argv[2]
for rel in json.load(sys.stdin):
    if rel["version"] == "go" + ver:
        for f in rel["files"]:
            if f["filename"] == name:
                print(f["sha256"])
                sys.exit(0)
sys.exit(1)
' "$go_version" "$archive")" || { echo "no checksum found for $archive" >&2; exit 1; }
  curl -fsSLo "/tmp/$archive" "https://go.dev/dl/$archive"
  echo "$want  /tmp/$archive" | sha256sum -c -
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "/tmp/$archive"
  rm -f "/tmp/$archive"
fi
go version

step "switch off what would steal CPU or disk during a run (the Google guest agent stays: ssh needs it)"
for unit in unattended-upgrades.service apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service \
  motd-news.timer packagekit.service fwupd.service snapd.service snapd.socket google-osconfig-agent.service google-cloud-ops-agent.service; do
  if systemctl cat "$unit" >/dev/null 2>&1; then
    sudo systemctl disable --now "$unit" >/dev/null 2>&1 && echo "stopped and disabled $unit" || echo "could not stop $unit (continuing)"
  fi
done

step "build the binaries"
scripts/build.sh

step "what this machine is (kept for the record)"
{ date -u +%FT%TZ; uname -srm; lscpu; echo; lscpu -e; } >"$facts/cpu.txt"
{ systemctl list-units --type=service --state=running --no-legend; } >"$facts/running-services.txt"
{ docker --version; docker compose version; go version; free -m; df -h /; } >"$facts/software-and-disk.txt" 2>&1 || true
cat "$facts/cpu.txt" | sed -n '1,25p'
echo "..."
echo "running services after setup: $(wc -l <"$facts/running-services.txt") (list in $facts/running-services.txt)"
echo
echo "Setup finished. Next: scripts/vm/dry-run.sh"
