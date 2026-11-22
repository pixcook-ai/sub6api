#!/usr/bin/env bash
set -euo pipefail
commit=$(git rev-parse HEAD)
date=$(git show -s --format=%cI HEAD)
version=$(sh backend/scripts/resolve-version.sh)
output="$PWD/dist/deployment"
mkdir -p "$output"
(cd backend && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embed -trimpath \
  -ldflags="-s -w -X main.Version=$version -X main.Commit=$commit -X main.Date=$date -X main.BuildType=release" \
  -o "$output/sub2api" ./cmd/server)
cp scripts/release/deploy-sub6api.sh "$output/deploy.sh"
printf '%s\n' "$commit" > "$output/COMMIT"
git ls-tree -r HEAD backend/migrations | sha256sum | cut -d' ' -f1 > "$output/MIGRATIONS_SHA256"
bash scripts/release/runtime-baseline.sh > "$output/RUNTIME_SHA256"
tar -czf "$output/resources.tar.gz" -C backend resources
(cd "$output" && sha256sum sub2api deploy.sh COMMIT MIGRATIONS_SHA256 RUNTIME_SHA256 resources.tar.gz > SHA256SUMS)
