#!/bin/sh
# Ícaro installer. Downloads a release archive, verifies its sha256 against
# checksums.txt and the ed25519 signature over checksums.txt, then installs
# the icaro binary. Nothing is installed unless both checks pass.
#
#   curl -fsSL https://github.com/ggrocco/icaro/releases/latest/download/install.sh | sh
#   sh install.sh --version v0.3.0 --bin-dir ~/.local/bin
#
# Options:
#   --version TAG               install TAG instead of the latest release
#   --bin-dir DIR               install into DIR (default ~/.local/bin)
#   --base-url URL              mirror laid out as URL/latest and URL/TAG/<asset>
#   --insecure-skip-signature   skip the signature check (checksums still verified)
#
# Environment: ICARO_INSTALL_PUBKEY overrides the embedded release public key
# (a PEM string or a path to a PEM file).
set -eu

REPO="ggrocco/icaro"
VERSION=""
BIN_DIR=""
BASE_URL=""
SKIP_SIG=0

# Replaced with the release public key (PEM) when install.sh is published
# as a release asset.
RELEASE_PUBKEY='__ICARO_RELEASE_PUBKEY__'

say() { printf '%s\n' "icaro-install: $*" >&2; }
die() { say "error: $*"; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?--version needs a value}"; shift 2 ;;
    --bin-dir) BIN_DIR="${2:?--bin-dir needs a value}"; shift 2 ;;
    --base-url) BASE_URL="${2:?--base-url needs a value}"; shift 2 ;;
    --insecure-skip-signature) SKIP_SIG=1; shift ;;
    -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
    *) die "unknown option $1" ;;
  esac
done

case "$(uname -s)" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) die "unsupported OS $(uname -s); Ícaro supports Linux and macOS" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --proto '=https,http' -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  die "need curl or wget"
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "need sha256sum or shasum"
fi

if [ "$(id -u)" -eq 0 ] && [ -z "$BIN_DIR" ]; then
  die "running as root: pass --bin-dir (e.g. /usr/local/bin) to confirm a system-wide install"
fi
[ -n "$BIN_DIR" ] || BIN_DIR="$HOME/.local/bin"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

if [ -z "$VERSION" ]; then
  if [ -n "$BASE_URL" ]; then
    fetch "$BASE_URL/latest" "$TMP/latest" || die "cannot read $BASE_URL/latest"
    VERSION="$(tr -d ' \r\n' < "$TMP/latest")"
  else
    fetch "https://api.github.com/repos/$REPO/releases/latest" "$TMP/latest.json" || die "cannot query the latest release"
    VERSION="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$TMP/latest.json" | head -n1)"
  fi
fi
[ -n "$VERSION" ] || die "could not determine the version to install"
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac

if [ -n "$BASE_URL" ]; then
  ASSETS="$BASE_URL/$VERSION"
else
  ASSETS="https://github.com/$REPO/releases/download/$VERSION"
fi
ARCHIVE="icaro_${VERSION#v}_${OS}_${ARCH}.tar.gz"

say "installing icaro $VERSION ($OS/$ARCH) into $BIN_DIR"
fetch "$ASSETS/checksums.txt" "$TMP/checksums.txt" || die "cannot download checksums.txt"
fetch "$ASSETS/$ARCHIVE" "$TMP/$ARCHIVE" || die "cannot download $ARCHIVE"

# 1. Signature over checksums.txt.
if [ "$SKIP_SIG" -eq 1 ]; then
  say "WARNING: --insecure-skip-signature: the release signature is NOT verified."
  say "WARNING: anyone who can alter the download can alter this binary."
else
  PUBKEY="${ICARO_INSTALL_PUBKEY:-$RELEASE_PUBKEY}"
  case "$PUBKEY" in
    __ICARO_RELEASE_PUBKEY__|"") die "no release public key embedded; use the install.sh published with the release, set ICARO_INSTALL_PUBKEY, or pass --insecure-skip-signature" ;;
  esac
  if [ -f "$PUBKEY" ]; then cp "$PUBKEY" "$TMP/key.pem"; else printf '%s\n' "$PUBKEY" > "$TMP/key.pem"; fi
  command -v openssl >/dev/null 2>&1 || die "openssl (3.0+) is required to verify the ed25519 signature; install it or pass --insecure-skip-signature"
  fetch "$ASSETS/checksums.txt.sig" "$TMP/checksums.txt.sig" || die "cannot download checksums.txt.sig"
  if ! openssl pkeyutl -verify -pubin -inkey "$TMP/key.pem" -rawin -in "$TMP/checksums.txt" -sigfile "$TMP/checksums.txt.sig" >/dev/null 2>&1; then
    die "signature verification FAILED for checksums.txt (or this openssl lacks ed25519 -rawin support; OpenSSL 3.0+ is required)"
  fi
  say "signature OK"
fi

# 2. Archive checksum against the (signed) list.
WANT="$(awk -v f="$ARCHIVE" '$2 == f || $2 == "*"f {print $1}' "$TMP/checksums.txt" | head -n1)"
[ -n "$WANT" ] || die "$ARCHIVE is not listed in checksums.txt"
GOT="$(sha256 "$TMP/$ARCHIVE")"
[ "$WANT" = "$GOT" ] || die "checksum mismatch for $ARCHIVE (want $WANT, got $GOT); refusing to install"
say "checksum OK"

mkdir -p "$TMP/x"
tar -xzf "$TMP/$ARCHIVE" -C "$TMP/x"
[ -f "$TMP/x/icaro" ] || die "archive does not contain the icaro binary"

mkdir -p "$BIN_DIR"
cp "$TMP/x/icaro" "$BIN_DIR/.icaro.tmp.$$"
chmod 0755 "$BIN_DIR/.icaro.tmp.$$"
mv -f "$BIN_DIR/.icaro.tmp.$$" "$BIN_DIR/icaro"
say "installed $BIN_DIR/icaro"
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "add $BIN_DIR to your PATH" ;;
esac
say "next: icaro init && icaro doctor"
