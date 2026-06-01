#!/usr/bin/env bash
# AnyCode daemon installer.
#   curl -fsSL https://raw.githubusercontent.com/Yuikij/anycode-daemon/main/install.sh | sudo bash
#
# Detects platform, downloads the matching daemon binary from the latest
# GitHub Release, and installs it as `anycode` on your PATH.
set -euo pipefail

REPO="${ANYCODE_REPO:-Yuikij/anycode-daemon}"
INSTALL_DIR="${ANYCODE_INSTALL_DIR:-/usr/local/bin}"
BIN_NAME="anycode"

err() { echo "error: $*" >&2; exit 1; }

os="$(uname -s)"
arch="$(uname -m)"

case "$os" in
  Darwin) goos="darwin" ;;
  Linux)  goos="linux" ;;
  *) err "unsupported OS: $os (use the Windows release asset directly)" ;;
esac

case "$arch" in
  x86_64|amd64) goarch="amd64" ;;
  arm64|aarch64) goarch="arm64" ;;
  *) err "unsupported architecture: $arch" ;;
esac

asset="anycode-daemon-${goos}-${goarch}"
url="${ANYCODE_DOWNLOAD_BASE:-https://github.com/${REPO}/releases/latest/download}/${asset}"

tmp="$(mktemp)"
echo "→ Downloading ${url}"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$url" -o "$tmp" || err "download failed"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$tmp" "$url" || err "download failed"
else
  err "need curl or wget"
fi

chmod +x "$tmp"

dest="${INSTALL_DIR}/${BIN_NAME}"
echo "→ Installing to ${dest}"
if [ -w "$INSTALL_DIR" ]; then
  mv "$tmp" "$dest"
else
  if command -v sudo >/dev/null 2>&1; then
    sudo mv "$tmp" "$dest"
  else
    err "no write permission to ${INSTALL_DIR}; rerun with sudo"
  fi
fi

echo "✓ AnyCode daemon installed: $(${dest} --version 2>/dev/null || echo "$dest")"
echo

# Best-effort: set up the Claude Code ACP bridge so `claude` works out of the
# box. Non-fatal — the daemon also self-installs this on first Claude use.
if command -v claude-code-acp >/dev/null 2>&1; then
  echo "✓ claude-code-acp already installed"
elif command -v npm >/dev/null 2>&1; then
  echo "→ Installing @zed-industries/claude-code-acp (Claude Code bridge)..."
  if npm install -g @zed-industries/claude-code-acp >/dev/null 2>&1; then
    echo "✓ claude-code-acp installed"
  else
    echo "! claude-code-acp install failed — run: npm install -g @zed-industries/claude-code-acp"
  fi
else
  echo "! Node.js/npm not found. To use Claude Code, install Node.js then run:"
  echo "    npm install -g @zed-industries/claude-code-acp"
fi
echo
echo "Next:"
echo "  anycode login      # sign in to anycodeapp.com"
echo "  anycode register   # register this machine"
echo "  anycode start -d   # go online in the background"
echo "  anycode status     # check the daemon  |  anycode log -f  |  anycode stop"
