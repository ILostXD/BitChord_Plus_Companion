"""Create a per-job streamrip config without putting credentials on the command line."""

import hashlib
import os
import sys
from pathlib import Path

from streamrip.config import ConfigData
from tomlkit import dumps


def main() -> None:
    config_path, download_dir = map(Path, sys.argv[1:3])
    cfg = ConfigData.defaults().toml
    cfg["qobuz"]["email_or_userid"] = os.environ["QOBUZ_USERNAME"]
    cfg["qobuz"]["password_or_token"] = hashlib.md5(
        os.environ["QOBUZ_PASSWORD"].encode()
    ).hexdigest()
    cfg["qobuz"]["quality"] = 4
    cfg["qobuz"]["download_booklets"] = False
    cfg["downloads"]["folder"] = str(download_dir)
    cfg["downloads"]["disc_subdirectories"] = False
    cfg["downloads"]["source_subdirectories"] = False
    cfg["database"]["downloads_enabled"] = False
    cfg["database"]["failed_downloads_enabled"] = False
    cfg["artwork"]["save_artwork"] = False
    cfg["filepaths"]["folder_format"] = "album"
    cfg["filepaths"]["add_singles_to_folder"] = True
    cfg["cli"]["text_output"] = False
    cfg["cli"]["progress_bars"] = False
    cfg["misc"]["check_for_updates"] = False
    config_path.write_text(dumps(cfg), encoding="utf-8")
    config_path.chmod(0o600)


if __name__ == "__main__":
    main()
