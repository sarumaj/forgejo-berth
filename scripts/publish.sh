#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Publishes dist/ as the release of $TAG through the Forgejo API ($API: the repository's API
# URL, $TOKEN: a token that may write releases). The notes are CHANGELOG.md's section of the
# version, else the commits since the previous tag. Run again, it replaces the files.
set -eu
: "${TAG:?}" "${API:?}" "${TOKEN:?}"
version=${TAG#v}
notes=$(awk -v v="$version" '/^## / { if (found) exit; h = $2; gsub(/^\[|\]$/, "", h); sub(/^v/, "", h)
  if (h == v) { found = 1; next } } found' CHANGELOG.md 2>/dev/null || true)
if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
  previous=$(git describe --tags --abbrev=0 "$TAG^" 2>/dev/null || true)
  notes=$(git log --no-merges --format='- %s' ${previous:+"$previous.."}"$TAG")
fi
auth="Authorization: token $TOKEN"
body=$(python3 -c 'import json, sys; print(json.dumps({"tag_name": sys.argv[1], "name": sys.argv[1], "body": sys.argv[2]}))' "$TAG" "$notes")
id=$(curl -fsS -H "$auth" "$API/releases/tags/$TAG" 2>/dev/null | python3 -c 'import json, sys; print(json.load(sys.stdin)["id"])' || true)
if [ -z "$id" ]; then
  id=$(curl -fsS -H "$auth" -H 'Content-Type: application/json' -d "$body" "$API/releases" |
    python3 -c 'import json, sys; print(json.load(sys.stdin)["id"])')
else
  curl -fsS -X PATCH -H "$auth" -H 'Content-Type: application/json' -d "$body" "$API/releases/$id" >/dev/null
fi
assets=$(curl -fsS -H "$auth" "$API/releases/$id/assets")
for file in dist/*; do
  name=$(basename "$file")
  old=$(printf '%s' "$assets" | python3 -c 'import json, sys
print(" ".join(str(a["id"]) for a in json.load(sys.stdin) if a["name"] == sys.argv[1]))' "$name")
  for asset in $old; do
    curl -fsS -X DELETE -H "$auth" "$API/releases/$id/assets/$asset"
  done
  curl -fsS -H "$auth" -F "attachment=@$file" "$API/releases/$id/assets?name=$name" >/dev/null
  echo "uploaded $name"
done
