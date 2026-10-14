#!/bin/sh
# Install the Backyard CLI (the `backyard` binary) from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --version 0.2.0 --dir ~/bin
#
# Options (or environment variables):
#   --version V   BACKYARD_VERSION      release to install (default: latest)
#   --dir D       BACKYARD_INSTALL_DIR  target directory (default: /usr/local/bin when
#                                       writable or sudo is available, else ~/.local/bin)
#   --repo O/R    BACKYARD_REPO         GitHub repository (default: thasami/backyard)
#   --no-sudo     BACKYARD_NO_SUDO=1    never use sudo
#                 GITHUB_TOKEN          optional, raises GitHub API rate limits
#                 BACKYARD_GITHUB_URL / BACKYARD_GITHUB_API_URL
#                                       GitHub Enterprise or a mirror of the releases
#
# The archive is verified against the release's checksums.txt (SHA-256) before
# anything is installed. Supported: Linux and macOS on amd64/arm64. On Windows,
# download backyard_<version>_windows_<arch>.zip from the releases page.
set -eu

REPO="${BACKYARD_REPO:-thasami/backyard}"
VERSION="${BACKYARD_VERSION:-latest}"
INSTALL_DIR="${BACKYARD_INSTALL_DIR:-}"
NO_SUDO="${BACKYARD_NO_SUDO:-}"
GITHUB="${BACKYARD_GITHUB_URL:-https://github.com}"
GITHUB_API="${BACKYARD_GITHUB_API_URL:-https://api.github.com}"

log() { printf '%s\n' "$*" >&2; }
fail() {
	log "error: $*"
	exit 1
}

usage() {
	cat >&2 <<EOF
Install the Backyard CLI from GitHub releases.

usage: install.sh [--version V] [--dir D] [--repo owner/name] [--no-sudo]

  --version V   release to install (default: latest)          [BACKYARD_VERSION]
  --dir D       install directory (default: /usr/local/bin,
                or ~/.local/bin without write access or sudo)  [BACKYARD_INSTALL_DIR]
  --repo O/R    GitHub repository (default: thasami/backyard)  [BACKYARD_REPO]
  --no-sudo     never use sudo                                 [BACKYARD_NO_SUDO=1]
EOF
	exit "${1:-0}"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || fail "--version needs a value"; VERSION="$2"; shift 2 ;;
	--version=*) VERSION="${1#*=}"; shift ;;
	--dir) [ $# -ge 2 ] || fail "--dir needs a value"; INSTALL_DIR="$2"; shift 2 ;;
	--dir=*) INSTALL_DIR="${1#*=}"; shift ;;
	--repo) [ $# -ge 2 ] || fail "--repo needs a value"; REPO="$2"; shift 2 ;;
	--repo=*) REPO="${1#*=}"; shift ;;
	--no-sudo) NO_SUDO=1; shift ;;
	-h | --help) usage 0 ;;
	*) log "unknown option: $1"; usage 2 ;;
	esac
done

case "$REPO" in
*/*) ;;
*) fail "--repo must look like owner/name, got '$REPO'" ;;
esac

have() { command -v "$1" >/dev/null 2>&1; }

# download URL FILE
download() {
	if have curl; then
		if [ -n "${GITHUB_TOKEN:-}" ] && [ "${1#"$GITHUB_API"}" != "$1" ]; then
			curl -fsSL --retry 3 -H "Authorization: Bearer $GITHUB_TOKEN" -o "$2" "$1"
		else
			curl -fsSL --retry 3 -o "$2" "$1"
		fi
	elif have wget; then
		if [ -n "${GITHUB_TOKEN:-}" ] && [ "${1#"$GITHUB_API"}" != "$1" ]; then
			wget -q --header "Authorization: Bearer $GITHUB_TOKEN" -O "$2" "$1"
		else
			wget -q -O "$2" "$1"
		fi
	else
		fail "curl or wget is required"
	fi
}

detect_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	MINGW* | MSYS* | CYGWIN* | Windows_NT)
		fail "on Windows, download backyard_<version>_windows_<arch>.zip from $GITHUB/$REPO/releases" ;;
	*) fail "unsupported operating system: $(uname -s)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64 | armv8*) echo arm64 ;;
	*) fail "unsupported architecture: $(uname -m) (release builds exist for amd64 and arm64)" ;;
	esac
}

# sha256 FILE prints the hex digest.
sha256() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif have openssl; then
		openssl dgst -sha256 "$1" | awk '{print $NF}'
	else
		fail "sha256sum, shasum or openssl is required to verify the download"
	fi
}

latest_version() {
	api="$GITHUB_API/repos/$REPO/releases/latest"
	download "$api" "$tmp/latest.json" || fail "could not query $api (set --version, or GITHUB_TOKEN if rate limited)"
	tag="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)"
	[ -n "$tag" ] || fail "no published release found for $REPO"
	echo "$tag"
}

choose_dir() {
	if [ -n "$INSTALL_DIR" ]; then
		echo "$INSTALL_DIR"
	elif [ -w /usr/local/bin ] || { [ -z "$NO_SUDO" ] && [ "$(id -u)" != 0 ] && have sudo && [ -d /usr/local/bin ]; }; then
		echo /usr/local/bin
	else
		echo "${HOME:?HOME is not set}/.local/bin"
	fi
}

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t backyard)"
trap 'rm -rf "$tmp"' EXIT INT TERM

os="$(detect_os)"
arch="$(detect_arch)"
if [ "$VERSION" = "latest" ]; then
	tag="$(latest_version)"
else
	tag="v${VERSION#v}"
fi
version="${tag#v}"
archive="backyard_${version}_${os}_${arch}.tar.gz"
base="$GITHUB/$REPO/releases/download/$tag"

log "Downloading backyard $version ($os/$arch) from $GITHUB/$REPO"
download "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive"
download "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"

want="$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1}' "$tmp/checksums.txt" | head -n 1)"
[ -n "$want" ] || fail "$archive is not listed in checksums.txt"
got="$(sha256 "$tmp/$archive")"
[ "$want" = "$got" ] || fail "checksum mismatch for $archive (want $want, got $got)"
log "Checksum verified ($got)"

mkdir -p "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x"
bin="$tmp/x/backyard"
[ -f "$bin" ] || bin="$(find "$tmp/x" -type f -name backyard | head -n 1)"
if [ -z "$bin" ] || [ ! -f "$bin" ]; then
	fail "the archive does not contain a backyard binary"
fi
chmod 0755 "$bin"

dir="$(choose_dir)"
sudo=""
if ! mkdir -p "$dir" 2>/dev/null || [ ! -w "$dir" ]; then
	if [ -z "$NO_SUDO" ] && [ "$(id -u)" != 0 ] && have sudo; then
		sudo="sudo"
		log "Installing to $dir requires sudo"
		$sudo mkdir -p "$dir"
	else
		fail "$dir is not writable (use --dir, e.g. --dir ~/.local/bin)"
	fi
fi
$sudo cp "$bin" "$dir/backyard.tmp.$$"
$sudo chmod 0755 "$dir/backyard.tmp.$$"
$sudo mv -f "$dir/backyard.tmp.$$" "$dir/backyard"

log "Installed $dir/backyard"
"$dir/backyard" version >&2 || true
case ":${PATH}:" in
*":$dir:"*) ;;
*) log "Note: $dir is not in your PATH. Add it, e.g.: export PATH=\"$dir:\$PATH\"" ;;
esac
log "Next: backyard login --server https://backyard.your-company.com"
