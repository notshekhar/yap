#!/usr/bin/env bash
# yap installer.
#   curl -fsSL https://raw.githubusercontent.com/notshekhar/yap/main/install.sh | bash
#
# Downloads the prebuilt binary for this machine from GitHub Releases. There is
# nothing to install first — yap is a single static binary.
#
# Layout after install:
#   $YAP_HOME/yap              (default: ~/.yap-bin/yap)
#   $BIN_DIR/yap → that        (symlink)
#
# Your identity and messages live in ~/.yap and are never touched by this
# script, including on uninstall. That directory holds the private key that *is*
# your address: delete it and the address is gone for good.
#
# Flags (curl | bash -s -- <flags>) — each maps to the env knob beside it:
#   -v, --version <vX.Y.Z>   pin a specific tag         (YAP_VERSION)
#       --force              reinstall even if current  (YAP_FORCE=1)
#       --uninstall          remove install + symlink   (YAP_UNINSTALL=1)
#       --no-modify-path     do not touch shell rc      (YAP_NO_MODIFY_PATH=1)
#   -h, --help
#
# Extra env knobs:
#   YAP_REPO_SLUG   notshekhar/yap    override the repo
#   YAP_HOME        $HOME/.yap-bin    where the binary lands
#   YAP_BIN_DIR                       symlink dir (auto: /usr/local/bin or
#                                     $HOME/.local/bin)

set -euo pipefail

REPO_SLUG="${YAP_REPO_SLUG:-notshekhar/yap}"
YAP_HOME="${YAP_HOME:-$HOME/.yap-bin}"
FORCE="${YAP_FORCE:-0}"
UNINSTALL="${YAP_UNINSTALL:-0}"
PIN_VERSION="${YAP_VERSION:-}"
NO_MODIFY_PATH="${YAP_NO_MODIFY_PATH:-0}"

usage() {
  cat <<EOF
yap installer

Usage: install.sh [options]

Options:
  -v, --version <vX.Y.Z>  Install a specific release
      --force             Reinstall even when up to date
      --uninstall         Remove yap and its symlink
      --no-modify-path    Do not add the bin dir to your shell rc
  -h, --help              This message
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    -v|--version) PIN_VERSION="${2:-}"; shift 2 ;;
    --force) FORCE=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    --no-modify-path) NO_MODIFY_PATH=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage; exit 1 ;;
  esac
done

# ── Output ─────────────────────────────────────────────────────────────────
if [ -t 1 ]; then
  BOLD='\033[1m'; DIM='\033[2m'; RED='\033[31m'; MINT='\033[38;5;79m'; NC='\033[0m'
else
  BOLD=''; DIM=''; RED=''; MINT=''; NC=''
fi
bold() { printf "${BOLD}%s${NC}\n" "$1"; }
dim()  { printf "${DIM}%s${NC}\n" "$1"; }
err()  { printf "${RED}%s${NC}\n" "$1" >&2; }

need() { command -v "$1" >/dev/null 2>&1 || { err "missing $1"; exit 1; }; }
need curl
need tar

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    return 1
  fi
}

# "0.2.0" > "0.1.9"
ver_gt() {
  [ "$1" = "$2" ] && return 1
  local top
  top="$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)"
  [ "$top" = "$2" ] && return 0
  return 1
}

# ── Download progress bar ──────────────────────────────────────────────────
# curl traces into a FIFO; content-length and `<= recv data` records drive a
# ■■■･･･ 42% bar. TTY only — anything else falls back to plain curl.
unbuffered_sed() {
  if echo | sed -u -e "" >/dev/null 2>&1; then
    sed -nu "$@"
  elif echo | sed -l -e "" >/dev/null 2>&1; then
    sed -nl "$@"
  else
    local pad; pad="$(printf "\n%512s" "")"
    sed -ne "s/$/\\${pad}/" "$@"
  fi
}

PROGRESS_COLOR='\033[38;5;79m'
PROGRESS_NC='\033[0m'

print_progress() {
  local bytes="$1" length="$2"
  [ "$length" -gt 0 ] || return 0
  local width=50
  local percent=$(( bytes * 100 / length ))
  [ "$percent" -gt 100 ] && percent=100
  local on=$(( percent * width / 100 ))
  local off=$(( width - on ))
  local filled empty
  filled=$(printf "%*s" "$on" ""); filled=${filled// /■}
  empty=$(printf "%*s" "$off" ""); empty=${empty// /･}
  printf "\r${PROGRESS_COLOR}%s%s %3d%%${PROGRESS_NC}" "$filled" "$empty" "$percent" >&4
}

download_with_progress() {
  local url="$1" output="$2"
  if [ -t 2 ]; then exec 4>&2; else exec 4>/dev/null; fi

  local tracefile="${TMPDIR:-/tmp}/yap_install_$$.trace"
  rm -f "$tracefile"
  mkfifo "$tracefile" 2>/dev/null || return 1

  printf "\033[?25l" >&4
  trap "trap - RETURN; rm -f \"$tracefile\"; printf '\033[?25h' >&4; exec 4>&-" RETURN

  ( curl -f --trace-ascii "$tracefile" -s -L -o "$output" "$url" ) &
  local curl_pid=$!

  unbuffered_sed \
    -e 'y/ACDEGHLNORTV/acdeghlnortv/' \
    -e '/^0000: content-length:/p' \
    -e '/^<= recv data/p' \
    "$tracefile" | \
  {
    local length=0 bytes=0
    while IFS=" " read -r -a line; do
      [ "${#line[@]}" -lt 2 ] && continue
      local tag="${line[0]} ${line[1]}"
      if [ "$tag" = "0000: content-length:" ]; then
        # A redirect chain restarts the count; the asset response wins.
        length="$(echo "${line[2]}" | tr -d '\r')"
        bytes=0
      elif [ "$tag" = "<= recv" ]; then
        bytes=$(( bytes + ${line[3]} ))
        [ "$length" -gt 0 ] && print_progress "$bytes" "$length"
      fi
    done
  }

  wait $curl_pid
  local ret=$?
  echo "" >&4
  return $ret
}

# ── Platform ───────────────────────────────────────────────────────────────
detect_target() {
  local os arch
  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *) err "unsupported OS: $(uname -s)"; exit 1 ;;
  esac
  case "$(uname -m)" in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64)  arch=x64 ;;
    *) err "unsupported architecture: $(uname -m)"; exit 1 ;;
  esac
  printf "%s-%s" "$os" "$arch"
}

resolve_latest_tag() {
  # The releases/latest redirect is not rate-limited the way the API is.
  local loc
  loc="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/${REPO_SLUG}/releases/latest" 2>/dev/null || true)"
  case "$loc" in
    */tag/*) printf "%s" "${loc##*/tag/}"; return 0 ;;
  esac
  curl -fsSL "https://api.github.com/repos/${REPO_SLUG}/releases/latest" 2>/dev/null \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1
}

pick_bin_dir() {
  if [ -n "${YAP_BIN_DIR:-}" ]; then
    mkdir -p "$YAP_BIN_DIR"; printf "%s" "$YAP_BIN_DIR"; return
  fi
  if [ -w /usr/local/bin ] 2>/dev/null; then
    printf "/usr/local/bin"; return
  fi
  mkdir -p "$HOME/.local/bin"; printf "%s" "$HOME/.local/bin"
}

add_to_path() {
  local dir="$1"
  case ":$PATH:" in *":$dir:"*) return 0 ;; esac
  [ "$NO_MODIFY_PATH" = "1" ] && { dim "  add to PATH yourself: export PATH=\"$dir:\$PATH\""; return 0; }

  local line="export PATH=\"$dir:\$PATH\""
  local touched=0
  for rc in "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.profile"; do
    [ -f "$rc" ] || continue
    grep -qF "$dir" "$rc" 2>/dev/null && { touched=1; continue; }
    printf '\n# yap\n%s\n' "$line" >> "$rc"
    dim "  added $dir to $rc"
    touched=1
  done
  [ "$touched" = "0" ] && dim "  add to PATH yourself: $line"
  return 0
}

# ── Uninstall ──────────────────────────────────────────────────────────────
if [ "$UNINSTALL" = "1" ]; then
  bold "Removing yap"
  for d in /usr/local/bin "$HOME/.local/bin" "${YAP_BIN_DIR:-}"; do
    [ -n "$d" ] || continue
    [ -L "$d/yap" ] && { rm -f "$d/yap"; dim "  removed $d/yap"; }
  done
  [ -d "$YAP_HOME" ] && { rm -rf "$YAP_HOME"; dim "  removed $YAP_HOME"; }
  dim "  your identity and messages are untouched in ~/.yap"
  bold "Done."
  exit 0
fi

# ── Install ────────────────────────────────────────────────────────────────
target="$(detect_target)"
tag="$PIN_VERSION"
[ -z "$tag" ] && tag="$(resolve_latest_tag)"
[ -z "$tag" ] && { err "could not resolve the latest release of $REPO_SLUG"; exit 1; }
case "$tag" in v*) ;; *) tag="v$tag" ;; esac

installed=""
if [ -x "$YAP_HOME/yap" ]; then
  installed="$("$YAP_HOME/yap" -version 2>/dev/null | awk '{print $2}' || true)"
fi

printf "${MINT}yap${NC} ${DIM}%s${NC}\n" "$target"
if [ -n "$installed" ] && [ "$FORCE" != "1" ]; then
  if ! ver_gt "${tag#v}" "$installed"; then
    bold "Already at $installed (latest $tag)"
    exit 0
  fi
  dim "  update: $installed → ${tag#v}"
else
  dim "  installing ${tag#v}"
fi

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

asset="yap-${target}.tar.gz"
url="https://github.com/${REPO_SLUG}/releases/download/${tag}/${asset}"
tarball="$scratch/$asset"

if ! { [ -t 2 ] && download_with_progress "$url" "$tarball"; }; then
  curl -fL --progress-bar "$url" -o "$tarball" || { err "download failed: $url"; exit 1; }
fi
[ -s "$tarball" ] || { err "downloaded nothing from $url"; exit 1; }

# Checksums are published beside the asset. A missing one is a warning, a
# mismatched one is fatal.
if curl -fsSL "${url}.sha256" -o "$scratch/sum" 2>/dev/null && [ -s "$scratch/sum" ]; then
  want="$(awk '{print $1}' "$scratch/sum")"
  got="$(sha256_of "$tarball" || true)"
  if [ -n "$got" ] && [ "$want" != "$got" ]; then
    err "checksum mismatch — refusing to install"
    err "  expected $want"
    err "  got      $got"
    exit 1
  fi
  [ -n "$got" ] && dim "  sha256 ok"
else
  dim "  no published checksum for this asset"
fi

rm -rf "$YAP_HOME"
mkdir -p "$YAP_HOME"
# The tarball holds one directory named for the target; strip it so the binary
# lands directly in YAP_HOME.
tar -xzf "$tarball" -C "$YAP_HOME" --strip-components=1
chmod +x "$YAP_HOME/yap"

# macOS quarantines anything downloaded, and a quarantined binary is killed on
# launch with a message about an unidentified developer. Clearing the attribute
# is what makes an unsigned release usable at all.
if [ "$(uname -s)" = "Darwin" ] && command -v xattr >/dev/null 2>&1; then
  xattr -d com.apple.quarantine "$YAP_HOME/yap" 2>/dev/null || true
fi

bin_dir="$(pick_bin_dir)"
ln -sf "$YAP_HOME/yap" "$bin_dir/yap"
dim "  linked $bin_dir/yap"
add_to_path "$bin_dir"

echo
bold "$("$YAP_HOME/yap" -version 2>/dev/null || echo yap)"
case "$target" in
  darwin-*) dim "  run: yap" ;;
  linux-*)  dim "  run: yap   (no Bluetooth on Linux yet — use -tcp for now)" ;;
esac
dim "  people running yap near you show up on their own. No accounts, no server."
