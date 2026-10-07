#!/usr/bin/env bash
set -Eeuo pipefail

fail() {
  printf 'publish-release: %s\n' "$*" >&2
  exit 1
}

: "${VERSION:?Set VERSION to the release version without a leading v}"
: "${SOURCE_SHA:?Set SOURCE_SHA to the 40-character source commit SHA}"
: "${GH_REPO:?Set GH_REPO to OWNER/REPO}"
: "${GH_TOKEN:?Set GH_TOKEN for GitHub API access}"

[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || fail "invalid VERSION: $VERSION"
[[ "$SOURCE_SHA" =~ ^[0-9a-fA-F]{40}$ ]] || fail 'SOURCE_SHA must contain exactly 40 hexadecimal characters'
[[ "$GH_REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "invalid GH_REPO: $GH_REPO"

for tool in git gh python3 sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || fail "required tool is missing: $tool"
done

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$ROOT_DIR"
TAG="v$VERSION"
ASSET_NAMES=(
  "tamias-$VERSION-macos-arm64.pkg"
  "tamias-$VERSION-macos-amd64.pkg"
  "tamias-$VERSION-windows-amd64-setup.exe"
  "tamias-$VERSION-linux-amd64.AppImage"
)
SUM_NAME=SHA256SUMS.txt
DIST_ASSETS=()

for name in "${ASSET_NAMES[@]}"; do
  [[ -s "$ROOT_DIR/dist/$name" ]] || fail "required release asset is missing or empty: dist/$name"
  DIST_ASSETS+=("$ROOT_DIR/dist/$name")
done

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/tamias-publish-release.XXXXXX")"
trap 'rm -rf -- "$TMP_DIR"' EXIT

verify_remote_tag() {
  local refs ref sha tag_sha= peeled_sha= expected_ref="refs/tags/$TAG"
  if ! refs="$(git ls-remote --tags origin "$expected_ref" "${expected_ref}^{}")"; then
    fail "could not read remote tag $TAG from origin"
  fi
  while read -r sha ref; do
    [[ -n "${sha:-}" && -n "${ref:-}" ]] || continue
    case "$ref" in
      "$expected_ref") tag_sha="$sha" ;;
      "${expected_ref}^{}") peeled_sha="$sha" ;;
    esac
  done <<< "$refs"
  [[ "$tag_sha" =~ ^[0-9a-fA-F]{40}$ ]] || fail "remote tag $TAG does not exist or has an invalid object SHA"
  local actual_sha="${peeled_sha:-$tag_sha}"
  [[ "$actual_sha" =~ ^[0-9a-fA-F]{40}$ ]] || fail "remote tag $TAG has an invalid peeled SHA"
  actual_sha_lc="$(printf '%s' "$actual_sha" | tr '[:upper:]' '[:lower:]')"
  source_sha_lc="$(printf '%s' "$SOURCE_SHA" | tr '[:upper:]' '[:lower:]')"
  [[ "$actual_sha_lc" == "$source_sha_lc" ]] || fail "remote tag $TAG now points to $actual_sha, expected SOURCE_SHA=$SOURCE_SHA"
}

# Recheck the peeled remote ref after the build matrix has finished. Checkout's
# persisted credential lets ls-remote authenticate for private repositories.
verify_remote_tag

api_get() {
  local endpoint="$1" body_path="$2" response="$TMP_DIR/api-response" error="$TMP_DIR/api-error"
  local rc=0 status
  gh api --include "$endpoint" >"$response" 2>"$error" || rc=$?
  status="$(awk '/^HTTP\// { code=$2 } END { gsub(/\r/, "", code); print code }' "$response")"
  if [[ "$status" == 404 ]]; then
    return 4
  fi
  if (( rc != 0 )) || [[ ! "$status" =~ ^2[0-9][0-9]$ ]]; then
    cat "$error" >&2
    [[ -n "$status" ]] && printf 'GitHub API returned HTTP %s for %s\n' "$status" "$endpoint" >&2
    fail "GitHub API request failed for $endpoint"
  fi
  awk 'BEGIN { body=0 } body { print; next } /^\r?$/ { body=1 }' "$response" >"$body_path"
  python3 - "$body_path" <<'PY' || fail "GitHub API returned invalid JSON for $endpoint"
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    json.load(f)
PY
}

RELEASE_ENDPOINT="repos/$GH_REPO/releases/tags/$TAG"
RELEASE_JSON="$TMP_DIR/release.json"
release_exists=1
if api_get "$RELEASE_ENDPOINT" "$RELEASE_JSON"; then
  :
else
  rc=$?
  if (( rc == 4 )); then
    release_exists=0
  else
    exit "$rc"
  fi
fi

is_prerelease=false
[[ "$VERSION" == *-* ]] && is_prerelease=true

release_property() {
  python3 - "$RELEASE_JSON" "$1" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    value = json.load(f).get(sys.argv[2])
if isinstance(value, bool):
    print("true" if value else "false")
elif value is None:
    print("")
else:
    print(value)
PY
}

asset_exists() {
  python3 - "$RELEASE_JSON" "$1" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    release = json.load(f)
matches = [asset for asset in release.get("assets", []) if asset.get("name") == sys.argv[2]]
if len(matches) > 1:
    raise SystemExit("duplicate release asset name: " + sys.argv[2])
print("true" if matches else "false")
PY
}

make_checksums() {
  local directory="$1"
  (cd "$directory" && sha256sum -- "${ASSET_NAMES[@]}" >"$SUM_NAME")
}

verify_checksums() {
  local checksum_file="$1" directory="$2"
  python3 - "$checksum_file" "$directory" "${ASSET_NAMES[@]}" <<'PY'
import hashlib, pathlib, re, sys

checksum_file = pathlib.Path(sys.argv[1])
directory = pathlib.Path(sys.argv[2])
expected_names = sys.argv[3:]
entries = {}
for line_number, line in enumerate(checksum_file.read_text(encoding="utf-8").splitlines(), 1):
    match = re.fullmatch(r"([0-9a-fA-F]{64})  (.+)", line)
    if not match:
        raise SystemExit(f"invalid checksum line {line_number}")
    digest, name = match.groups()
    if name in entries:
        raise SystemExit(f"duplicate checksum entry: {name}")
    entries[name] = digest.lower()
if set(entries) != set(expected_names):
    raise SystemExit("SHA256SUMS does not contain exactly the four expected installer names")
for name in expected_names:
    path = directory / name
    if not path.is_file():
        raise SystemExit(f"missing installer for checksum validation: {name}")
    digest = hashlib.sha256()
    with path.open("rb") as asset:
        for block in iter(lambda: asset.read(1024 * 1024), b""):
            digest.update(block)
    actual = digest.hexdigest()
    if entries[name] != actual:
        raise SystemExit(f"checksum mismatch for {name}")
PY
}

download_asset() {
  local name="$1" destination="$2"
  gh release download "$TAG" --repo "$GH_REPO" --pattern "$name" --dir "$destination"
  [[ -s "$destination/$name" ]] || fail "GitHub did not download expected release asset $name"
}

if (( release_exists == 0 )); then
  notes_args=(--generate-notes)
  notes_file="$ROOT_DIR/docs/releases/$VERSION.md"
  [[ ! -f "$notes_file" ]] || notes_args=(--notes-file "$notes_file")
  create_args=("$TAG" --repo "$GH_REPO" --verify-tag --draft --title "Tamias $VERSION" "${notes_args[@]}")
  [[ "$is_prerelease" != true ]] || create_args+=(--prerelease)
  gh release create "${create_args[@]}"
  make_checksums "$ROOT_DIR/dist"
  gh release upload "$TAG" --repo "$GH_REPO" --clobber "${DIST_ASSETS[@]}" "$ROOT_DIR/dist/$SUM_NAME"
  verify_remote_tag
  gh release edit "$TAG" --repo "$GH_REPO" --draft=false
  printf 'Published new release %s with four installers and checksums.\n' "$TAG"
  exit 0
fi

release_draft="$(release_property draft)"
release_immutable="$(release_property immutable)"
[[ "$release_immutable" == true ]] || release_immutable=false

if [[ "$release_draft" == true ]]; then
  make_checksums "$ROOT_DIR/dist"
  gh release upload "$TAG" --repo "$GH_REPO" --clobber "${DIST_ASSETS[@]}" "$ROOT_DIR/dist/$SUM_NAME"
  verify_remote_tag
  gh release edit "$TAG" --repo "$GH_REPO" --draft=false
  printf 'Published draft release %s with four installers and checksums.\n' "$TAG"
  exit 0
fi

if [[ "$release_immutable" == true ]]; then
  for name in "${ASSET_NAMES[@]}" "$SUM_NAME"; do
    [[ "$(asset_exists "$name")" == true ]] || fail "immutable release $TAG is incomplete: missing $name; it cannot be modified"
  done
fi

REMOTE_DIR="$TMP_DIR/remote"
ASSEMBLY_DIR="$TMP_DIR/final-assets"
mkdir -p "$REMOTE_DIR" "$ASSEMBLY_DIR"
missing_assets=()
missing_count=0
for name in "${ASSET_NAMES[@]}"; do
  if [[ "$(asset_exists "$name")" == true ]]; then
    download_asset "$name" "$REMOTE_DIR"
    cp -- "$REMOTE_DIR/$name" "$ASSEMBLY_DIR/$name"
  else
    missing_assets+=("$ROOT_DIR/dist/$name")
    missing_count=$((missing_count + 1))
    cp -- "$ROOT_DIR/dist/$name" "$ASSEMBLY_DIR/$name"
  fi
done
make_checksums "$ASSEMBLY_DIR"

if [[ "$(asset_exists "$SUM_NAME")" == true ]]; then
  download_asset "$SUM_NAME" "$REMOTE_DIR"
  if ! verify_checksums "$REMOTE_DIR/$SUM_NAME" "$ASSEMBLY_DIR"; then
    if [[ "$release_immutable" == true ]]; then
      fail "immutable release $TAG has invalid SHA256SUMS.txt and cannot be repaired"
    fi
    fail "published release $TAG has a SHA256SUMS.txt that does not match the complete installer set; preserving it"
  fi
else
  missing_assets+=("$ASSEMBLY_DIR/$SUM_NAME")
  missing_count=$((missing_count + 1))
fi

if [[ "$release_immutable" == true ]]; then
  printf 'Immutable release %s is complete and its checksums are valid; no changes needed.\n' "$TAG"
  exit 0
fi

if ((missing_count > 0)); then
  verify_remote_tag
  gh release upload "$TAG" --repo "$GH_REPO" "${missing_assets[@]}"
  printf 'Added %s missing asset(s) to published release %s without replacing existing assets.\n' "$missing_count" "$TAG"
else
  printf 'Published release %s already has the complete installer set and valid checksums.\n' "$TAG"
fi
