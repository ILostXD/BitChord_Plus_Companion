"""One-time server configuration; never overwrite an existing secret file."""
import os
from pathlib import Path
import secrets

root = Path(__file__).resolve().parent.parent
target = root / ".env"
if target.exists():
    print("Existing .env preserved")
else:
    fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as config:
        config.write((root / ".env.example").read_text().replace(
            "replace-with-a-long-random-token", secrets.token_urlsafe(48)))
    print("Private .env created; configure upstream credentials before the health phase")
