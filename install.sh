#!/usr/bin/env bash
# crq installer.
# Installs the Go crq binary into ~/.local/bin by default.
set -euo pipefail

REPO="${CRQ_INSTALL_REPO:-kristofferR/codereview-queue}"
REF="${CRQ_INSTALL_REF:-main}"
BIN_DIR="${CRQ_BIN_DIR:-$HOME/.local/bin}"
NAME="crq"
SKILL_NAME="codereview-queue"
SKILL_DEST="${CRQ_SKILL_DIR:-${CODEX_HOME:-$HOME/.codex}/skills/$SKILL_NAME}"

say() { printf 'crq-install: %s\n' "$*"; }

# Replace the installed binary atomically: stage next to the target, then rename.
# Never write over the existing file in place — on macOS, overwriting an
# already-executed binary on the same inode leaves the kernel's cached code
# signature stale and every later run is killed with SIGKILL ("Killed: 9").
# A rename swaps in a fresh inode, and a running daemon keeps its old one.
install_binary() {
  local src="$1"
  local dest="$2"
  local staged
  staged="$(mktemp "$(dirname "$dest")/.$(basename "$dest").tmp.XXXXXX")"
  cp "$src" "$staged"
  chmod 0755 "$staged"
  mv -f "$staged" "$dest"
}

mkdir -p "$BIN_DIR"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
esac

download() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1"
  else
    say "ERROR: need curl or wget"
    exit 1
  fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# Keep the release-asset and source-build working dirs separate so a partial or
# directory-bearing release extraction can never be mistaken for the source tree
# by the "first dir under the work root" selection below.
rel="$tmp/release"
srcroot="$tmp/source"
mkdir -p "$rel" "$srcroot"
src_dir=""

source_url() {
  printf 'https://github.com/%s/archive/%s.tar.gz' "$REPO" "$REF"
}

ensure_source_dir() {
  if [ -n "${src_dir:-}" ] && [ -d "$src_dir" ]; then
    return 0
  fi
  if [ -n "${CRQ_INSTALL_SOURCE_DIR:-}" ]; then
    src_dir="$CRQ_INSTALL_SOURCE_DIR"
    [ -f "$src_dir/cmd/crq/main.go" ] || {
      say "ERROR: CRQ_INSTALL_SOURCE_DIR does not look like a crq checkout: $src_dir"
      exit 1
    }
    return 0
  fi

  src="$(source_url)"
  say "downloading source $src"
  download "$src" "$srcroot/src.tgz"
  tar -xzf "$srcroot/src.tgz" -C "$srcroot"
  # GitHub archives extract to a single "<repo>-<ref>/" dir; match it without
  # hardcoding the repo name so CRQ_INSTALL_REPO forks also work. Searching only
  # under $srcroot guarantees we never pick up a release-asset directory.
  src_dir="$(find "$srcroot" -mindepth 1 -maxdepth 1 -type d | head -1)"
  [ -n "$src_dir" ] || { say "ERROR: source archive layout not recognized"; exit 1; }
}

skill_install_enabled() {
  case "${CRQ_INSTALL_SKILL:-1}" in
    0|false|FALSE|no|NO) return 1 ;;
    *) return 0 ;;
  esac
}

install_skill_from_root() {
  root="$1"
  skill_src="$root/skills/$SKILL_NAME"
  [ -f "$skill_src/SKILL.md" ] || return 1

  skill_parent="$(dirname "$SKILL_DEST")"
  mkdir -p "$skill_parent"
  install_dest="$SKILL_DEST"
  if [ -L "$SKILL_DEST" ]; then
    link_target="$(readlink "$SKILL_DEST")"
    case "$link_target" in
      /*) install_dest="$link_target" ;;
      *) install_dest="$skill_parent/$link_target" ;;
    esac
  fi

  install_parent="$(dirname "$install_dest")"
  mkdir -p "$install_parent"
  skill_tmp="$(mktemp -d "$install_parent/.${SKILL_NAME}.tmp.XXXXXX")"
  cp -R "$skill_src/." "$skill_tmp/"
  rm -rf "$install_dest"
  mv "$skill_tmp" "$install_dest"
  chmod 755 "$install_dest"
  if [ "$install_dest" = "$SKILL_DEST" ]; then
    say "installed Codex skill to $SKILL_DEST"
  else
    say "installed Codex skill to $install_dest via $SKILL_DEST"
  fi
}

install_skill() {
  if ! skill_install_enabled; then
    say "skipping skill install (CRQ_INSTALL_SKILL=${CRQ_INSTALL_SKILL:-0})"
    return 0
  fi
  if install_skill_from_root "$rel"; then
    return 0
  fi
  ensure_source_dir
  install_skill_from_root "$src_dir" || {
    say "ERROR: skill source not found at skills/$SKILL_NAME"
    exit 1
  }
}

asset="crq_${os}_${arch}.tar.gz"
release_url="https://github.com/${REPO}/releases/latest/download/${asset}"
if [ -z "${CRQ_INSTALL_REF:-}" ] && [ -z "${CRQ_INSTALL_SOURCE_DIR:-}" ]; then
  say "trying release asset $release_url"
  if download "$release_url" "$rel/crq.tgz" 2>/dev/null \
    && tar -xzf "$rel/crq.tgz" -C "$rel" 2>/dev/null \
    && [ -f "$rel/crq" ]; then
    install_binary "$rel/crq" "$BIN_DIR/$NAME"
    say "installed to $BIN_DIR/$NAME"
    install_skill
    say "run 'crq help' for the agent loop contract; Codex can also use the installed skill"
    exit 0
  fi
  say "release asset unavailable or unusable; falling back to source build"
fi

command -v go >/dev/null 2>&1 || {
  say "ERROR: Go is required for source install fallback"
  exit 1
}

ensure_source_dir

say "building crq"
( cd "$src_dir" && go build -trimpath -ldflags "-s -w" -o "$tmp/crq" ./cmd/crq )
install_binary "$tmp/crq" "$BIN_DIR/$NAME"
say "installed to $BIN_DIR/$NAME"
install_skill
say "run 'crq help' for the agent loop contract; Codex can also use the installed skill"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "NOTE: $BIN_DIR is not on your PATH; add: export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac
