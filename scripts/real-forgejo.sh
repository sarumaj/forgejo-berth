#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Runs the broker against a throwaway Forgejo and forgejo-runner in Docker (rootless works):
# a Forgejo with an admin, a trusted owner alice and another owner carol, a repository of
# each with a workflow; slots that are runner containers (e2e/real). Everything is removed
# afterwards. Images: FORGEJO_IMAGE, RUNNER_IMAGE; the port on 127.0.0.1: PORT.
set -eu
forgejo_image=${FORGEJO_IMAGE:-codeberg.org/forgejo/forgejo:17}
runner_image=${RUNNER_IMAGE:-data.forgejo.org/forgejo/runner:13}
port=${PORT:-13017}
network=berth-real-$$
name=berth-forgejo-$$
work=$(mktemp -d)
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker ps -aq --filter name=berth-slot- | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -f "$work/admin.token" "$work/alice.token"
  rmdir "$work"
}
trap cleanup EXIT

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

BERTH_REAL_URL=$url BERTH_REAL_INNER_URL=http://forgejo:3000 \
  BERTH_REAL_ADMIN_TOKEN_FILE="$work/admin.token" BERTH_REAL_ALICE_TOKEN_FILE="$work/alice.token" \
  BERTH_REAL_RUNNER_IMAGE="$runner_image" BERTH_REAL_NETWORK="$network" \
  go test -tags realforgejo -run TestRealForgejo -count=1 -timeout 25m -v ./e2e/real/
