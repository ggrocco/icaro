package inject

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"icaro/internal/logs"
)

// Entry is one audited provider request. It never holds a body, a header
// value or the key.
type Entry struct {
	Time          time.Time `json:"time"`
	Session       string    `json:"session"`
	Profile       string    `json:"profile"`
	Provider      string    `json:"provider"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Status        int       `json:"status"`
	RequestBytes  int64     `json:"request_bytes"`
	ResponseBytes int64     `json:"response_bytes"`
	DurationMS    int64     `json:"duration_ms"`
	Error         string    `json:"error,omitempty"`
}

// Audit records requests made through the injector.
type Audit interface{ Record(Entry) }

// FileAudit appends one JSON line per request to <Dir>/<session>.log. Each
// line is a single O_APPEND write, so concurrent requests of a session do
// not interleave and no lock is needed.
type FileAudit struct {
	Dir string
	Log *slog.Logger
}

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Path is the audit log of a session.
func (a *FileAudit) Path(sessionID string) string { return filepath.Join(a.Dir, sessionID+".log") }

// Record implements Audit. Failures are logged, never returned: auditing
// must not break the agent's request after it has been answered.
func (a *FileAudit) Record(e Entry) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if !sessionIDRe.MatchString(e.Session) {
		a.warn("injection audit: refusing an odd session id", "session", e.Session)
		return
	}
	// An Entry holds only strings and numbers, so it always marshals.
	line, _ := json.Marshal(e) //nolint:errchkjson // see above
	// Nothing secret is put in an entry; the redactor is the backstop.
	line = append([]byte(logs.RedactString(string(line), nil)), '\n')
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		a.warn("injection audit", "err", err)
		return
	}
	f, err := os.OpenFile(a.Path(e.Session), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		a.warn("injection audit", "err", err)
		return
	}
	if _, err := f.Write(line); err != nil {
		a.warn("injection audit", "err", err)
	}
	_ = f.Close()
}

func (a *FileAudit) warn(msg string, args ...any) {
	if a.Log != nil {
		a.Log.Warn(msg, args...)
	}
}
