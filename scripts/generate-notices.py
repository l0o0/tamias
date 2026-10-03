#!/usr/bin/env python3
"""Collect installed Go and production npm license texts without network access."""
import hashlib
import json
import pathlib
import re
import subprocess

root = pathlib.Path(__file__).resolve().parents[1]
go_env = json.loads(subprocess.check_output(["go", "env", "-json", "GOMODCACHE", "GOROOT", "GOVERSION"], cwd=root))
go_mod = json.loads(subprocess.check_output(["go", "mod", "edit", "-json"], cwd=root))
groups = {}


def collect(name, directory):
    directory = pathlib.Path(directory)
    candidates = sorted(p for p in directory.iterdir() if p.is_file() and
                        re.match(r"^(licen[sc]e|copying|notice)(\..*)?$", p.name, re.I))
    if not any(re.match(r"^(licen[sc]e|copying)", p.name, re.I) for p in candidates):
        raise SystemExit(f"No license text found for {name}: {directory}")
    for p in candidates:
        text = p.read_text(encoding="utf-8").strip()
        digest = hashlib.sha256(text.encode()).hexdigest()
        group = groups.setdefault(digest, {"text": text, "packages": []})
        group["packages"].append(f"{name} — {p.name}")


go_root = pathlib.Path(go_env["GOROOT"])
if not (go_root / "LICENSE").exists() and (go_root.parent / "LICENSE").exists():
    go_root = go_root.parent  # Homebrew keeps LICENSE next to libexec.
collect("Go standard library " + go_env["GOVERSION"], go_root)
for item in sorted(go_mod["Require"], key=lambda x: x["Path"]):
    module = re.sub(r"[A-Z]", lambda m: "!" + m[0].lower(), item["Path"])
    collect(item["Path"] + "@" + item["Version"], pathlib.Path(go_env["GOMODCACHE"]) / (module + "@" + item["Version"]))

lock = json.loads((root / "frontend/package-lock.json").read_text())
for key, package in sorted(lock["packages"].items()):
    if not key or package.get("dev"):
        continue
    collect(key.removeprefix("node_modules/") + "@" + package["version"], root / "frontend" / key)

parts = ["tamiops — Third-party notices\n\n"
         "tamiops source code is licensed under AGPL-3.0-only; see LICENSE.\n"
         "The following dependencies retain their own licenses. This document\n"
         "contains Go module and production npm dependency license/notice texts.\n"
         "SQLite, embedded by go-sqlite3, is public domain: https://sqlite.org/copyright.html\n"
         "Linux system libraries bundled in the AppImage have additional notices\n"
         "under usr/share/doc inside the image.\n"]
for group in groups.values():
    parts.append("\n" + "=" * 72 + "\n" + "\n".join(group["packages"]) + "\n\n" + group["text"] + "\n")
(root / "THIRD_PARTY_NOTICES.txt").write_text("".join(parts), encoding="utf-8")
print(f"Wrote THIRD_PARTY_NOTICES.txt ({len(groups)} distinct license/notice texts)")
