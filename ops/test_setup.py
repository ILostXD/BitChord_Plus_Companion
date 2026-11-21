import tempfile
from unittest.mock import patch
from pathlib import Path
import setup
from setup import quote, read_env, write_env, check

def test():
    values = {"TOKEN": "abc$HOME#value'quote", "FOLDER": "C:\\Music\\Artist's album", "EMPTY": ""}
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / ".env"
        write_env(path, values)
        assert read_env(path) == values
        previous = path.read_bytes()
        try:
            write_env(path, {"TOKEN": "bad\nvalue"})
            raise AssertionError("newlines accepted")
        except ValueError:
            pass
        assert path.read_bytes() == previous, "invalid config overwrote existing settings"
        assert not list(path.parent.glob(".glaze-env-*")), "private temp file leaked"
    assert not check({}), "check without configuration claimed success"
    for mode in ("disabled", "key", "credentials", "qobuz_first"):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "ops").mkdir()
            library, downloads = root / "music", root / "downloads"
            library.mkdir(); downloads.mkdir()
            answers = ["glaze-test", "http://navidrome:4533", "owner", "no" if mode == "disabled" else "yes"]
            passwords = ["nav$'password"]
            if mode != "disabled":
                auth_mode = "credentials" if mode == "qobuz_first" else mode
                answers += ["http://slskd:5030", auth_mode]
                if auth_mode == "credentials":
                    answers += ["web-user"]
                passwords += ["slsk$'secret"]
                answers += [str(library), str(downloads), "10001", "10001"]
                if mode == "qobuz_first":
                    answers += ["yes", "qobuz-user", "invalid", "qobuz"]
                    passwords += ["qobuz-secret"]
                else:
                    answers += ["no"]
                passwords += [""]  # Optional fingerprint application key.
            passwords += [""]  # Optional Last.fm metadata key, including Jam-only setups.
            answers += ["no"]  # Never start services during the check.
            with patch.object(setup, "__file__", str(root / "ops" / "setup.py")), patch("sys.argv", ["setup.py"]), \
                 patch("builtins.input", side_effect=answers), patch.object(setup.getpass, "getpass", side_effect=passwords), \
                 patch.object(setup.subprocess, "run") as deployment:
                setup.main()
                deployment.assert_not_called()
            configured = read_env(root / ".env")
            if mode == "qobuz_first":
                assert configured["ACQUISITION_PROVIDER_ORDER"] == "qobuz,slskd"
                assert configured["QOBUZ_USERNAME"] == "qobuz-user" and configured["QOBUZ_PASSWORD"] == "qobuz-secret"
            assert configured["DOWNLOADS_ENABLED"] == str(mode != "disabled").lower()
            assert configured["NAVIDROME_PASSWORD"] == "nav$'password"
            assert configured["LASTFM_API_KEY"] == ""
            if mode == "key":
                assert configured["SLSKD_API_KEY"] == "slsk$'secret" and configured["SLSKD_PASSWORD"] == ""
            elif mode == "credentials":
                assert configured["SLSKD_API_KEY"] == "" and configured["SLSKD_USERNAME"] == "web-user"
                assert configured["SLSKD_PASSWORD"] == "slsk$'secret"
    print("Setup quoting, private atomic update, preservation and missing-config checks passed")

if __name__ == "__main__":
    test()
