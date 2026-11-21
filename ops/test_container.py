"""Local packaging check; requires Docker and a built Companion image. No real credentials."""
import argparse
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import Request, urlopen
from urllib.error import HTTPError, URLError
from setup import write_env


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--docker", default="docker")
    parser.add_argument("--image", default="bitchord-plus-companion:local")
    parser.add_argument("--compose-only", action="store_true", help="Validate Compose without a running Docker engine")
    args = parser.parse_args()
    def docker(*parts):
        result = subprocess.run([args.docker, *parts], capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError(result.stderr)
        return result.stdout

    root = Path(__file__).resolve().parent.parent
    with tempfile.TemporaryDirectory() as directory:
        local = Path(directory)
        for name in ("compose.jam.yaml", "compose.downloads.yaml"):
            shutil.copyfile(root / name, local / name)
        values = {"COMPANION_API_TOKEN": "test-only-" + "x" * 32, "DOWNLOADS_ENABLED": "false",
                  "NAVIDROME_PASSWORD": "literal$HOME'password", "SLSKD_API_KEY": "literal$'key",
                  "NAVIDROME_LIBRARY_HOST_PATH": str(local / "music"), "SLSKD_DOWNLOAD_HOST_PATH": str(local / "downloads")}
        write_env(local / ".env", values)
        for downloads in (False, True):
            command = ["compose", "--project-directory", str(local), "-f", str(local / "compose.jam.yaml")]
            if downloads:
                command += ["-f", str(local / "compose.downloads.yaml")]
            data = json.loads(docker(*command, "config", "--format", "json"))
            service = data["services"]["companion"]
            # Compose config re-escapes literal dollars so its output can be reused as input.
            assert service["environment"]["NAVIDROME_PASSWORD"] == values["NAVIDROME_PASSWORD"].replace("$", "$$")
            assert service["environment"]["SLSKD_API_KEY"] == values["SLSKD_API_KEY"].replace("$", "$$")
            assert service["read_only"] and service["user"] == "10001:10001"
            targets = {volume["target"] for volume in service["volumes"]}
            assert ("/music" in targets) == downloads and ("/downloads" in targets) == downloads

    if args.compose_only:
        print("Compose literal secrets, optional mounts and non-root/read-only configuration passed")
        return

    class Navidrome(BaseHTTPRequestHandler):
        def do_GET(self):
            body = json.dumps({"subsonic-response": {"status": "ok", "starred2": {"artist": []}}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        def log_message(self, *args):
            pass

    fixture = ThreadingHTTPServer(("0.0.0.0", 0), Navidrome)
    threading.Thread(target=fixture.serve_forever, daemon=True).start()
    token = "test-only-" + "x" * 32
    container = ""
    try:
        container = docker("run", "--rm", "--detach", "--read-only", "--cap-drop=ALL",
                           "--security-opt=no-new-privileges:true", "--tmpfs", "/tmp:size=2147483648,mode=1777",
                           "--mount", "type=volume,target=/state", "--publish", "127.0.0.1::8080",
                           "--env", "COMPANION_API_TOKEN=" + token, "--env", "DOWNLOADS_ENABLED=false",
                           "--env", "COMPANION_STATE_PATH=/state", "--env", "NAVIDROME_USERNAME=owner",
                           "--env", "NAVIDROME_PASSWORD=test-only-password",
                           "--env", f"NAVIDROME_API_URL=http://host.docker.internal:{fixture.server_port}", args.image).strip()
        info = json.loads(docker("inspect", container))[0]
        port = info["NetworkSettings"]["Ports"]["8080/tcp"][0]["HostPort"]
        def get(path, authenticated=True):
            request = Request(f"http://127.0.0.1:{port}" + path,
                              headers={"Authorization": "Bearer " + token} if authenticated else {})
            try:
                response = urlopen(request, timeout=10)
            except HTTPError as error:
                response = error
            with response:
                return response.status, json.load(response)
        for attempt in range(20):
            try:
                status, health = get("/health")
                break
            except (URLError, ConnectionError):
                time.sleep(0.25)
        assert status == 200 and health["state_writable"] and not health["downloads_enabled"], health
        assert "slskd" not in health["dependencies"] and health["dependencies"]["navidrome"]["authenticated"]
        status, capabilities = get("/api/v1/capabilities")
        assert status == 200 and capabilities["jam_streaming"] and not capabilities["downloads"]
        assert get("/api/v1/capabilities", False)[0] == 401
        print("Compose literal secrets, optional mounts and read-only non-root Jam-only runtime passed")
    finally:
        if container:
            docker("stop", "--time", "3", container)
        fixture.shutdown(); fixture.server_close()


if __name__ == "__main__":
    main()
