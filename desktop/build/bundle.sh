#!/bin/sh
# Builds Icaro.app (ADR 0027): the menu bar app plus the icaro CLI it
# starts the daemon with, as a universal (arm64 + x86_64) bundle.
#
#   desktop/build/bundle.sh OUT_DIR VERSION
#
# Signing: with MACOS_SIGN_IDENTITY set (a "Developer ID Application"
# identity in the keychain), every binary and the bundle are signed with
# the hardened runtime and a timestamp, as notarization requires.
# Without it the bundle is signed ad hoc, which runs on the machine that
# built it but is not notarizable. ICARO_RELEASE_PUBKEY, when set, is
# embedded in the bundled CLI as in the release build (icaro update).
set -eu

out=${1:?usage: bundle.sh OUT_DIR VERSION}
version=${2:?usage: bundle.sh OUT_DIR VERSION}
[ "$(uname -s)" = Darwin ] || { echo "bundle.sh needs macOS (lipo, iconutil, codesign)" >&2; exit 1; }

here=$(cd "$(dirname "$0")" && pwd)
desktop=$(dirname "$here")
root=$(dirname "$desktop")
app=$out/Icaro.app
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

cli_ldflags="-s -w -X icaro/internal/cli.version=$version"
if [ -n "${ICARO_RELEASE_PUBKEY:-}" ]; then
  cli_ldflags="$cli_ldflags -X icaro/internal/update.DefaultPublicKey=$ICARO_RELEASE_PUBKEY"
fi
for arch in arm64 amd64; do
  (cd "$desktop" && CGO_ENABLED=1 GOOS=darwin GOARCH=$arch MACOSX_DEPLOYMENT_TARGET=12.0 \
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$tmp/icaro-app-$arch" .)
  (cd "$root" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch \
    go build -trimpath -ldflags "$cli_ldflags" -o "$tmp/icaro-$arch" ./cmd/icaro)
done
lipo -create -output "$app/Contents/MacOS/icaro-app" "$tmp/icaro-app-arm64" "$tmp/icaro-app-amd64"
lipo -create -output "$app/Contents/MacOS/icaro" "$tmp/icaro-arm64" "$tmp/icaro-amd64"

sed "s/@VERSION@/${version#v}/g" "$here/Info.plist" > "$app/Contents/Info.plist"
plutil -lint "$app/Contents/Info.plist" >/dev/null

iconset=$tmp/AppIcon.iconset
mkdir -p "$iconset"
for size in 16 32 128 256 512; do
  sips -z $size $size "$desktop/icons/app.png" --out "$iconset/icon_${size}x${size}.png" >/dev/null
  sips -z $((size * 2)) $((size * 2)) "$desktop/icons/app.png" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns -o "$app/Contents/Resources/AppIcon.icns" "$iconset"

if [ -n "${MACOS_SIGN_IDENTITY:-}" ]; then
  for bin in "$app/Contents/MacOS/icaro" "$app/Contents/MacOS/icaro-app"; do
    codesign --force --options runtime --timestamp --sign "$MACOS_SIGN_IDENTITY" "$bin"
  done
  codesign --force --options runtime --timestamp --sign "$MACOS_SIGN_IDENTITY" "$app"
else
  codesign --force --deep --sign - "$app"
fi
codesign --verify --strict --verbose=2 "$app"
echo "$app"
