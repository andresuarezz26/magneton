#!/usr/bin/env bash
set -euo pipefail

REPO="andresuarezz26/magneton"
INSTALL_DIR="$HOME/.local/bin"

# ── platform detection ────────────────────────────────────────────────────────

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
  x86_64)        ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Error: unsupported architecture: $ARCH"; exit 1 ;;
esac

case "$OS" in
  darwin|linux) ;;
  *) echo "Error: unsupported OS: $OS (Windows is not supported)"; exit 1 ;;
esac

# ── latest release ────────────────────────────────────────────────────────────

echo "Fetching latest release…"
LATEST=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
  | grep '"tag_name"' \
  | sed -E 's/.*"([^"]+)".*/\1/')

if [ -z "$LATEST" ]; then
  echo "Error: could not determine latest release."
  echo "Check: https://github.com/$REPO/releases"
  exit 1
fi

# ── download ──────────────────────────────────────────────────────────────────

BINARY="magneton_${OS}_${ARCH}"
URL="https://github.com/$REPO/releases/download/$LATEST/$BINARY"

mkdir -p "$INSTALL_DIR"

echo "Downloading magneton $LATEST ($OS/$ARCH)…"
curl -fsSL "$URL" -o "$INSTALL_DIR/magneton"
chmod +x "$INSTALL_DIR/magneton"

echo "✓ Installed → $INSTALL_DIR/magneton"

# ── PATH ──────────────────────────────────────────────────────────────────────

if ! echo ":${PATH}:" | grep -q ":$INSTALL_DIR:"; then
  if [ -f "$HOME/.zshrc" ];          then RC="$HOME/.zshrc"
  elif [ -f "$HOME/.bashrc" ];       then RC="$HOME/.bashrc"
  elif [ -f "$HOME/.bash_profile" ]; then RC="$HOME/.bash_profile"
  else RC="$HOME/.profile"; fi

  echo ""
  echo "  $INSTALL_DIR is not in your PATH. Add it:"
  echo "    echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> $RC && source $RC"
fi

# ── prerequisites ─────────────────────────────────────────────────────────────

echo ""
MISSING=0

if ! command -v claude &>/dev/null; then
  echo "⚠  Claude Code not found."
  echo "   Install it at https://claude.ai/download — required to run Magneton."
  MISSING=1
fi

if ! command -v gh &>/dev/null; then
  echo "⚠  GitHub CLI (gh) not found."
  echo "   Install it at https://cli.github.com — required to open pull requests."
  MISSING=1
elif ! gh auth status &>/dev/null 2>&1; then
  echo "⚠  gh is installed but not authenticated."
  echo "   Run: gh auth login"
  MISSING=1
fi

if ! command -v git &>/dev/null; then
  echo "⚠  git not found. Install it from https://git-scm.com/"
  MISSING=1
fi

# ── next step ─────────────────────────────────────────────────────────────────

echo ""
if [ "$MISSING" -eq 0 ]; then
  echo "All prerequisites found. Run:"
else
  echo "Fix the warnings above, then run:"
fi
echo "  magneton init"
echo ""
