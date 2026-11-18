package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Chat is a workflow assistant chat (ADR 0035).
type Chat struct {
	ID             string
	Title          string
	Profile        string
	Agent          string
	ProjectID      string
	Workflow       string
	SessionID      string
	ConversationID string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Chat message roles and turn statuses.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"

	TurnQueued       = "queued"
	TurnRunning      = "running"
	TurnDone         = "done"
	TurnError        = "error"
	TurnCancelled    = "cancelled"
	TurnInterrupted  = "interrupted"
	TurnRateLimited  = "rate_limited"
	TurnAuthRequired = "auth_required"
)

// ChatMessage is one visible message; an assistant message is one turn.
type ChatMessage struct {
	ID           string
	ChatID       string
	Seq          int
	Role         string
	Text         string
	Status       string
	Error        string
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
	// CostMicro is the CLI's own cost figure in millionths of a dollar
	// (API-key profiles only; an estimate).
	CostMicro  int64
	LogOffset  int64
	ResetAt    *time.Time
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// ChatDraft is a chat's current draft.
type ChatDraft struct {
	ChatID    string
	YAML      string
	Issues    string // JSON array
	Source    string // user | assistant
	Version   int
	UpdatedAt time.Time
}

// ChatApproval is a gated tool call waiting for a person.
type ChatApproval struct {
	ID        string
	ChatID    string
	Tool      string
	Request   string // JSON
	Decision  string // "" while pending: approved | declined | expired
	Actor     string
	Result    string
	CreatedAt time.Time
	ExpiresAt time.Time
	DecidedAt *time.Time
}

// MaxChatText caps a message's text in SQL; the full stream is in the
// chat's log file.
const MaxChatText = 64 << 10

const chatCols = `id, title, profile, agent, project_id, workflow, session_id, conversation_id, created_by, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanChat(sc scanner) (*Chat, error) {
	var c Chat
	var created, updated string
	err := sc.Scan(&c.ID, &c.Title, &c.Profile, &c.Agent, &c.ProjectID, &c.Workflow, &c.SessionID, &c.ConversationID, &c.CreatedBy, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt = parseTS(created), parseTS(updated)
	return &c, nil
}

// CreateChat inserts a chat; ID, CreatedAt and UpdatedAt are filled in.
func (s *Store) CreateChat(ctx context.Context, c *Chat) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	now := Now()
	c.CreatedAt, c.UpdatedAt = now, now
	_, err := s.exec(ctx, `INSERT INTO chats (`+chatCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Title, c.Profile, c.Agent, c.ProjectID, c.Workflow, c.SessionID, c.ConversationID, c.CreatedBy, ts(now), ts(now))
	return err
}

// GetChat returns a chat.
func (s *Store) GetChat(ctx context.Context, id string) (*Chat, error) {
	return scanChat(s.queryRow(ctx, `SELECT `+chatCols+` FROM chats WHERE id = ?`, id))
}

// GetChatBySession returns the chat an assist session belongs to.
func (s *Store) GetChatBySession(ctx context.Context, sessionID string) (*Chat, error) {
	if sessionID == "" {
		return nil, ErrNotFound
	}
	return scanChat(s.queryRow(ctx, `SELECT `+chatCols+` FROM chats WHERE session_id = ?`, sessionID))
}

// Chat fields SetChatField may write. Writing one field at a time keeps
// concurrent writers (a turn recording its conversation id, a rename from
// the API) from undoing each other.
const (
	ChatTitle          = "title"
	ChatWorkflow       = "workflow"
	ChatSessionID      = "session_id"
	ChatConversationID = "conversation_id"
)

// SetChatField writes one of the chat's mutable fields and bumps
// UpdatedAt.
func (s *Store) SetChatField(ctx context.Context, id, field, value string) error {
	switch field {
	case ChatTitle, ChatWorkflow, ChatSessionID, ChatConversationID:
	default:
		return fmt.Errorf("store: chat field %q is not writable", field)
	}
	res, err := s.exec(ctx, `UPDATE chats SET `+field+` = ?, updated_at = ? WHERE id = ?`, value, ts(Now()), id) //nolint:gosec // field is from the allowlist above
	return wrote(res, err)
}

// ListChats returns chats, most recently updated first.
func (s *Store) ListChats(ctx context.Context, limit int) ([]*Chat, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.query(ctx, `SELECT `+chatCols+` FROM chats ORDER BY updated_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateChat writes the chat's mutable fields and bumps UpdatedAt.
func (s *Store) UpdateChat(ctx context.Context, c *Chat) error {
	c.UpdatedAt = Now()
	res, err := s.exec(ctx, `UPDATE chats SET title = ?, workflow = ?, session_id = ?, conversation_id = ?, updated_at = ? WHERE id = ?`,
		c.Title, c.Workflow, c.SessionID, c.ConversationID, ts(c.UpdatedAt), c.ID)
	return wrote(res, err)
}

// DeleteChat removes a chat with its messages, draft and approvals.
func (s *Store) DeleteChat(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		// Children first: SQLite enforces the cascade only with foreign
		// keys on, and deleting them explicitly keeps both backends alike.
		for _, q := range []string{`DELETE FROM chat_approvals WHERE chat_id = ?`, `DELETE FROM chat_drafts WHERE chat_id = ?`, `DELETE FROM chat_messages WHERE chat_id = ?`} {
			if _, err := tx.ExecContext(ctx, s.rebind(q), id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM chats WHERE id = ?`), id)
		return wrote(res, err)
	})
}

// wrote returns err, or ErrNotFound when the statement changed no row.
func wrote(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	return affected(res)
}

const chatMessageCols = `id, chat_id, seq, role, text, status, error, input_tokens, output_tokens, cache_read, cache_write, cost_micro, log_offset, reset_at, created_at, finished_at`

func scanChatMessage(sc scanner) (*ChatMessage, error) {
	var m ChatMessage
	var created string
	var reset, finished sql.NullString
	err := sc.Scan(&m.ID, &m.ChatID, &m.Seq, &m.Role, &m.Text, &m.Status, &m.Error, &m.InputTokens, &m.OutputTokens,
		&m.CacheRead, &m.CacheWrite, &m.CostMicro, &m.LogOffset, &reset, &created, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt, m.ResetAt, m.FinishedAt = parseTS(created), parseTSPtr(reset), parseTSPtr(finished)
	return &m, nil
}

// AddChatMessage appends a message to its chat (Seq is assigned) and
// bumps the chat's UpdatedAt. Text is capped at MaxChatText.
func (s *Store) AddChatMessage(ctx context.Context, m *ChatMessage) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	m.Text = capText(m.Text)
	m.CreatedAt = Now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		var seq int
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COALESCE(MAX(seq), 0) FROM chat_messages WHERE chat_id = ?`), m.ChatID).Scan(&seq); err != nil {
			return err
		}
		m.Seq = seq + 1
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO chat_messages (`+chatMessageCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			m.ID, m.ChatID, m.Seq, m.Role, m.Text, m.Status, m.Error, m.InputTokens, m.OutputTokens, m.CacheRead, m.CacheWrite,
			m.CostMicro, m.LogOffset, tsPtr(m.ResetAt), ts(m.CreatedAt), tsPtr(m.FinishedAt)); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE chats SET updated_at = ? WHERE id = ?`), ts(m.CreatedAt), m.ChatID)
		return wrote(res, err)
	})
}

// GetChatMessage returns a message.
func (s *Store) GetChatMessage(ctx context.Context, id string) (*ChatMessage, error) {
	return scanChatMessage(s.queryRow(ctx, `SELECT `+chatMessageCols+` FROM chat_messages WHERE id = ?`, id))
}

// ListChatMessages returns a chat's messages in order.
func (s *Store) ListChatMessages(ctx context.Context, chatID string) ([]*ChatMessage, error) {
	rows, err := s.query(ctx, `SELECT `+chatMessageCols+` FROM chat_messages WHERE chat_id = ? ORDER BY seq`, chatID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*ChatMessage
	for rows.Next() {
		m, err := scanChatMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpdateChatMessageIf writes a message's turn fields only while its
// status is one of from (compare-and-set). It reports whether it wrote.
func (s *Store) UpdateChatMessageIf(ctx context.Context, m *ChatMessage, from ...string) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("store: UpdateChatMessageIf needs at least one status")
	}
	m.Text = capText(m.Text)
	args := []any{m.Text, m.Status, m.Error, m.InputTokens, m.OutputTokens, m.CacheRead, m.CacheWrite, m.CostMicro, m.LogOffset, tsPtr(m.ResetAt), tsPtr(m.FinishedAt), m.ID}
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.exec(ctx, `UPDATE chat_messages SET text = ?, status = ?, error = ?, input_tokens = ?, output_tokens = ?, cache_read = ?, cache_write = ?,
		cost_micro = ?, log_offset = ?, reset_at = ?, finished_at = ? WHERE id = ? AND status IN (`+placeholders(len(from))+`)`, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InterruptChatTurns marks every queued or running turn interrupted (a
// daemon restart lost its exec) and returns how many it marked.
func (s *Store) InterruptChatTurns(ctx context.Context, reason string) (int, error) {
	res, err := s.exec(ctx, `UPDATE chat_messages SET status = ?, error = ?, finished_at = ? WHERE status IN (?, ?)`,
		TurnInterrupted, reason, ts(Now()), TurnQueued, TurnRunning)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func capText(s string) string {
	if len(s) <= MaxChatText {
		return s
	}
	n := MaxChatText
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// PutChatDraft replaces a chat's draft and returns it with its new
// version.
func (s *Store) PutChatDraft(ctx context.Context, chatID, yaml, issues, source string) (*ChatDraft, error) {
	if issues == "" {
		issues = "[]"
	}
	d := &ChatDraft{ChatID: chatID, YAML: yaml, Issues: issues, Source: source, UpdatedAt: Now()}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var v int
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT version FROM chat_drafts WHERE chat_id = ?`), chatID).Scan(&v)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			d.Version = 1
			_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO chat_drafts (chat_id, yaml, issues, source, version, updated_at) VALUES (?,?,?,?,?,?)`),
				chatID, yaml, issues, source, d.Version, ts(d.UpdatedAt))
			return err
		case err != nil:
			return err
		}
		d.Version = v + 1
		_, err = tx.ExecContext(ctx, s.rebind(`UPDATE chat_drafts SET yaml = ?, issues = ?, source = ?, version = ?, updated_at = ? WHERE chat_id = ?`),
			yaml, issues, source, d.Version, ts(d.UpdatedAt), chatID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// GetChatDraft returns a chat's draft.
func (s *Store) GetChatDraft(ctx context.Context, chatID string) (*ChatDraft, error) {
	var d ChatDraft
	var updated string
	err := s.queryRow(ctx, `SELECT chat_id, yaml, issues, source, version, updated_at FROM chat_drafts WHERE chat_id = ?`, chatID).
		Scan(&d.ChatID, &d.YAML, &d.Issues, &d.Source, &d.Version, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.UpdatedAt = parseTS(updated)
	return &d, nil
}

const chatApprovalCols = `id, chat_id, tool, request, decision, actor, result, created_at, expires_at, decided_at`

func scanChatApproval(sc scanner) (*ChatApproval, error) {
	var a ChatApproval
	var created, expires string
	var decided sql.NullString
	err := sc.Scan(&a.ID, &a.ChatID, &a.Tool, &a.Request, &a.Decision, &a.Actor, &a.Result, &created, &expires, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt, a.ExpiresAt, a.DecidedAt = parseTS(created), parseTS(expires), parseTSPtr(decided)
	return &a, nil
}

// CreateChatApproval records a gated call; ID and CreatedAt are filled in.
func (s *Store) CreateChatApproval(ctx context.Context, a *ChatApproval) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	a.CreatedAt = Now()
	_, err := s.exec(ctx, `INSERT INTO chat_approvals (`+chatApprovalCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.ChatID, a.Tool, a.Request, a.Decision, a.Actor, a.Result, ts(a.CreatedAt), ts(a.ExpiresAt), tsPtr(a.DecidedAt))
	return err
}

// GetChatApproval returns a gated call.
func (s *Store) GetChatApproval(ctx context.Context, id string) (*ChatApproval, error) {
	return scanChatApproval(s.queryRow(ctx, `SELECT `+chatApprovalCols+` FROM chat_approvals WHERE id = ?`, id))
}

// ListChatApprovals returns a chat's gated calls, oldest first; pending
// only when pendingOnly.
func (s *Store) ListChatApprovals(ctx context.Context, chatID string, pendingOnly bool) ([]*ChatApproval, error) {
	q := `SELECT ` + chatApprovalCols + ` FROM chat_approvals WHERE chat_id = ?`
	if pendingOnly {
		q += ` AND decision = ''`
	}
	rows, err := s.query(ctx, q+` ORDER BY created_at, id`, chatID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*ChatApproval
	for rows.Next() {
		a, err := scanChatApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideChatApproval records the decision on a pending call. It returns
// ErrNotFound for an unknown id and ErrConflict when already decided.
func (s *Store) DecideChatApproval(ctx context.Context, id, decision, actor, result string) (*ChatApproval, error) {
	res, err := s.exec(ctx, `UPDATE chat_approvals SET decision = ?, actor = ?, result = ?, decided_at = ? WHERE id = ? AND decision = ''`,
		decision, actor, result, ts(Now()), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetChatApproval(ctx, id); err != nil {
			return nil, err
		}
		return nil, ErrConflict
	}
	return s.GetChatApproval(ctx, id)
}

// ExpireChatApprovals marks every pending gated call expired (a restart
// ended the turns waiting on them) and returns how many it marked.
func (s *Store) ExpireChatApprovals(ctx context.Context) (int, error) {
	res, err := s.exec(ctx, `UPDATE chat_approvals SET decision = 'expired', decided_at = ? WHERE decision = ''`, ts(Now()))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetChatApprovalResult records a decided call's result (the run id).
func (s *Store) SetChatApprovalResult(ctx context.Context, id, result string) error {
	res, err := s.exec(ctx, `UPDATE chat_approvals SET result = ? WHERE id = ?`, result, id)
	return wrote(res, err)
}
