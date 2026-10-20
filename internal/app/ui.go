package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"icaro/internal/api"
)

func init() {
	RegisterWirer(75, "ui", func(_ context.Context, a *App) error {
		ui := &uiListener{addr: a.Cfg.Daemon.UIListen, handler: a.Handler, log: a, reqs: make(chan chan uiResult)}
		a.API.RegisterUI(api.UIOptions{Open: ui.Open})
		a.Add(ui)
		return nil
	})
}

type uiResult struct {
	url string
	err error
}

// uiListener owns the loopback UI listener (ADR 0023). It is started on
// the first `icaro ui`, not at boot, so no TCP port is open by default.
// One goroutine owns the server; Open talks to it over a channel.
type uiListener struct {
	addr    string
	handler func() http.Handler
	log     *App
	reqs    chan chan uiResult
}

func (u *uiListener) Name() string { return "ui" }

// Open starts the listener if needed and returns its base URL.
func (u *uiListener) Open() (string, error) {
	if u.addr == "" {
		return "", errors.New("daemon.ui_listen is empty; set it to a loopback address such as 127.0.0.1:8787")
	}
	reply := make(chan uiResult, 1)
	select {
	case u.reqs <- reply:
	case <-time.After(5 * time.Second):
		return "", errors.New("ui listener is not running")
	}
	r := <-reply
	return r.url, r.err
}

func (u *uiListener) Run(ctx context.Context) error {
	var srv *http.Server
	var url string
	for {
		select {
		case <-ctx.Done():
			if srv != nil {
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = srv.Shutdown(sctx)
				cancel()
			}
			return nil
		case reply := <-u.reqs:
			if srv != nil {
				reply <- uiResult{url: url}
				continue
			}
			ln, err := net.Listen("tcp", u.addr)
			if err != nil {
				reply <- uiResult{err: fmt.Errorf("open UI listener on %s: %w", u.addr, err)}
				continue
			}
			srv = &http.Server{
				Handler:           u.handler(),
				ReadHeaderTimeout: 10 * time.Second,
				BaseContext:       func(net.Listener) context.Context { return ctx },
				ConnContext:       ConnContext,
			}
			go func(s *http.Server) {
				if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					u.log.Log.Error("ui listener", "err", err)
				}
			}(srv)
			url = "http://" + ln.Addr().String()
			u.log.Log.Info("ui listening (token or session cookie required)", "url", url)
			reply <- uiResult{url: url}
		}
	}
}
