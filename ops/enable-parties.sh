#!/usr/bin/env bash
# Lets the companion's Listen Together party server (and the Requests refresh) through nginx.
# Run once on the server: sudo bash ops/enable-parties.sh [/etc/nginx/sites-enabled/glaze-jam]
set -euo pipefail
site="${1:-/etc/nginx/sites-enabled/glaze-jam}"
marker="# BEGIN BitChord+ parties"
if grep -q "$marker" "$site"; then
  echo "Already enabled in $site"
  exit 0
fi
cp "$site" "$site.bak-$(date +%Y%m%d%H%M%S)"
block=$(cat <<'NGINX'
    # BEGIN BitChord+ parties
    location = /api/v1/acquisition/refresh {
        limit_except POST { deny all; }
        proxy_pass http://127.0.0.1:8088;
    }
    location ~ ^/(healthz|api/time|api/presence|api/parties(/.*)?|api/stats/live.*|invite/.*)$ {
        client_max_body_size 16k;
        proxy_pass http://127.0.0.1:8088;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
    location ^~ /ws/parties/ {
        proxy_pass http://127.0.0.1:8088;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_read_timeout 3600s;
    }
    # END BitChord+ parties
NGINX
)
# Before the catch-all 404 of the HTTPS server.
python3 - "$site" "$block" <<'PY'
import sys
path, block = sys.argv[1], sys.argv[2]
s = open(path).read()
anchor = "    location / { return 404; }"
i = s.rfind(anchor)
if i < 0:
    sys.exit("catch-all location not found; add the block by hand")
open(path, "w").write(s[:i] + block + "\n" + s[i:])
PY
if nginx -t; then
  systemctl reload nginx
  echo "Enabled. Check: curl -s https://jam.andyhserver.duckdns.org/healthz"
else
  echo "nginx -t failed; restoring the backup"
  cp "$(ls -t "$site".bak-* | head -1)" "$site"
  exit 1
fi
