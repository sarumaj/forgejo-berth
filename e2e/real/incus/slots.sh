#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Makes the end-to-end run's slots on its Incus host (scripts/real-forgejo.sh incus), run
# there: a Debian container with forgejo-runner (/berth/forgejo-runner) as a systemd service,
# copied to each slot and snapshot clean, as the incus provider expects them. It then trusts
# the broker's client certificate, restricted to the slots' project, and leaves the server's
# certificate and the client's in /berth.
#
#   slots.sh PROJECT SLOT...
set -eu
project=$1
shift
if [ "$project" != default ]; then
  # The default project's profiles and images, the containers and volumes its own.
  incus project create "$project" -c features.profiles=false -c features.images=false
fi
incus project switch "$project"
# Privileged: the host has 65536 ids of its own, too few to map a container's. A host that
# confines its containers with AppArmor (a rootless Docker's) refuses LXC a profile of its
# own: the container keeps the one it is in.
incus profile set default security.privileged=true
incus profile set default raw.lxc=lxc.apparmor.profile=unchanged
incus launch -q images:debian/13 berth-base
booted() {
  # shellcheck disable=SC2016 # the script runs in the container
  incus exec "$1" -- sh -c 'for _ in $(seq 120); do
    case $(systemctl is-system-running 2>/dev/null) in running|degraded) exit 0 ;; esac; sleep 1
  done; exit 1'
}
booted berth-base
incus file push -q --mode 0755 /berth/forgejo-runner berth-base/usr/local/bin/forgejo-runner
incus exec berth-base -- sh -euc '
useradd --system --create-home --home-dir /var/lib/forgejo-runner --shell /usr/sbin/nologin runner
install -d -o runner -g runner -m 0700 /var/lib/forgejo-runner/work /var/cache/forgejo-runner
install -d -m 0750 -g runner /etc/forgejo-runner
cat > /etc/systemd/system/forgejo-runner.service <<UNIT
[Unit]
Description=forgejo-runner, one job
After=network-online.target
Wants=network-online.target

[Service]
User=runner
WorkingDirectory=/var/lib/forgejo-runner
ExecStart=/usr/local/bin/forgejo-runner --config /etc/forgejo-runner/config.yml one-job --wait
UNIT
systemctl daemon-reload'
incus stop berth-base
# Forgejo by its name on the Docker network, which the containers' DNS does not know: in
# /etc/hosts, which the image's template writes anew at a copy's first start.
forgejo=$(getent hosts forgejo | awk '{print $1; exit}')
for slot in "$@"; do
  incus copy berth-base "$slot"
  incus start "$slot"
  booted "$slot"
  incus exec "$slot" -- sh -c "echo '$forgejo forgejo' >> /etc/hosts"
  incus stop "$slot"
  incus snapshot create "$slot" clean
done
incus delete berth-base

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 \
  -subj /CN=forgejo-berth-e2e -keyout /berth/client.key -out /berth/client.crt 2>/dev/null
incus config trust add-certificate /berth/client.crt --restricted --projects "$project"
cp /var/lib/incus/server.crt /berth/server.crt
