"""Read and staged-repair verification on generated audio only. Requires ffmpeg and Mutagen."""
import base64
import json
import subprocess
import sys
import tempfile
from pathlib import Path

import mutagen
from mutagen.flac import Picture
from mutagen.id3 import APIC, TALB, TIT2, TPE1, TXXX
from mutagen.mp4 import MP4Cover


def run(*args, data=None):
    return subprocess.run(args, input=data, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, check=True).stdout


def check(helper):
    cover = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aX1sAAAAASUVORK5CYII=")
    patch = {"TITLE": ["Confirmed title"], "ARTIST": ["Tyler, The Creator", "Another Artist"], "ALBUM": ["Confirmed album"]}
    with tempfile.TemporaryDirectory(prefix="glaze-taglib-test-") as directory:
        for extension, codec in [("flac", "flac"), ("mp3", "libmp3lame"), ("ogg", "libvorbis"), ("opus", "libopus"), ("m4a", "aac")]:
            filename = Path(directory) / ("fixture." + extension)
            run("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:a", codec, str(filename))
            audio = mutagen.File(filename)
            if audio.tags is None:
                audio.add_tags()
            if extension == "mp3":
                audio.tags.add(TIT2(encoding=3, text=["old title"]))
                audio.tags.add(TPE1(encoding=3, text=["old artist"]))
                audio.tags.add(TALB(encoding=3, text=["old album"]))
                audio.tags.add(TXXX(encoding=3, desc="CUSTOM", text=["keep me"]))
                audio.tags.add(APIC(encoding=3, mime="image/png", type=3, data=cover))
                audio.tags.save(filename, v2_version=3)
            elif extension == "m4a":
                audio.tags["\xa9nam"] = ["old title"]
                audio.tags["\xa9ART"] = ["old artist"]
                audio.tags["\xa9alb"] = ["old album"]
                audio.tags["----:com.apple.iTunes:CUSTOM"] = [b"keep me"]
                audio.tags["covr"] = [MP4Cover(cover, imageformat=MP4Cover.FORMAT_PNG)]
                audio.save()
            else:
                audio.tags["title"], audio.tags["artist"], audio.tags["album"] = ["old title"], ["old artist"], ["old album"]
                audio.tags["CUSTOM"] = ["keep me"]
                if extension == "flac":
                    picture = Picture()
                    picture.type, picture.mime, picture.data = 3, "image/png", cover
                    audio.add_picture(picture)
                audio.save()
            original_bytes = filename.read_bytes()
            filename.chmod(0o444)
            before = json.loads(run(helper, str(filename)))
            assert before['properties']['TITLE'] == ['old title']
            assert before['properties']['ARTIST'] == ['old artist']
            assert before['properties']['ALBUM'] == ['old album']
            assert before['duration_ms'] > 0
            rejected = subprocess.run([helper, str(filename)], input=json.dumps(patch).encode(), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            assert rejected.returncode != 0
            assert filename.read_bytes() == original_bytes, (extension, "metadata inspection changed bytes")
            filename.chmod(0o644)
            pcm_before = run("ffmpeg", "-v", "error", "-i", str(filename), "-f", "s16le", "-")
            verified_patch = {"TITLE": ["Confirmed title"], "ARTIST": ["Confirmed artist"], "ALBUM": ["Confirmed album"]}
            after = json.loads(run(helper, "--repair-download", str(filename), data=json.dumps(verified_patch).encode()))
            for key, value in verified_patch.items():
                assert after["properties"][key] == value, extension
            assert pcm_before == run("ffmpeg", "-v", "error", "-i", str(filename), "-f", "s16le", "-"), (extension, "decoded audio changed")
            audio = mutagen.File(filename)
            if extension == "mp3":
                assert audio.tags.getall("APIC")[0].data == cover
                assert str(audio.tags.getall("TXXX")[0]) == "keep me"
            elif extension == "m4a":
                assert audio.tags["covr"][0] == cover
                assert audio.tags["----:com.apple.iTunes:CUSTOM"][0] == b"keep me"
            else:
                assert audio.tags["CUSTOM"] == ["keep me"]
                if extension == "flac":
                    assert audio.pictures[0].data == cover
            print(extension + ": inspection, staged repair, original audio/art/custom tags passed")



if __name__ == "__main__":
    check(sys.argv[1] if len(sys.argv) > 1 else "glaze-taglib")
