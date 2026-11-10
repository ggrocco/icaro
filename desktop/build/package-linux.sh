#!/bin/sh
# Packages the desktop app for Linux (ADR 0027): the tray app plus the
# icaro CLI it starts the daemon with, as a tarball laid out like an
# install prefix, for the architecture of the machine that builds it.
#
#   desktop/build/package-linux.sh OUT_DIR VERSION
#
# Writes OUT_DIR/Icaro_<version>_linux_<arch>.tar.gz and prints its path.
# Install it with
#   tar -xzf Icaro_*.tar.gz --strip-components=1 -C ~/.local
#
# The app links the system's GTK 4 and WebKitGTK 6 (the build needs
# libgtk-4-dev and libwebkitgtk-6.0-dev), so it runs on distributions at
# least as new as the one that built it. ICARO_RELEASE_PUBKEY, when set,
# is embedded in the bundled CLI as in the release build (icaro update).
set -eu

out=${1:?usage: package-linux.sh OUT_DIR VERSION}
version=${2:?usage: package-linux.sh OUT_DIR VERSION}
[ "$(uname -s)" = Linux ] || { echo "package-linux.sh needs Linux (GTK 4, WebKitGTK 6)" >&2; exit 1; }

here=$(cd "$(dirname "$0")" && pwd)
desktop=$(dirname "$here")
root=$(dirname "$desktop")
arch=$(go env GOARCH)
name=Icaro_${version#v}_linux_$arch
mkdir -p "$out"
out=$(cd "$out" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
stage=$tmp/$name

mkdir -p "$stage/bin" "$stage/share/applications" \
  "$stage/share/icons/hicolor/scalable/apps" "$stage/share/doc/icaro-app"

cli_ldflags="-s -w -X icaro/internal/cli.version=$version"
if [ -n "${ICARO_RELEASE_PUBKEY:-}" ]; then
  cli_ldflags="$cli_ldflags -X icaro/internal/update.DefaultPublicKey=$ICARO_RELEASE_PUBKEY"
fi
(cd "$desktop" && CGO_ENABLED=1 GOOS=linux GOARCH="$arch" \
  go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/bin/icaro-app" .)
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
  go build -trimpath -ldflags "$cli_ldflags" -o "$stage/bin/icaro" ./cmd/icaro)

cp "$here/dev.icaro.app.desktop" "$stage/share/applications/dev.icaro.app.desktop"
cp "$root/site/icaro.svg" "$stage/share/icons/hicolor/scalable/apps/dev.icaro.app.svg"
cp "$root/LICENSE" "$stage/share/doc/icaro-app/LICENSE"
cp "$desktop/THIRD_PARTY_LICENSES.linux.md" "$stage/share/doc/icaro-app/THIRD_PARTY_LICENSES.md"
cp "$root/THIRD_PARTY_LICENSES.md" "$stage/share/doc/icaro-app/THIRD_PARTY_LICENSES.cli.md"
sed "s/@VERSION@/${version#v}/g" "$here/README.linux.txt" > "$stage/share/doc/icaro-app/README.txt"

# Root-owned entries with sorted names, so the archive does not depend on
# the build user.
tar -C "$tmp" --sort=name --owner=0 --group=0 --numeric-owner -czf "$out/$name.tar.gz" "$name"
echo "$out/$name.tar.gz"
