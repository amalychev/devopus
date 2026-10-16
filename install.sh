#!/bin/sh
# Installs the devopus binary from GitHub releases and runs 'devopus agent install':
#   curl -fsSL https://raw.githubusercontent.com/devopus/devopus/main/install.sh | sh
#
# DEVOPUS_VERSION   a version such as 0.1.0 (default: the latest release)
# DEVOPUS_BIN_DIR   where to put the binary (default: /usr/local/bin if writable, else ~/.local/bin)
# DEVOPUS_BASE_URL  where the release files are (default: GitHub releases of devopus/devopus)
# DEVOPUS_NO_SETUP  set to skip 'devopus agent install'
set -eu

say() { printf '%s\n' "$*" >&2; }
die() { say "devopus: $*"; exit 1; }

case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "$(uname -s) is not supported yet: macOS and Linux are" ;;
esac
case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "$(uname -m) is not supported: amd64 and arm64 are" ;;
esac

version="${DEVOPUS_VERSION:-latest}"
base="${DEVOPUS_BASE_URL:-}"
if [ -z "$base" ]; then
	if [ "$version" = latest ]; then
		base="https://github.com/devopus/devopus/releases/latest/download"
	else
		base="https://github.com/devopus/devopus/releases/download/v${version#v}"
	fi
fi

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
else
	die "needs curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
else
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
archive="devopus_${os}_${arch}.tar.gz"
say "Downloading $archive ($version)"
fetch "$base/$archive" "$tmp/$archive" || die "cannot download $base/$archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "cannot download $base/checksums.txt"
want="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || die "checksums.txt has no line for $archive"
[ "$(sha256 "$tmp/$archive")" = "$want" ] || die "checksum mismatch for $archive"
tar -xzf "$tmp/$archive" -C "$tmp" devopus

dir="${DEVOPUS_BIN_DIR:-}"
if [ -z "$dir" ]; then
	if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		dir=/usr/local/bin
	else
		dir="$HOME/.local/bin"
	fi
fi
mkdir -p "$dir"
# Into place with a rename, so a running agent keeps its file until it restarts.
cp "$tmp/devopus" "$dir/.devopus.new"
chmod 0755 "$dir/.devopus.new"
mv -f "$dir/.devopus.new" "$dir/devopus"
say "Installed $("$dir/devopus" version 2>/dev/null || echo devopus) to $dir/devopus"

case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "Add $dir to PATH, for example: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.zshrc" ;;
esac

if [ -n "${DEVOPUS_NO_SETUP:-}" ]; then
	say "Next: devopus agent install"
	exit 0
fi
# The script itself comes through a pipe: 'devopus agent install' asks its questions on the terminal.
if [ -r /dev/tty ] && (: </dev/tty) 2>/dev/null; then
	"$dir/devopus" agent install </dev/tty
else
	say "Next: devopus agent install"
fi
