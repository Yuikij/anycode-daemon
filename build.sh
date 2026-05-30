#!/usr/bin/env bash
# Build the daemon for Linux (the deploy target) and macOS (local debug),
# then sync the Linux binary into the iOS app bundle so it ships with the
# next Xcode build. Run this any time daemon Go source changes.
set -euo pipefail

cd "$(dirname "$0")"

echo "→ Building Linux amd64 binary..."
GOOS=linux GOARCH=amd64 go build -o anycode-daemon-linux-amd64 .

echo "→ Building local macOS binary..."
go build -o anycode-daemon .

IOS_BUNDLE_DIR="$(cd .. && pwd)/ios/AnyCode"
if [ -d "$IOS_BUNDLE_DIR" ]; then
    echo "→ Syncing Linux binary into iOS bundle..."
    cp -f anycode-daemon-linux-amd64 "$IOS_BUNDLE_DIR/anycode-daemon-linux-amd64"
fi

VERSION=$(grep -oE 'Version = "[^"]+"' main.go | sed -E 's/.*"(.*)"/\1/')

# Keep the iOS bundle's expected daemon version in sync so the app knows it
# ships a newer binary and offers to upgrade the remote daemon.
DAEMON_BUNDLE="${IOS_BUNDLE_DIR:-}/Services/DaemonBundle.swift"
if [ -n "${IOS_BUNDLE_DIR:-}" ] && [ -f "$DAEMON_BUNDLE" ]; then
    sed -i '' -E "s/(static let version = \")[^\"]+(\")/\1${VERSION}\2/" "$DAEMON_BUNDLE"
    echo "→ Synced DaemonBundle.swift version to ${VERSION}"
fi

echo "✓ daemon v${VERSION} built and bundled."
