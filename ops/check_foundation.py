"""Read the server-local token and verify the deployed foundation without printing secrets."""
import json
from pathlib import Path
from urllib.request import Request, urlopen
from urllib.error import HTTPError
from setup import read_env

root = Path(__file__).resolve().parent.parent
config = read_env(root / ".env")
base = "http://127.0.0.1:" + config.get("COMPANION_PORT", "8088")
for path in ("/live", "/"):
    request = Request(base + path, headers={"Authorization": "Bearer " + config["COMPANION_API_TOKEN"]})
    with urlopen(request, timeout=5) as response:
        print(path, response.status, json.loads(response.read()))

routes = [
    ("GET", "/api/v1/acquisition/search"),
    ("POST", "/api/v1/acquisition/jobs"),
    ("GET", "/api/v1/acquisition/jobs"),
    ("GET", "/api/v1/acquisition/jobs/check"),
    ("DELETE", "/api/v1/acquisition/jobs/check"),
    ("POST", "/api/v1/tagging/jobs"),
    ("GET", "/api/v1/tagging/jobs"),
    ("GET", "/api/v1/tagging/jobs/check"),
    ("DELETE", "/api/v1/tagging/jobs/check"),
    ("POST", "/api/v1/library/scan"),
]
for method, path in routes:
    request = Request(base + path, method=method,
                      headers={"Authorization": "Bearer " + config["COMPANION_API_TOKEN"]})
    try:
        urlopen(request, timeout=5)
        raise RuntimeError("Stub unexpectedly succeeded: " + path)
    except HTTPError as error:
        assert error.code == 501, (method, path, error.code)
        assert json.loads(error.read())["error"] == "not_implemented"
print(f"All {len(routes)} deployed REST stubs returned 501 not_implemented")
