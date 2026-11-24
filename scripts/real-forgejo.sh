#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Runs the broker against a throwaway Forgejo and forgejo-runner in Docker (rootless works):
# a Forgejo with an admin, a trusted owner alice and another owner carol, a repository of
# each with a workflow; slots that are runner containers (e2e/real). Everything is removed
# afterwards. Images: FORGEJO_IMAGE, RUNNER_IMAGE; the port on 127.0.0.1: PORT.
#
#   scripts/real-forgejo.sh          slots played by the test, the runner in host mode
#   scripts/real-forgejo.sh docker   the docker provider on the same engine (DOCKER_HOST, else
#                                    /var/run/docker.sock): a Docker of each job's own
#   scripts/real-forgejo.sh incus    the incus provider on a real Incus in a privileged
#                                    container (e2e/real/incus, image INCUS_HOST_IMAGE), its
#                                    API on 127.0.0.1:INCUS_PORT, the slots in INCUS_PROJECT
set -eu
variant=${1:-}
case "$variant" in
  "") test=TestRealForgejo ;;
  docker) test=TestRealForgejoDocker ;;
  incus) test=TestRealForgejoIncus ;;
  *) echo "usage: $0 [docker|incus]" >&2; exit 2 ;;
esac
forgejo_image=${FORGEJO_IMAGE:-codeberg.org/forgejo/forgejo:17}
runner_image=${RUNNER_IMAGE:-data.forgejo.org/forgejo/runner:13}
port=${PORT:-13017}
incus_image=${INCUS_HOST_IMAGE:-docker.io/library/debian:trixie}
incus_port=${INCUS_PORT:-13018}
incus_project=${INCUS_PROJECT:-default}
network=berth-real-$$
name=berth-forgejo-$$
incus=berth-incus-$$
work=$(mktemp -d)
cleanup() {
  docker rm -f -v "$name" "$incus" >/dev/null 2>&1 || true
  docker ps -aq --filter name=berth-slot- | xargs -r docker rm -f -v >/dev/null 2>&1 || true
  docker ps -aq --filter label=org.forgejo-berth.host=real-e2e | xargs -r docker rm -f -v >/dev/null 2>&1 || true
  docker volume ls -q --filter label=org.forgejo-berth.host=real-e2e | xargs -r docker volume rm >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -f "$work/admin.token" "$work/alice.token"
  rm -rf "$work/incus"
  rmdir "$work"
}
trap cleanup EXIT
trap "exit 1" INT TERM

docker network create "$network" >/dev/null
docker run -d --name "$name" --network "$network" --network-alias forgejo -p "127.0.0.1:$port:3000" \
  -e FORGEJO__security__INSTALL_LOCK=true -e FORGEJO__database__DB_TYPE=sqlite3 \
  -e FORGEJO__server__ROOT_URL=http://forgejo:3000/ -e FORGEJO__actions__ENABLED=true \
  "$forgejo_image" >/dev/null
url=http://127.0.0.1:$port
for _ in $(seq 90); do curl -sf "$url/api/v1/version" >/dev/null && break; sleep 2; done

user() { docker exec -u git "$name" forgejo admin user "$@"; }
user create --admin --username root --password Root-pass-123 --email root@example.org \
  --must-change-password=false >/dev/null
user create --username alice --password Alice-pass-123 --email alice@example.org \
  --must-change-password=false >/dev/null
user create --username carol --password Carol-pass-123 --email carol@example.org \
  --must-change-password=false >/dev/null
(umask 077
 user generate-access-token --username root --token-name berth --raw \
   --scopes write:admin,read:repository,write:repository > "$work/admin.token"
 user generate-access-token --username alice --token-name berth --raw \
   --scopes write:user,write:repository > "$work/alice.token")

repo() { # token file, API path, name
  curl -fsS -H "Authorization: token $(cat "$1")" -H 'Content-Type: application/json' \
    -d "{\"name\":\"$3\",\"auto_init\":true,\"default_branch\":\"main\"}" "$url/api/v1$2" >/dev/null
}
repo "$work/alice.token" /user/repos app
repo "$work/admin.token" /admin/users/carol/repos site

if [ "$variant" = incus ]; then
  # The Incus host: Incus in a privileged container on the test's network, with the slots,
  # the runner's binary taken from its image (e2e/real/incus).
  mkdir "$work/incus"
  runner=$(docker create "$runner_image")
  docker cp -q "$runner:/bin/forgejo-runner" "$work/incus/forgejo-runner"
  docker rm "$runner" >/dev/null
  docker run -d --name "$incus" --privileged --cgroupns=private --network "$network" \
    -p "127.0.0.1:$incus_port:8443" -v "$work/incus:/berth" -v "$PWD/e2e/real/incus:/scripts:ro" \
    --entrypoint /scripts/host.sh "$incus_image" >/dev/null
  for _ in $(seq 150); do docker exec "$incus" test -e /run/berth-ready 2>/dev/null && break; sleep 2; done
  docker exec "$incus" test -e /run/berth-ready || { docker logs "$incus" 2>&1 | tail -20; exit 1; }
  docker exec "$incus" /scripts/slots.sh "$incus_project" a b
fi

BERTH_REAL_URL=$url BERTH_REAL_INNER_URL=http://forgejo:3000 \
  BERTH_REAL_ADMIN_TOKEN_FILE="$work/admin.token" BERTH_REAL_ALICE_TOKEN_FILE="$work/alice.token" \
  BERTH_REAL_RUNNER_IMAGE="$runner_image" BERTH_REAL_NETWORK="$network" \
  BERTH_REAL_DOCKER_HOST="${DOCKER_HOST:-unix:///var/run/docker.sock}" \
  BERTH_REAL_INCUS_HOST="$incus" BERTH_REAL_INCUS_ADDRESS="https://127.0.0.1:$incus_port" \
  BERTH_REAL_INCUS_DIR="$work/incus" BERTH_REAL_INCUS_PROJECT="$incus_project" \
  go test -tags realforgejo -run "^$test\$" -count=1 -timeout 25m -v ./e2e/real/
