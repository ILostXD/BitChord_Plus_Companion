#!/bin/sh
# Run on the companion host as root after deploying the release and storage APIs.
set -eu
target=/etc/nginx/sites-available/glaze-jam
source=$(cd "$(dirname "$0")/.." && pwd)/ops/nginx-jam.conf
backup="$target.before-presave-$(date -u +%Y%m%dT%H%M%SZ)"
test -f "$target"
test -f "$source"
cp -p "$target" "$backup"
install -m 644 "$source" "$target"
if nginx -t; then
    systemctl reload nginx
else
    cp -p "$backup" "$target"
    exit 1
fi
