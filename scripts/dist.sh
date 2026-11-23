#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
# Builds the release archives into dist/: static binaries (CGO_ENABLED=0, the SQLite driver
# is pure Go) for linux/amd64 and linux/arm64, each with the licence and the README, and
# their SHA-256 checksums. $1 is the version built in (a tag, or a commit for a test build).
set -eu
version=${1:?usage: scripts/dist.sh VERSION}
rm -rf dist && mkdir dist
for arch in amd64 arm64; do
  name="forgejo-berth_${version#v}_linux_${arch}"
  mkdir "dist/$name"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "dist/$name/forgejo-berth" ./cmd/forgejo-berth
  cp LICENSE README.md "dist/$name/"
  tar -C dist -czf "dist/$name.tar.gz" "$name"
  rm -r "dist/$name"
done
(cd dist && sha256sum ./*.tar.gz | sed 's# \./# #' > checksums.txt)
