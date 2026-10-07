import hashlib
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


SOURCE_ROOT = Path(__file__).resolve().parents[2]
HELPER = SOURCE_ROOT / "scripts" / "publish-release.sh"
SOURCE_SHA = "a" * 40


FAKE_GIT = r'''#!/usr/bin/env python3
import os
import sys

if len(sys.argv) < 2 or sys.argv[1] != "ls-remote":
    raise SystemExit("unexpected git command: " + repr(sys.argv[1:]))
refs = [arg for arg in sys.argv[2:] if arg.startswith("refs/tags/")]
if not refs:
    raise SystemExit("missing tag ref arguments")
tag_ref = refs[0]
sha = os.environ.get("FAKE_TAG_SHA", "a" * 40)
peeled = os.environ.get("FAKE_PEELED_SHA", "")
if os.environ.get("FAKE_TAG_MISSING") == "1":
    raise SystemExit(0)
print(sha + "\t" + tag_ref)
if peeled:
    print(peeled + "\t" + tag_ref + "^{}")
'''


FAKE_GH = r'''#!/usr/bin/env python3
import json
import os
import shutil
import sys
from pathlib import Path

state_path = Path(os.environ["FAKE_GH_STATE"])
asset_dir = Path(os.environ["FAKE_GH_ASSETS"])
state = json.loads(state_path.read_text())
args = sys.argv[1:]

def save():
    state_path.write_text(json.dumps(state))

def event(value):
    state.setdefault("events", []).append(value)
    save()

def asset_names():
    release = state.get("release")
    return [] if release is None else [a["name"] for a in release.get("assets", [])]

def emit_api(status, payload):
    labels = {200: "OK", 401: "Unauthorized", 403: "Forbidden", 404: "Not Found", 500: "Internal Server Error"}
    sys.stdout.write("HTTP/2.0 %d %s\r\nContent-Type: application/json\r\n\r\n" % (status, labels.get(status, "Error")))
    sys.stdout.write(json.dumps(payload) + "\n")
    raise SystemExit(0 if 200 <= status < 300 else 1)

if args[:1] == ["api"]:
    endpoint = args[-1]
    event(["api", endpoint])
    if state.get("api_error_status"):
        status = int(state["api_error_status"])
        emit_api(status, {"message": "simulated API failure"})
    if "/releases/tags/" not in endpoint or state.get("release") is None:
        emit_api(404, {"message": "Not Found"})
    release = state["release"]
    emit_api(200, {
        "draft": release.get("draft", False),
        "prerelease": release.get("prerelease", False),
        "immutable": release.get("immutable", False),
        "assets": [{"name": name, "id": index + 1} for index, name in enumerate(asset_names())],
    })

if args[:2] == ["release", "create"]:
    event(["create"] + args[2:])
    state["release"] = {"draft": True, "prerelease": "--prerelease" in args, "immutable": False, "assets": []}
    save()
    raise SystemExit(0)

if args[:2] == ["release", "upload"]:
    tag = args[2]
    clobber = "--clobber" in args
    paths = [Path(arg) for arg in args[3:] if not arg.startswith("--") and arg != os.environ.get("GH_REPO")]
    names = [path.name for path in paths]
    event(["upload", tag, clobber, names])
    release = state.get("release")
    if release is None or release.get("immutable"):
        print("release cannot be modified", file=sys.stderr)
        raise SystemExit(1)
    current = asset_names()
    for path in paths:
        if not path.is_file():
            print("missing upload source: " + str(path), file=sys.stderr)
            raise SystemExit(1)
        if path.name in current and not clobber:
            print("asset already exists: " + path.name, file=sys.stderr)
            raise SystemExit(1)
    for path in paths:
        destination = asset_dir / path.name
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(path, destination)
        if path.name not in current:
            release["assets"].append({"name": path.name})
            current.append(path.name)
    save()
    raise SystemExit(0)

if args[:2] == ["release", "download"]:
    tag = args[2]
    pattern = args[args.index("--pattern") + 1]
    destination = Path(args[args.index("--dir") + 1])
    event(["download", tag, pattern])
    if pattern not in asset_names():
        print("asset not found: " + pattern, file=sys.stderr)
        raise SystemExit(1)
    destination.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(asset_dir / pattern, destination / pattern)
    raise SystemExit(0)

if args[:2] == ["release", "edit"]:
    event(["edit"] + args[2:])
    if state.get("release") is None:
        raise SystemExit(1)
    if "--draft=false" in args:
        state["release"]["draft"] = False
    save()
    raise SystemExit(0)

raise SystemExit("unexpected gh command: " + repr(args))
'''


FAKE_SHA256SUM = r'''#!/usr/bin/env python3
import hashlib
import os
import sys
from pathlib import Path

args = [arg for arg in sys.argv[1:] if arg != "--"]
for name in args:
    path = Path(name)
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    print(digest + "  " + path.name)
'''


class PublishReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.repo = self.base / "repo"
        self.bin = self.base / "fake-bin"
        self.remote_assets = self.base / "remote-assets"
        self.repo.mkdir()
        self.bin.mkdir()
        self.remote_assets.mkdir()
        (self.repo / "scripts").mkdir()
        shutil.copyfile(HELPER, self.repo / "scripts" / "publish-release.sh")
        (self.repo / "dist").mkdir()
        self.state_path = self.base / "gh-state.json"
        self.state_path.write_text(json.dumps({"release": None, "events": []}))
        for name, source in (("git", FAKE_GIT), ("gh", FAKE_GH), ("sha256sum", FAKE_SHA256SUM)):
            path = self.bin / name
            path.write_text(source)
            path.chmod(0o755)
        self.version = "1.2.3"
        self.make_dist()

    def asset_names(self):
        return [
            f"tamias-{self.version}-macos-arm64.pkg",
            f"tamias-{self.version}-macos-amd64.pkg",
            f"tamias-{self.version}-windows-amd64-setup.exe",
            f"tamias-{self.version}-linux-amd64.AppImage",
        ]

    def make_dist(self):
        for name in self.asset_names():
            (self.repo / "dist" / name).write_bytes(("built:" + name).encode())

    def local_asset_hashes(self):
        return {
            name: hashlib.sha256((self.repo / "dist" / name).read_bytes()).hexdigest()
            for name in self.asset_names()
        }

    def create_sum_file(self):
        lines = [self.local_asset_hashes()[name] + "  " + name for name in self.asset_names()]
        (self.repo / "dist" / "SHA256SUMS.txt").write_text("\n".join(lines) + "\n")

    def seed_release(self, *, draft=False, prerelease=False, immutable=False, assets=()):
        release = {"draft": draft, "prerelease": prerelease, "immutable": immutable, "assets": []}
        for name, content in assets:
            (self.remote_assets / name).write_bytes(content)
            release["assets"].append({"name": name})
        self.state_path.write_text(json.dumps({"release": release, "events": []}))

    def env(self, **overrides):
        result = os.environ.copy()
        result.update({
            "PATH": str(self.bin) + os.pathsep + result.get("PATH", ""),
            "VERSION": self.version,
            "SOURCE_SHA": SOURCE_SHA,
            "GH_REPO": "tamiops/tamias",
            "GH_TOKEN": "fake-token",
            "FAKE_TAG_SHA": SOURCE_SHA,
            "FAKE_GH_STATE": str(self.state_path),
            "FAKE_GH_ASSETS": str(self.remote_assets),
            "TMPDIR": str(self.base),
        })
        result.update(overrides)
        return result

    def state(self):
        return json.loads(self.state_path.read_text())

    def run_helper(self, **env):
        return subprocess.run(
            ["bash", str(self.repo / "scripts" / "publish-release.sh")],
            cwd=self.repo,
            env=self.env(**env),
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

    def test_new_formal_release_uses_notes_and_publishes(self):
        notes = self.repo / "docs" / "releases"
        notes.mkdir(parents=True)
        (notes / f"{self.version}.md").write_text("Release notes\n")

        result = self.run_helper()

        self.assertEqual(result.returncode, 0, result.stderr)
        state = self.state()
        self.assertFalse(state["release"]["draft"])
        self.assertFalse(state["release"]["prerelease"])
        self.assertEqual(set(a["name"] for a in state["release"]["assets"]), set(self.asset_names() + ["SHA256SUMS.txt"]))
        create = next(e for e in state["events"] if e[0] == "create")
        self.assertIn("--verify-tag", create)
        self.assertIn("--draft", create)
        self.assertIn("--notes-file", create)
        self.assertNotIn("--prerelease", create)
        self.assertTrue(any(e[0] == "edit" and "--draft=false" in e for e in state["events"]))

    def test_new_prerelease_uses_generated_notes_and_prerelease_flag(self):
        self.version = "1.2.4-beta.1"
        (self.repo / "dist").mkdir(exist_ok=True)
        self.make_dist()

        result = self.run_helper()

        self.assertEqual(result.returncode, 0, result.stderr)
        create = next(e for e in self.state()["events"] if e[0] == "create")
        self.assertIn("--generate-notes", create)
        self.assertIn("--prerelease", create)

    def test_existing_draft_is_reuploaded_and_published(self):
        old_assets = [(name, b"old:" + name.encode()) for name in self.asset_names()]
        old_assets.append(("SHA256SUMS.txt", b"old checksum\n"))
        self.seed_release(draft=True, assets=old_assets)

        result = self.run_helper()

        self.assertEqual(result.returncode, 0, result.stderr)
        state = self.state()
        self.assertFalse(state["release"]["draft"])
        uploads = [e for e in state["events"] if e[0] == "upload"]
        self.assertEqual(len(uploads), 1)
        self.assertTrue(uploads[0][2])
        self.assertEqual(set(uploads[0][3]), set(self.asset_names() + ["SHA256SUMS.txt"]))

    def test_published_release_adds_missing_assets_then_rerun_skips(self):
        first = self.asset_names()[0]
        existing = b"already published bytes"
        self.seed_release(assets=[(first, existing)])

        first_run = self.run_helper()
        self.assertEqual(first_run.returncode, 0, first_run.stderr)
        state = self.state()
        self.assertEqual((self.remote_assets / first).read_bytes(), existing)
        upload = next(e for e in state["events"] if e[0] == "upload")
        self.assertFalse(upload[2])
        self.assertEqual(set(upload[3]), set(self.asset_names()[1:] + ["SHA256SUMS.txt"]))

        state["events"] = []
        self.state_path.write_text(json.dumps(state))
        second_run = self.run_helper()
        self.assertEqual(second_run.returncode, 0, second_run.stderr)
        self.assertFalse(any(e[0] == "upload" for e in self.state()["events"]))

    def test_published_partial_release_validates_existing_sum_and_only_adds_missing(self):
        self.create_sum_file()
        first = self.asset_names()[0]
        first_bytes = b"published first package"
        (self.repo / "dist" / first).write_bytes(first_bytes)
        lines = []
        for name in self.asset_names():
            content = first_bytes if name == first else (self.repo / "dist" / name).read_bytes()
            lines.append(hashlib.sha256(content).hexdigest() + "  " + name)
        sum_bytes = ("\n".join(lines) + "\n").encode()
        self.seed_release(assets=[(first, first_bytes), ("SHA256SUMS.txt", sum_bytes)])

        result = self.run_helper()

        self.assertEqual(result.returncode, 0, result.stderr)
        upload = next(e for e in self.state()["events"] if e[0] == "upload")
        self.assertFalse(upload[2])
        self.assertEqual(set(upload[3]), set(self.asset_names()[1:]))
        self.assertEqual((self.remote_assets / "SHA256SUMS.txt").read_bytes(), sum_bytes)

    def test_missing_dist_asset_fails_before_github_calls(self):
        (self.repo / "dist" / self.asset_names()[0]).unlink()

        result = self.run_helper()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("required release asset is missing", result.stderr)
        self.assertFalse(self.state()["events"])

    def test_moved_tag_fails_before_release_lookup(self):
        result = self.run_helper(FAKE_TAG_SHA="b" * 40)

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("expected SOURCE_SHA", result.stderr)
        self.assertFalse(self.state()["events"])

    def test_annotated_tag_is_compared_by_peeled_commit_sha(self):
        result = self.run_helper(FAKE_TAG_SHA="b" * 40, FAKE_PEELED_SHA=SOURCE_SHA)

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.state()["release"]["draft"])

    def test_api_401_is_not_treated_as_missing_release(self):
        self.state_path.write_text(json.dumps({"release": None, "events": [], "api_error_status": 401}))

        result = self.run_helper()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("HTTP 401", result.stderr)
        self.assertFalse(any(e[0] == "create" for e in self.state()["events"]))

    def test_immutable_incomplete_release_fails_without_upload(self):
        self.seed_release(immutable=True, assets=[(self.asset_names()[0], b"old")])

        result = self.run_helper()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("immutable release", result.stderr)
        self.assertFalse(any(e[0] == "upload" for e in self.state()["events"]))

    def test_immutable_complete_release_is_verified_and_skipped(self):
        package_bytes = {}
        for name in self.asset_names():
            package_bytes[name] = ("immutable:" + name).encode()
        sum_lines = [hashlib.sha256(package_bytes[name]).hexdigest() + "  " + name for name in self.asset_names()]
        assets = list(package_bytes.items()) + [("SHA256SUMS.txt", ("\n".join(sum_lines) + "\n").encode())]
        self.seed_release(immutable=True, assets=assets)

        result = self.run_helper()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Immutable release", result.stdout)
        self.assertFalse(any(e[0] == "upload" for e in self.state()["events"]))


if __name__ == "__main__":
    unittest.main()
