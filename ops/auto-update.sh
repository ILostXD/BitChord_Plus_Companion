#!/bin/sh
# Pulls the repository and rebuilds the container when there are new commits.
# Safe to run often: with nothing new it does nothing, and the image build runs the tests first.
set -eu
cd "$(dirname "$0")/.."
git fetch --quiet origin main
if [ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ]; then
  exit 0
fi
echo "$(date -u +%FT%TZ) updating to $(git rev-parse --short origin/main)"
git merge --ff-only origin/main
docker compose build
docker compose up -d --remove-orphans
echo "$(date -u +%FT%TZ) done"
