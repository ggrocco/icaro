package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Delivery statuses.
const (
	DeliveryRejected  = "rejected"  // verification failed
	DeliveryDuplicate = "duplicate" // a verified delivery with the same key was already processed
	DeliveryNoMatch   = "no_match"  // verified, but no event or workflow matched
	DeliveryProcessed = "processed" // matched; see results for runs, filters and skips
	DeliveryChallenge = "challenge" // a provider handshake was answered
	DeliveryError     = "error"     // processing failed; the provider may retry
)

// Delivery is one inbound webhook request and what became of it.
type Delivery struct {
	ID          string
	Integration string
	DeliveryKey string
	Workflow    string // set for url-token deliveries
	Method      string
	HeadersJSON string
	Query       string
	Body        []byte
	// BodySize and BodySHA256 describe the request body as received; Body
	// may be only a prefix of it (rejected deliveries).
	BodySize   int64
	BodySHA256 string
	// SignedSHA256 is the SHA-256 of the HMAC-signed material, empty when
	// nothing was signed (shared secret, URL token, rejected).
	SignedSHA256 string
	Verified     bool
	VerifyError  string
	Status       string
	Error        string
	EventsJSON   string
	ResultsJSON  string
	RedeliveryOf string
	ReceivedAt   time.Time
}

const deliveryCols = `id, integration, delivery_key, workflow, method, headers_json, query, body, body_size, body_sha256, signed_sha256, verified, verify_error, status, error, events_json, results_json, redelivery_of, received_at`

func scanDelivery(sc interface{ Scan(...any) error }) (*Delivery, error) {
	var d Delivery
	var verified int
	var redelivery sql.NullString
	var received string
	err := sc.Scan(&d.ID, &d.Integration, &d.DeliveryKey, &d.Workflow, &d.Method, &d.HeadersJSON, &d.Query, &d.Body,
		&d.BodySize, &d.BodySHA256, &d.SignedSHA256, &verified,
		&d.VerifyError, &d.Status, &d.Error, &d.EventsJSON, &d.ResultsJSON, &redelivery, &received)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Verified, d.RedeliveryOf, d.ReceivedAt = verified == 1, redelivery.String, parseTS(received)
	return &d, nil
}

// CreateDelivery inserts a delivery record; ID, ReceivedAt and the body
// size and hash (of Body) are filled in when empty.
func (s *Store) CreateDelivery(ctx context.Context, d *Delivery) error {
	if d.ID == "" {
		d.ID = NewID()
	}
	if d.ReceivedAt.IsZero() {
		d.ReceivedAt = Now()
	}
	if d.EventsJSON == "" {
		d.EventsJSON = "[]"
	}
	if d.ResultsJSON == "" {
		d.ResultsJSON = "[]"
	}
	if d.Body == nil {
		d.Body = []byte{}
	}
	if d.BodySHA256 == "" {
		sum := sha256.Sum256(d.Body)
		d.BodySize, d.BodySHA256 = int64(len(d.Body)), hex.EncodeToString(sum[:])
	}
	verified := 0
	if d.Verified {
		verified = 1
	}
	_, err := s.exec(ctx, `INSERT INTO webhook_deliveries (`+deliveryCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.Integration, d.DeliveryKey, d.Workflow, d.Method, d.HeadersJSON, d.Query, d.Body, d.BodySize, d.BodySHA256, d.SignedSHA256, verified, d.VerifyError,
		d.Status, d.Error, d.EventsJSON, d.ResultsJSON, nullStr(d.RedeliveryOf), ts(d.ReceivedAt))
	return err
}

// FinishDelivery records the outcome of processing.
func (s *Store) FinishDelivery(ctx context.Context, id, status, errMsg, eventsJSON, resultsJSON string) error {
	res, err := s.exec(ctx, `UPDATE webhook_deliveries SET status = ?, error = ?, events_json = ?, results_json = ? WHERE id = ?`,
		status, errMsg, eventsJSON, resultsJSON, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// GetDelivery fetches one delivery.
func (s *Store) GetDelivery(ctx context.Context, id string) (*Delivery, error) {
	return scanDelivery(s.queryRow(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries WHERE id = ?`, id))
}

// FindProcessedDelivery returns an earlier verified delivery that was
// processed and has the same key or, when signed is set, the same signed
// material (the provider-retry and replay dedup point).
func (s *Store) FindProcessedDelivery(ctx context.Context, integration, key, signed, workflow, exceptID string) (*Delivery, error) {
	return scanDelivery(s.queryRow(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
		WHERE integration = ? AND (delivery_key = ? OR (? <> '' AND signed_sha256 = ?)) AND workflow = ? AND verified = 1
		AND id <> ? AND redelivery_of IS NULL AND status IN (?, ?) ORDER BY received_at LIMIT 1`,
		integration, key, signed, signed, workflow, exceptID, DeliveryProcessed, DeliveryNoMatch))
}

// PruneDeliveries deletes deliveries received before olderThan and all
// but the newest maxRejected rejected (unverified) ones; maxRejected <= 0
// keeps them all. Verified deliveries are only aged out, so a flood of
// rejected requests cannot evict the records that dedupe replays.
func (s *Store) PruneDeliveries(ctx context.Context, olderThan time.Time, maxRejected int) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM webhook_deliveries WHERE received_at < ?`, ts(olderThan))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if maxRejected <= 0 {
		return n, nil
	}
	var cutoff string
	err = s.queryRow(ctx, `SELECT received_at FROM webhook_deliveries WHERE verified = 0
		ORDER BY received_at DESC LIMIT 1 OFFSET ?`, maxRejected-1).Scan(&cutoff)
	if errors.Is(err, sql.ErrNoRows) {
		return n, nil
	}
	if err != nil {
		return n, err
	}
	res, err = s.exec(ctx, `DELETE FROM webhook_deliveries WHERE verified = 0 AND received_at < ?`, cutoff)
	if err != nil {
		return n, err
	}
	m, _ := res.RowsAffected()
	return n + m, nil
}

// DeliveryFilter narrows ListDeliveries.
type DeliveryFilter struct {
	Integration string
	Status      string
	Limit       int
}

// ListDeliveries returns deliveries newest first (bodies included).
func (s *Store) ListDeliveries(ctx context.Context, f DeliveryFilter) ([]*Delivery, error) {
	var where []string
	var args []any
	if f.Integration != "" {
		where = append(where, "integration = ?")
		args = append(args, f.Integration)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	q := `SELECT ` + deliveryCols + ` FROM webhook_deliveries`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	q += fmt.Sprintf(" ORDER BY received_at DESC, id DESC LIMIT %d", limit)
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// WebhookToken is a generic webhook's per-workflow URL token.
type WebhookToken struct {
	Workflow  string
	Token     string
	CreatedAt time.Time
}

// EnsureWebhookToken returns the workflow's token, creating it with
// newToken when absent.
func (s *Store) EnsureWebhookToken(ctx context.Context, workflow string, newToken func() string) (*WebhookToken, error) {
	if t, err := s.getWebhookToken(ctx, `workflow = ?`, workflow); err == nil {
		return t, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	t := &WebhookToken{Workflow: workflow, Token: newToken(), CreatedAt: Now()}
	_, err := s.exec(ctx, `INSERT INTO webhook_tokens (workflow, token, created_at) VALUES (?,?,?)`, t.Workflow, t.Token, ts(t.CreatedAt))
	if isUniqueViolation(err) {
		return s.getWebhookToken(ctx, `workflow = ?`, workflow)
	}
	return t, err
}

// WebhookTokenByToken resolves a URL token.
func (s *Store) WebhookTokenByToken(ctx context.Context, token string) (*WebhookToken, error) {
	return s.getWebhookToken(ctx, `token = ?`, token)
}

func (s *Store) getWebhookToken(ctx context.Context, where string, arg any) (*WebhookToken, error) {
	var t WebhookToken
	var created string
	err := s.queryRow(ctx, `SELECT workflow, token, created_at FROM webhook_tokens WHERE `+where, arg).Scan(&t.Workflow, &t.Token, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = parseTS(created)
	return &t, nil
}

// ListWebhookTokens returns every token ordered by workflow.
func (s *Store) ListWebhookTokens(ctx context.Context) ([]*WebhookToken, error) {
	rows, err := s.query(ctx, `SELECT workflow, token, created_at FROM webhook_tokens ORDER BY workflow`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*WebhookToken
	for rows.Next() {
		var t WebhookToken
		var created string
		if err := rows.Scan(&t.Workflow, &t.Token, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTS(created)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// RotateWebhookToken replaces a workflow's token.
func (s *Store) RotateWebhookToken(ctx context.Context, workflow, token string) error {
	res, err := s.exec(ctx, `UPDATE webhook_tokens SET token = ?, created_at = ? WHERE workflow = ?`, token, ts(Now()), workflow)
	if err != nil {
		return err
	}
	return affected(res)
}

// DeleteWebhookToken removes a token (workflow no longer subscribes).
func (s *Store) DeleteWebhookToken(ctx context.Context, workflow string) error {
	_, err := s.exec(ctx, `DELETE FROM webhook_tokens WHERE workflow = ?`, workflow)
	return err
}

// CronState is the persisted schedule of one cron trigger.
type CronState struct {
	Workflow    string
	Idx         int
	Expr        string
	NextAt      time.Time
	LastFiredAt *time.Time
}

// ListCronStates returns every cron state.
func (s *Store) ListCronStates(ctx context.Context) ([]*CronState, error) {
	rows, err := s.query(ctx, `SELECT workflow, idx, expr, next_at, last_fired_at FROM cron_state ORDER BY workflow, idx`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*CronState
	for rows.Next() {
		var c CronState
		var next string
		var last sql.NullString
		if err := rows.Scan(&c.Workflow, &c.Idx, &c.Expr, &next, &last); err != nil {
			return nil, err
		}
		c.NextAt, c.LastFiredAt = parseTS(next), parseTSPtr(last)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// PutCronState inserts or replaces a cron state.
func (s *Store) PutCronState(ctx context.Context, c *CronState) error {
	_, err := s.exec(ctx, `INSERT INTO cron_state (workflow, idx, expr, next_at, last_fired_at) VALUES (?,?,?,?,?)
		ON CONFLICT (workflow, idx) DO UPDATE SET expr = excluded.expr, next_at = excluded.next_at, last_fired_at = excluded.last_fired_at`,
		c.Workflow, c.Idx, c.Expr, ts(c.NextAt), tsPtr(c.LastFiredAt))
	return err
}

// DeleteCronState removes a trigger's state.
func (s *Store) DeleteCronState(ctx context.Context, workflow string, idx int) error {
	_, err := s.exec(ctx, `DELETE FROM cron_state WHERE workflow = ? AND idx = ?`, workflow, idx)
	return err
}
