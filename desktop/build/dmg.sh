#!/bin/sh
# Wraps Icaro.app (built by bundle.sh) in a disk image with a link to
# /Applications, for drag-to-install (ADR 0027).
#
#   desktop/build/dmg.sh OUT_DIR VERSION
#
# Reads OUT_DIR/Icaro.app, writes OUT_DIR/Icaro_<version>_macos_universal.dmg
# and prints its path. Notarize and staple the app first, so the copy in
# the image carries its ticket. With MACOS_SIGN_IDENTITY set the image
# itself is signed, which notarizing the image requires.
set -eu

out=${1:?usage: dmg.sh OUT_DIR VERSION}
version=${2:?usage: dmg.sh OUT_DIR VERSION}
[ "$(uname -s)" = Darwin ] || { echo "dmg.sh needs macOS (hdiutil, ditto)" >&2; exit 1; }
[ -d "$out/Icaro.app" ] || { echo "dmg.sh: $out/Icaro.app not found; run bundle.sh first" >&2; exit 1; }

dmg=$out/Icaro_${version#v}_macos_universal.dmg
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# ditto keeps the signature's extended attributes and the stapled ticket.
ditto "$out/Icaro.app" "$tmp/Icaro.app"
ln -s /Applications "$tmp/Applications"
rm -f "$dmg"
hdiutil create -quiet -volname "Icaro" -srcfolder "$tmp" -fs HFS+ -format UDZO "$dmg"

if [ -n "${MACOS_SIGN_IDENTITY:-}" ]; then
  codesign --force --timestamp --sign "$MACOS_SIGN_IDENTITY" "$dmg"
fi
hdiutil verify -quiet "$dmg"
echo "$dmg"
