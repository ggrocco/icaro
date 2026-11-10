Ícaro desktop app @VERSION@ for Linux

A tray app on top of the Ícaro daemon. It needs Docker, GTK 4 and
WebKitGTK 6 (Debian/Ubuntu: libgtk-4-1 libwebkitgtk-6.0-4; Fedora: gtk4
webkitgtk6.0), and a desktop that hosts StatusNotifier tray items (KDE,
or GNOME with the AppIndicator extension).

Install into ~/.local, from the directory that holds the archive:

  tar -xzf Icaro_@VERSION@_linux_*.tar.gz --strip-components=1 -C ~/.local

That places:

  bin/icaro-app                              the app
  bin/icaro                                  the CLI the app starts the daemon with
  share/applications/dev.icaro.app.desktop   the launcher entry
  share/icons/hicolor/scalable/apps/         its icon
  share/doc/icaro-app/                       licenses and this file

Start "Ícaro" from the launcher, or run icaro-app (~/.local/bin must be on
PATH). To install elsewhere, extract with -C /some/prefix; the app looks
for icaro next to itself first.

Documentation: https://github.com/ggrocco/icaro/blob/main/docs/desktop.md
