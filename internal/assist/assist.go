// Package assist is the workflow assistant chat (ADR 0035). Each chat
// runs the profile's own agent CLI headless, one process per turn, in an
// assist session; the CLI reaches the authoring tools over the session's
// MCP socket, where writes land as drafts a person applies and runs wait
// for a person's approval. Ícaro never handles the subscription login: it
// only runs the vendor's CLI under it.
package assist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"icaro/internal/config"
	"icaro/internal/creds"
	"icaro/internal/service"
	"icaro/internal/session"
	"icaro/internal/store"
	"icaro/internal/workflow"
	"icaro/internal/workflow/events"
)

// Sessions is the part of the session manager a chat uses.
type Sessions interface {
	CreateAssist(ctx context.Context, req session.AssistRequest) (*session.Session, error)
	Get(ctx context.Context, id string) (*session.Session, error)
	Start(ctx context.Context, id string) (*session.Session, error)
	Stop(ctx context.Context, id string) (*session.Session, error)
	Remove(ctx context.Context, id string, keepWorktree bool) error
	RunTurn(ctx context.Context, id string, req session.TurnRequest, out io.Writer) (session.TurnResult, error)
	CancelTurn(ctx context.Context, id string) error
}

// Workflows is the part of the service layer the chat's tools use.
type Workflows interface {
	ValidateWorkflow(ctx context.Context, yaml string) (*workflow.Spec, workflow.Issues, error)
	ApplyWorkflow(ctx context.Context, yaml, actor string) (*service.WorkflowInfo, error)
	GetWorkflow(ctx context.Context, name string) (*service.WorkflowInfo, error)
	StartRun(ctx context.Context, name string, trigger map[string]any, actor string) (*service.RunInfo, error)
}

// SecretValues lists secret values for redaction.
type SecretValues interface {
	Values(ctx context.Context) ([]string, error)
}

// Options wire a Service.
type Options struct {
	Store     *store.Store
	Config    *config.Config
	Sessions  Sessions
	Workflows Workflows
	Secrets   SecretValues // optional
	Events    *events.Bus  // optional
	// Memories gives new conversations notes from earlier chats and
	// stores what the distiller writes (ADR 0036); optional.
	Memories Memories
	Log      *slog.Logger
	// LogsDir holds one append-only JSONL log per chat.
	LogsDir string
	// Now is the clock (tests).
	Now func() time.Time
	// Poll is how often a turn re-reads its session while it starts.
	Poll time.Duration
}

// Errors the API maps to statuses.
var (
	ErrNotFound = errors.New("chat not found")
	ErrInvalid  = errors.New("invalid request")
	ErrBusy     = errors.New("the chat is busy")
)

// Service runs chats. Each chat has one goroutine (its actor) that owns
// the chat's log and queue of turns; everyone else sends it messages.
type Service struct {
	o       Options
	actors  *owned[*actor]
	waiters *owned[chan string]
	base    context.Context
	started chan struct{}
	// distiller is set by NewDistiller; chats are queued on it when idle.
	distiller *Distiller
}

// New builds a Service; call Run before using it.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Poll <= 0 {
		o.Poll = 250 * time.Millisecond
	}
	return &Service{o: o, actors: newOwned[*actor](), waiters: newOwned[chan string](), base: context.Background(), started: make(chan struct{})}
}

// Name implements app.Component.
func (s *Service) Name() string { return "assistant" }

// Run recovers from a restart, then serves until ctx ends: turns that
// were in flight are interrupted and their leftover processes signalled,
// and approvals they waited on are expired.
func (s *Service) Run(ctx context.Context) error {
	s.base = ctx
	close(s.started)
	n, err := s.o.Store.InterruptChatTurns(ctx, "the daemon restarted during this turn; retry it")
	if err != nil {
		s.o.Log.Error("assistant: interrupt turns", "err", err)
	}
	if _, err := s.o.Store.ExpireChatApprovals(ctx); err != nil {
		s.o.Log.Error("assistant: expire approvals", "err", err)
	}
	if n > 0 {
		s.o.Log.Info("assistant: interrupted turns lost in a restart", "turns", n)
		go s.killLeftovers(ctx)
	}
	<-ctx.Done()
	var all []*actor
	s.actors.do(func(m map[string]*actor) {
		for _, a := range m {
			all = append(all, a)
		}
	})
	for _, a := range all {
		a.shutdown()
	}
	return nil
}

// killLeftovers signals any turn process a restart left running.
func (s *Service) killLeftovers(ctx context.Context) {
	chats, err := s.o.Store.ListChats(ctx, 1000)
	if err != nil {
		return
	}
	for _, c := range chats {
		if c.SessionID == "" {
			continue
		}
		if sess, err := s.o.Sessions.Get(ctx, c.SessionID); err == nil && sess.Status == session.StatusRunning {
			_ = s.o.Sessions.CancelTurn(ctx, c.SessionID)
		}
	}
}

// ChatInfo is a chat as clients see it.
type ChatInfo struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Profile       string    `json:"profile"`
	Agent         string    `json:"agent"`
	ProjectID     string    `json:"project_id,omitempty"`
	Workflow      string    `json:"workflow,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	SessionStatus string    `json:"session_status,omitempty"`
	CreatedBy     string    `json:"created_by,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Usage is a turn's token count.
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read,omitempty"`
	CacheWrite int64 `json:"cache_write,omitempty"`
}

// MessageInfo is one message.
type MessageInfo struct {
	ID     string `json:"id"`
	Seq    int    `json:"seq"`
	Role   string `json:"role"`
	Text   string `json:"text"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Usage  *Usage `json:"usage,omitempty"`
	// CostUSD is the CLI's own estimate, shown only for API-key (inject)
	// profiles; a subscription turn shows tokens only.
	CostUSD    *float64   `json:"cost_usd,omitempty"`
	ResetAt    *time.Time `json:"reset_at,omitempty"`
	LogOffset  int64      `json:"log_offset"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// DraftInfo is the chat's draft.
type DraftInfo struct {
	YAML      string          `json:"yaml"`
	Issues    json.RawMessage `json:"issues"`
	Source    string          `json:"source"`
	Version   int             `json:"version"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ApprovalInfo is a gated call.
type ApprovalInfo struct {
	ID        string          `json:"id"`
	Tool      string          `json:"tool"`
	Request   json.RawMessage `json:"request"`
	Decision  string          `json:"decision,omitempty"`
	Actor     string          `json:"actor,omitempty"`
	Result    string          `json:"result,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	DecidedAt *time.Time      `json:"decided_at,omitempty"`
}

// ChatDetail is a chat with its messages, draft and pending approvals.
type ChatDetail struct {
	ChatInfo
	Messages  []MessageInfo  `json:"messages"`
	Draft     *DraftInfo     `json:"draft,omitempty"`
	Approvals []ApprovalInfo `json:"approvals"`
	// LogSize is where a client following the chat's events starts.
	LogSize int64 `json:"log_size"`
}

// CreateRequest starts a chat.
type CreateRequest struct {
	Title string `json:"title,omitempty"`
	// Workflow seeds the draft with that workflow's current YAML.
	Workflow string `json:"workflow,omitempty"`
	// YAML seeds the draft directly (an editor's unsaved document).
	YAML      string `json:"yaml,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

// CreateChat makes a chat. Its session starts with the first message.
func (s *Service) CreateChat(ctx context.Context, req CreateRequest, actor string) (*ChatDetail, error) {
	profile := s.o.Config.AssistantProfile()
	prof, ok := s.o.Config.Profiles[profile]
	if !ok {
		return nil, fmt.Errorf("%w: no profile runs claude or codex; set assistant.profile", ErrInvalid)
	}
	if req.ProjectID != "" {
		if _, err := s.o.Store.GetProject(ctx, req.ProjectID); err != nil {
			return nil, fmt.Errorf("%w: unknown project %q", ErrInvalid, req.ProjectID)
		}
	}
	yaml := req.YAML
	if req.Workflow != "" && yaml == "" {
		w, err := s.o.Workflows.GetWorkflow(ctx, req.Workflow)
		if err != nil {
			return nil, fmt.Errorf("%w: workflow %q: %v", ErrInvalid, req.Workflow, err)
		}
		yaml = w.YAML
	}
	c := &store.Chat{Title: clip(strings.TrimSpace(s.redactor(ctx).RedactString(req.Title)), 120), Profile: profile, Agent: prof.Agent, ProjectID: req.ProjectID, Workflow: req.Workflow, CreatedBy: actor}
	if err := s.o.Store.CreateChat(ctx, c); err != nil {
		return nil, err
	}
	if yaml != "" {
		if _, err := s.putDraft(ctx, c.ID, yaml, "user"); err != nil {
			return nil, err
		}
	}
	return s.GetChat(ctx, c.ID)
}

// ListChats returns chats, newest activity first.
func (s *Service) ListChats(ctx context.Context) ([]ChatInfo, error) {
	cs, err := s.o.Store.ListChats(ctx, 200)
	if err != nil {
		return nil, err
	}
	out := make([]ChatInfo, 0, len(cs))
	for _, c := range cs {
		out = append(out, s.info(ctx, c))
	}
	return out, nil
}

func (s *Service) info(ctx context.Context, c *store.Chat) ChatInfo {
	ci := ChatInfo{ID: c.ID, Title: c.Title, Profile: c.Profile, Agent: c.Agent, ProjectID: c.ProjectID, Workflow: c.Workflow,
		SessionID: c.SessionID, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if c.SessionID != "" {
		if sess, err := s.o.Sessions.Get(ctx, c.SessionID); err == nil {
			ci.SessionStatus = sess.Status
		}
	}
	return ci
}

func (s *Service) chat(ctx context.Context, id string) (*store.Chat, error) {
	c, err := s.o.Store.GetChat(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	return c, err
}

// GetChat returns a chat with its messages, draft and pending approvals.
func (s *Service) GetChat(ctx context.Context, id string) (*ChatDetail, error) {
	c, err := s.chat(ctx, id)
	if err != nil {
		return nil, err
	}
	msgs, err := s.o.Store.ListChatMessages(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &ChatDetail{ChatInfo: s.info(ctx, c), Messages: make([]MessageInfo, 0, len(msgs)), Approvals: []ApprovalInfo{}}
	for _, m := range msgs {
		d.Messages = append(d.Messages, s.message(c, m))
	}
	if dr, err := s.o.Store.GetChatDraft(ctx, id); err == nil {
		d.Draft = draftInfo(dr)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	aps, err := s.o.Store.ListChatApprovals(ctx, id, true)
	if err != nil {
		return nil, err
	}
	for _, a := range aps {
		d.Approvals = append(d.Approvals, approvalInfo(a))
	}
	if fi, err := os.Stat(s.logPath(id)); err == nil {
		d.LogSize = fi.Size()
	}
	return d, nil
}

func (s *Service) message(c *store.Chat, m *store.ChatMessage) MessageInfo {
	mi := MessageInfo{ID: m.ID, Seq: m.Seq, Role: m.Role, Text: m.Text, Status: m.Status, Error: m.Error, ResetAt: m.ResetAt,
		LogOffset: m.LogOffset, CreatedAt: m.CreatedAt, FinishedAt: m.FinishedAt}
	if m.InputTokens+m.OutputTokens+m.CacheRead+m.CacheWrite > 0 {
		mi.Usage = &Usage{Input: m.InputTokens, Output: m.OutputTokens, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite}
	}
	if m.CostMicro > 0 && len(s.o.Config.Profiles[c.Profile].Inject) > 0 {
		v := float64(m.CostMicro) / 1e6
		mi.CostUSD = &v
	}
	return mi
}

func draftInfo(d *store.ChatDraft) *DraftInfo {
	return &DraftInfo{YAML: d.YAML, Issues: json.RawMessage(d.Issues), Source: d.Source, Version: d.Version, UpdatedAt: d.UpdatedAt}
}

func approvalInfo(a *store.ChatApproval) ApprovalInfo {
	return ApprovalInfo{ID: a.ID, Tool: a.Tool, Request: json.RawMessage(a.Request), Decision: a.Decision, Actor: a.Actor, Result: a.Result,
		CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt, DecidedAt: a.DecidedAt}
}

// MaxMessage caps a user message.
const MaxMessage = 32 << 10

// Send adds a user message and queues the assistant's turn behind any
// turn already queued or running in the chat. It returns both messages.
func (s *Service) Send(ctx context.Context, chatID, text, actor string) ([]MessageInfo, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("%w: the message is empty", ErrInvalid)
	}
	if len(text) > MaxMessage || !utf8.ValidString(text) {
		return nil, fmt.Errorf("%w: the message must be valid UTF-8 of at most %d bytes", ErrInvalid, MaxMessage)
	}
	c, err := s.chat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	// Only redacted copies of the text are stored, the title included.
	redacted := s.redactor(ctx).RedactString(text)
	if c.Title == "" {
		if err := s.o.Store.SetChatField(ctx, c.ID, store.ChatTitle, clip(firstLine(redacted), 80)); err != nil {
			return nil, err
		}
	}
	a := s.actor(chatID)
	off := a.size()
	u := &store.ChatMessage{ChatID: chatID, Role: store.RoleUser, Text: redacted, Status: store.TurnDone, LogOffset: off}
	if err := s.o.Store.AddChatMessage(ctx, u); err != nil {
		return nil, err
	}
	// The turn reads the unredacted text from the queue; only the stored
	// copy is redacted.
	m := &store.ChatMessage{ChatID: chatID, Role: store.RoleAssistant, Status: store.TurnQueued, LogOffset: off}
	if err := s.o.Store.AddChatMessage(ctx, m); err != nil {
		return nil, err
	}
	a.append(entry{Type: EntryMessage, MessageID: u.ID, Message: ptr(s.message(c, u))})
	a.append(entry{Type: EntryMessage, MessageID: m.ID, Message: ptr(s.message(c, m))})
	a.enqueue(job{messageID: m.ID, text: text, actor: actor})
	return []MessageInfo{s.message(c, u), s.message(c, m)}, nil
}

// Retry queues a new turn with the text of the user message before an
// interrupted, failed or cancelled assistant message.
func (s *Service) Retry(ctx context.Context, chatID, messageID, actor string) ([]MessageInfo, error) {
	msgs, err := s.o.Store.ListChatMessages(ctx, chatID)
	if err != nil {
		return nil, err
	}
	for i, m := range msgs {
		if m.ID != messageID {
			continue
		}
		if m.Role != store.RoleAssistant || !retryable(m.Status) || i == 0 || msgs[i-1].Role != store.RoleUser {
			return nil, fmt.Errorf("%w: only an interrupted, failed or cancelled reply can be retried", ErrInvalid)
		}
		return s.Send(ctx, chatID, msgs[i-1].Text, actor)
	}
	if _, err := s.chat(ctx, chatID); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: unknown message %q", ErrInvalid, messageID)
}

func retryable(status string) bool {
	switch status {
	case store.TurnInterrupted, store.TurnError, store.TurnCancelled, store.TurnRateLimited, store.TurnAuthRequired:
		return true
	}
	return false
}

// Cancel stops the chat's running turn and drops its queued ones.
func (s *Service) Cancel(ctx context.Context, chatID string) error {
	if _, err := s.chat(ctx, chatID); err != nil {
		return err
	}
	s.actor(chatID).cancel()
	return nil
}

// PutDraft replaces the chat's draft with the person's editor document.
func (s *Service) PutDraft(ctx context.Context, chatID, yaml string) (*DraftInfo, error) {
	if _, err := s.chat(ctx, chatID); err != nil {
		return nil, err
	}
	if len(yaml) > maxDraft {
		return nil, fmt.Errorf("%w: the draft is over %d bytes", ErrInvalid, maxDraft)
	}
	return s.putDraft(ctx, chatID, yaml, "user")
}

// maxDraft caps a draft.
const maxDraft = 512 << 10

// putDraft validates yaml, stores it as the draft and logs the change.
func (s *Service) putDraft(ctx context.Context, chatID, yaml, source string) (*DraftInfo, error) {
	issues := workflow.Issues{}
	var name string
	if strings.TrimSpace(yaml) != "" {
		spec, is, err := s.o.Workflows.ValidateWorkflow(ctx, yaml)
		if err != nil {
			return nil, err
		}
		if is != nil {
			issues = is
		}
		if spec != nil {
			name = spec.Name
		}
	}
	raw, _ := json.Marshal(issues)
	d, err := s.o.Store.PutChatDraft(ctx, chatID, yaml, string(raw), source)
	if err != nil {
		return nil, err
	}
	if name != "" {
		if c, err := s.o.Store.GetChat(ctx, chatID); err == nil && c.Workflow == "" {
			_ = s.o.Store.SetChatField(ctx, chatID, store.ChatWorkflow, name)
		}
	}
	info := draftInfo(d)
	s.actor(chatID).append(entry{Type: EntryDraft, Draft: &DraftEntry{Version: d.Version, Source: source, Issues: len(issues)}})
	return info, nil
}

// Apply saves yaml, the person's editor document after they accepted the
// assistant's proposal, as a new workflow version attributed to them via
// this chat ("<actor> via assistant <chat>"), and notes it in the chat.
func (s *Service) Apply(ctx context.Context, chatID, yaml, actor string) (*service.WorkflowInfo, error) {
	if _, err := s.chat(ctx, chatID); err != nil {
		return nil, err
	}
	if actor == "" {
		actor = "someone"
	}
	w, err := s.o.Workflows.ApplyWorkflow(ctx, yaml, actor+" via assistant "+chatID)
	if err != nil {
		return nil, err
	}
	_ = s.o.Store.SetChatField(ctx, chatID, store.ChatWorkflow, w.Name)
	s.actor(chatID).append(entry{Type: EntryStatus, Note: fmt.Sprintf("Saved as %s v%d.", w.Name, w.Version)})
	return w, nil
}

// Decide approves or declines a gated call.
func (s *Service) Decide(ctx context.Context, chatID, approvalID string, approve bool, actor string) (*ApprovalInfo, error) {
	a, err := s.o.Store.GetChatApproval(ctx, approvalID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && a.ChatID != chatID) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	decision := DecisionDeclined
	if approve {
		decision = DecisionApproved
	}
	a, err = s.o.Store.DecideChatApproval(ctx, approvalID, decision, actor, "")
	if errors.Is(err, store.ErrConflict) {
		return nil, fmt.Errorf("%w: this call was already decided", ErrBusy)
	}
	if err != nil {
		return nil, err
	}
	var ch chan string
	s.waiters.do(func(m map[string]chan string) { ch = m[approvalID] })
	if ch != nil {
		select {
		case ch <- decision:
		default:
		}
	}
	info := approvalInfo(a)
	s.actor(chatID).append(entry{Type: EntryApproval, Approval: &info})
	return &info, nil
}

// Decisions on gated calls.
const (
	DecisionApproved = "approved"
	DecisionDeclined = "declined"
	DecisionExpired  = "expired"
)

// Delete removes a chat: its turn is cancelled, its session removed, and
// its rows and log deleted.
func (s *Service) Delete(ctx context.Context, chatID string) error {
	c, err := s.chat(ctx, chatID)
	if err != nil {
		return err
	}
	var a *actor
	s.actors.do(func(m map[string]*actor) { a = m[chatID]; delete(m, chatID) })
	if a != nil {
		a.shutdown()
	}
	if c.SessionID != "" {
		if err := s.o.Sessions.Remove(ctx, c.SessionID, false); err != nil && !errors.Is(err, session.ErrNotFound) {
			return err
		}
	}
	if err := s.o.Store.DeleteChat(ctx, chatID); err != nil {
		return err
	}
	// The memories it left stay: they are knowledge, not the chat.
	_ = s.o.Store.DeleteDistill(ctx, chatID)
	_ = os.Remove(s.logPath(chatID))
	return nil
}

// LogLine is one whole line of a chat's log and the offset after it, where
// a reader resumes.
type LogLine struct {
	Data json.RawMessage
	Next int64
}

// Entries reads the chat's log from offset, returning only whole lines
// and the offset after the last one.
func (s *Service) Entries(ctx context.Context, chatID string, offset int64) ([]LogLine, int64, error) {
	if _, err := s.chat(ctx, chatID); err != nil {
		return nil, offset, err
	}
	f, err := os.Open(s.logPath(chatID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, offset, err
	}
	defer func() { _ = f.Close() }()
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxRead))
	if err != nil {
		return nil, offset, err
	}
	var out []LogLine
	for len(buf) > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		line := buf[:i]
		buf = buf[i+1:]
		offset += int64(i + 1)
		if json.Valid(line) {
			out = append(out, LogLine{Data: json.RawMessage(append([]byte(nil), line...)), Next: offset})
		}
	}
	return out, offset, nil
}

// maxRead caps one Entries read.
const maxRead = 1 << 20

// Topic is the event bus key a chat's doorbell rings on.
func Topic(chatID string) string { return "chat:" + chatID }

// LogPath is where a chat's log lives.
func (s *Service) logPath(chatID string) string { return filepath.Join(s.o.LogsDir, chatID+".jsonl") }

func (s *Service) actor(chatID string) *actor {
	var a *actor
	s.actors.do(func(m map[string]*actor) {
		if a = m[chatID]; a == nil {
			a = newActor(s, chatID)
			m[chatID] = a
		}
	})
	return a
}

func (s *Service) agentFor(profile string) (creds.Agent, error) {
	return creds.LookupAgent(s.o.Config.Profiles[profile].Agent)
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func ptr[T any](v T) *T { return &v }

// owned is a map owned by one goroutine; callers send it operations
// instead of sharing it under a mutex.
type owned[V any] struct {
	ops chan func(map[string]V)
}

func newOwned[V any]() *owned[V] {
	o := &owned[V]{ops: make(chan func(map[string]V))}
	go func() {
		m := map[string]V{}
		for op := range o.ops {
			op(m)
		}
	}()
	return o
}

// do runs fn on the owning goroutine and waits for it.
func (o *owned[V]) do(fn func(map[string]V)) {
	done := make(chan struct{})
	o.ops <- func(m map[string]V) { fn(m); close(done) }
	<-done
}
