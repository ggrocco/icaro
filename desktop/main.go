// Command icaro-app is the Ícaro desktop app (Phase 7, ADR 0027): a menu
// bar (tray) app on top of the daemon. It holds no business logic; it
// renders what internal/desktop decides and passes clicks back to it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"icaro/desktop/icons"
	"icaro/internal/desktop"
)

// version is set at build time (-ldflags -X main.version=...).
var version = "dev"

// BundleID is the app's identifier: the bundle id of Icaro.app, the
// single-instance key, and the login item's identifier.
const BundleID = "dev.icaro.app"

func main() {
	var o desktop.Options
	var trayOnly, showVersion, debug bool
	flag.StringVar(&o.ConfigPath, "config", "", "config file (as icaro --config)")
	flag.StringVar(&o.Home, "home", "", "state directory (as icaro --home)")
	flag.StringVar(&o.Icaro, "icaro", "", "icaro binary that starts the daemon (default: search, or $"+desktop.IcaroEnv+")")
	flag.BoolVar(&trayOnly, "tray-only", false, "open no window at launch")
	flag.BoolVar(&debug, "debug", false, "log menu updates and notifications to stderr")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Parse()
	if showVersion {
		fmt.Println("icaro-app", version)
		return
	}
	level := slog.LevelWarn
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	if err := run(o, trayOnly, level); err != nil {
		fmt.Fprintln(os.Stderr, "icaro-app:", err)
		os.Exit(1)
	}
}

func run(o desktop.Options, trayOnly bool, level slog.Level) error {
	d, err := desktop.Open(o)
	if err != nil {
		return err
	}
	ns := notifications.New()
	s := &shell{notes: ns}
	ctrl := &desktop.Controller{Daemon: d, UI: s, PrefsPath: desktop.PrefsPath(d.Layout), ForceTrayOnly: trayOnly}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.app = application.New(application.Options{
		Name:        "Ícaro",
		Description: "Parallel isolated coding agents",
		LogLevel:    level,
		Services:    []application.Service{application.NewService(ns)},
		// The app lives in the menu bar: no Dock icon, and closing the
		// window keeps it running.
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyAccessory,
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		Linux: application.LinuxOptions{ProgramName: "icaro-app"},
		// A second launch (Finder, Spotlight, `open`) shows the running
		// app's window instead of starting another tray.
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: BundleID,
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				ctrl.Act(ctx, desktop.Action{Kind: desktop.ActOpenUI})
			},
		},
		OnShutdown: cancel,
	})
	if login := newLoginItem(s.app); login != nil {
		ctrl.Login = login
	}

	s.tray = s.app.SystemTray.New()
	if runtime.GOOS == "darwin" {
		s.tray.SetTemplateIcon(icons.TrayTemplate)
	} else {
		s.tray.SetIcon(icons.Tray)
	}
	s.tray.SetTooltip("Ícaro")
	s.menu = s.app.NewMenu()
	s.tray.SetMenu(s.menu)
	s.act = func(a desktop.Action) { ctrl.Act(ctx, a) }

	ns.OnNotificationResponse(func(r notifications.NotificationResult) {
		if r.Error == nil {
			s.act(actionFromData(r.Response.UserInfo))
		}
	})
	s.app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		if _, err := ns.RequestNotificationAuthorization(); err != nil {
			slog.Warn("notifications are off", "error", err)
		}
		go func() { _ = ctrl.Run(ctx) }()
	})
	return s.app.Run()
}
