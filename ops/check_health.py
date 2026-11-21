"""Real deployed check, run on the server. A degraded result is a failure, not completion."""
import json
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import Request, urlopen
from setup import read_env

root = Path(__file__).resolve().parent.parent
config = read_env(root / ".env")
request = Request("http://127.0.0.1:" + config.get("COMPANION_PORT", "8088") + "/health",
                  headers={"Authorization": "Bearer " + config["COMPANION_API_TOKEN"]})
try:
    response = urlopen(request, timeout=10)
except HTTPError as error:
    response = error
with response:
    report = json.loads(response.read())
    print(json.dumps(report, indent=2))
    if response.status != 200 or report["status"] != "ok":
        raise SystemExit("Real infrastructure verification FAILED; phase not complete")
    if not all(item["reachable"] and item["authenticated"] for item in report["dependencies"].values()):
        raise SystemExit("Real upstream authentication/reachability FAILED")
print("Enabled services verified; downloads " + ("enabled" if report.get("downloads_enabled", True) else "disabled"))
