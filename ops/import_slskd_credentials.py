"""Reuse existing slskd web/API credentials server-side; never alter slskd or print secrets.

Needs PyYAML on the host (usually already present), not in the service image.
"""
import os
from pathlib import Path
import sys
import yaml

source = Path(sys.argv[1])
config_path = Path(__file__).resolve().parent.parent / ".env"
settings = yaml.safe_load(source.read_text())
# Standard slskd config, also accept a single named root wrapper.
if "web" not in settings and len(settings) == 1:
    settings = next(iter(settings.values()))
auth = settings.get("web", {}).get("authentication", {})
keys = auth.get("api_keys", {})
key = next((item.get("key") for item in keys.values()
            if isinstance(item, dict) and item.get("key")), "")
values = {"SLSKD_API_KEY": key, "SLSKD_USERNAME": auth.get("username", ""),
          "SLSKD_PASSWORD": auth.get("password", "")}
if not key and not (values["SLSKD_USERNAME"] and values["SLSKD_PASSWORD"]):
    raise SystemExit("No existing slskd API/web credentials found; configure companion .env manually")

lines = config_path.read_text().splitlines()
for index, line in enumerate(lines):
    name = line.partition("=")[0]
    if name in values and line == name + "=" and values[name]:
        value = str(values[name])
        if "\n" in value or "\r" in value:
            raise SystemExit("Multiline credentials require manual configuration")
        lines[index] = name + "='" + value.replace("'", "\\'") + "'"
config_path.write_text("\n".join(lines) + "\n")
os.chmod(config_path, 0o600)
print("Existing slskd credentials copied into private companion config; no upstream changes")
