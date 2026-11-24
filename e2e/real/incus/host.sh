#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# The entrypoint of the end-to-end run's Incus host: a privileged Debian container of its own
# cgroup namespace (scripts/real-forgejo.sh incus). It installs Incus, starts its daemon and
# initialises it with the API on :8443.
#
# cgroup v2 lets a cgroup enable controllers for its children only while it holds no
# processes itself. LXC places a container beside the cgroup of PID 1, pruning a trailing
# init.scope as on a systemd host: so PID 1 moves to init.scope, the daemon to a leaf of its
# own, and the namespace's root, left empty, enables cpu, memory and pids for the containers.
# Without that a container has no memory controller and no memory.peak.
set -eu
cg=/sys/fs/cgroup
mkdir -p "$cg/init.scope" "$cg/incusd"
# Right after the container's start the move can fail for a moment.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  echo $$ > "$cg/init.scope/cgroup.procs" 2>/dev/null && break
  sleep 1
done
echo "+cpu +memory +pids" > "$cg/cgroup.subtree_control"

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends incus-base dnsmasq-base ca-certificates openssl \
  socat >/dev/null

sh -c 'echo $$ > /sys/fs/cgroup/incusd/cgroup.procs && exec /usr/libexec/incus/incusd --group root' \
  > /var/log/incusd.log 2>&1 &
incus admin waitready --timeout 120
incus admin init --minimal
incus config set core.https_address :8443
# The local socket, also where the test reaches it: /berth is the run's directory.
socat UNIX-LISTEN:/berth/incus.socket,fork,mode=0600 UNIX-CONNECT:/var/lib/incus/unix.socket &
touch /run/berth-ready
wait
