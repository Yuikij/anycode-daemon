#!/usr/bin/env bash
# Cross-compile the AnyCode daemon for all supported platforms into ./dist,
# named so the npm wrapper and curl installer can fetch them from a GitHub
# Release. Optionally upload with `gh` when --upload is passed.
#
#   ./release.sh            # build all platforms into ./dist
#   ./release.sh --upload   # build + create/upload a GitHub release (needs gh)
set -euo pipefail
cd "$(dirname "$0")"

VERSION=$(grep -oE 'Version = "[^"]+"' main.go | sed -E 's/.*"(.*)"/\1/')
# Public release repo (override with ANYCODE_REPO=owner/name).
REPO="${ANYCODE_REPO:-Yuikij/anycode-daemon}"
OUT="dist"
rm -rf "$OUT"
mkdir -p "$OUT"

# platform/arch -> output suffix (matches install.sh + npm wrapper expectations)
TARGETS=(
  "darwin amd64 anycode-daemon-darwin-amd64"
  "darwin arm64 anycode-daemon-darwin-arm64"
  "linux  amd64 anycode-daemon-linux-amd64"
  "linux  arm64 anycode-daemon-linux-arm64"
  "windows amd64 anycode-daemon-windows-amd64.exe"
)

for t in "${TARGETS[@]}"; do
  read -r goos goarch name <<< "$t"
  echo "→ Building $goos/$goarch -> $OUT/$name"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "-s -w" -o "$OUT/$name" .
done

( cd "$OUT" && shasum -a 256 anycode-daemon-* > SHA256SUMS )
echo "✓ Built daemon v${VERSION} for all platforms into $OUT/"

if [[ "${1:-}" == "--upload" ]]; then
  command -v gh >/dev/null || { echo "gh CLI not found"; exit 1; }
  TAG="v${VERSION}"
  echo "→ Creating GitHub release ${TAG} on ${REPO}"
  gh release create "$TAG" "$OUT"/* --repo "$REPO" --title "AnyCode daemon ${TAG}" --notes "Daemon ${TAG}" || \
    gh release upload "$TAG" "$OUT"/* --repo "$REPO" --clobber
  echo "✓ Uploaded release ${TAG}"
fi
