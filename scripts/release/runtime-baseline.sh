#!/usr/bin/env bash
set -euo pipefail
commit=${1:-HEAD}
{
  git show "$commit:Dockerfile" | awk '/^ARG (ALPINE_IMAGE|POSTGRES_IMAGE)=/ {print} /^FROM \${ALPINE_IMAGE}/ {runtime=1} runtime {print}'
  git show "$commit:deploy/docker-entrypoint.sh"
  git ls-tree -r "$commit" tools/tosub2-login
} | sha256sum | cut -d' ' -f1
