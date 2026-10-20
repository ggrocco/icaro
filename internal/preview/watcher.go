package preview

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxScanDepth bounds how deep below a root candidates are searched
// (worktrees live at <root>/<project>/<session>).
const maxScanDepth = 3

// skipDirs are never searched for compose files.
var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "target": true, "dist": true, "build": true}

// Scan returns candidate directories (holding a compose file) below roots
// with their config hashes. Search stops at a candidate: nested compose
// files belong to the candidate's own project.
func Scan(roots []string) map[string]string {
	out := map[string]string{}
	for _, root := range roots {
		scanDir(root, 0, out)
	}
	return out
}

func scanDir(dir string, depth int, out map[string]string) {
	if FindComposeFile(dir) != "" {
		if p, err := NormalizePath(dir); err == nil {
			if h, err := ConfigHash(dir); err == nil {
				out[p] = h
			}
		}
		return
	}
	if depth >= maxScanDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || skipDirs[e.Name()] {
			continue
		}
		scanDir(filepath.Join(dir, e.Name()), depth+1, out)
	}
}

// watcher polls the roots. Polling is portable and behaves the same on
// macOS bind mounts and Linux; a change is applied only after it holds for
// one more interval (debounce), so a half-written compose file or a
// checkout in progress does not restart a preview.
type watcher struct {
	roots    []string
	interval time.Duration
	sync     func(ctx context.Context, path string) error
	remove   func(ctx context.Context, path string) error
	log      *slog.Logger
	applied  map[string]string
	pending  map[string]string
}

func newWatcher(roots []string, interval time.Duration, sync, remove func(ctx context.Context, path string) error, log *slog.Logger) *watcher {
	return &watcher{roots: roots, interval: interval, sync: sync, remove: remove, log: log, applied: map[string]string{}}
}

func (w *watcher) run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		w.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tick scans once and applies changes that were also seen by the previous
// scan.
func (w *watcher) tick(ctx context.Context) {
	seen := Scan(w.roots)
	prev := w.pending
	w.pending = seen
	if prev == nil {
		return // first scan only primes the debounce
	}
	for p, h := range seen {
		if prev[p] != h || w.applied[p] == h {
			continue
		}
		w.applied[p] = h
		if err := w.sync(ctx, p); err != nil && ctx.Err() == nil {
			w.log.Warn("preview sync from watcher", "path", p, "err", err)
		}
	}
	for p := range w.applied {
		if _, ok := seen[p]; ok {
			continue
		}
		if _, stillPrev := prev[p]; stillPrev {
			continue // vanished this scan only; wait one more interval
		}
		delete(w.applied, p)
		if err := w.remove(ctx, p); err != nil && ctx.Err() == nil {
			w.log.Warn("preview cleanup from watcher", "path", p, "err", err)
		}
	}
}
