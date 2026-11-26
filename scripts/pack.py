#!/usr/bin/env python3
"""Build a deterministic .yampkg for athena/assistant.

yam-publish on Windows writes backslashes into MANIFEST.yaml and drops the
executable bit. This script emits the same package layout directly with POSIX
paths and correct modes.
"""
from __future__ import annotations

import hashlib
import io
import os
import re
import sys
import tarfile
from pathlib import Path

import zstandard

ROOT = Path(__file__).resolve().parent.parent
FILES = [
    ("bin/athena-postinstall", ROOT / "scripts" / "postinstall.sh", 0o755),
    ("share/athena.assistant/bin/athena", ROOT / "dist" / "athena", 0o755),
    ("share/athena.assistant/bin/negatives.bin", ROOT / "plugin" / "bin" / "negatives.bin", 0o644),
    ("share/athena.assistant/plugin.toml", ROOT / "plugin" / "plugin.toml", 0o644),
]


def read_meta() -> tuple[str, str]:
    text = (ROOT / "yam.yaml").read_text(encoding="utf-8")
    version = re.search(r"^version:\s*(\S+)", text, re.M)
    desc = re.search(r'^description:\s*"([^"]*)"', text, re.M)
    if not version or not desc:
        raise SystemExit("version/description not found in yam.yaml")
    return version.group(1), desc.group(1)


def main() -> None:
    version, description = read_meta()
    for _, src, _ in FILES:
        if not src.is_file():
            raise SystemExit(f"missing file: {src}")

    hashes = {dst: hashlib.sha256(src.read_bytes()).hexdigest() for dst, src, _ in FILES}
    manifest = io.StringIO()
    manifest.write(f"name: athena/assistant\nversion: {version}\nlicense: Apache-2.0\n")
    manifest.write(f"description: '{description}'\n")
    manifest.write("homepage: https://github.com/ruslan200065/AthenaAssistantYAM\n")
    manifest.write("target: yandexstation_2\narch: aarch64\n")
    for key in ("depends", "recommends", "enhances", "provides", "conflicts", "replaces", "permissions"):
        manifest.write(f"{key}: []\n")
    manifest.write("entware: null\nservices: {}\npostinstall:\n- bin/athena-postinstall\n")
    manifest.write("files_sha256:\n")
    for dst in hashes:
        manifest.write(f"  {dst}: {hashes[dst]}\n")
    manifest_bytes = manifest.getvalue().encode("utf-8")

    out = ROOT / "dist" / f"athena-assistant-{version}.yampkg"
    tmp = out.with_suffix(".tmp")
    with open(tmp, "wb") as raw:
        with zstandard.ZstdCompressor(level=19).stream_writer(raw, closefd=False) as z:
            with tarfile.open(fileobj=z, mode="w|", format=tarfile.PAX_FORMAT) as tar:
                def add(name: str, data: bytes, mode: int) -> None:
                    info = tarfile.TarInfo(name)
                    info.size = len(data)
                    info.mode = mode
                    info.mtime = 0
                    info.uid = 0
                    info.gid = 0
                    info.uname = ""
                    info.gname = ""
                    tar.addfile(info, io.BytesIO(data))

                add("MANIFEST.yaml", manifest_bytes, 0o644)
                for dst, src, mode in FILES:
                    add("files/" + dst, src.read_bytes(), mode)

    os.replace(tmp, out)
    print(out)
    print(f"{out.stat().st_size} bytes")


if __name__ == "__main__":
    main()
