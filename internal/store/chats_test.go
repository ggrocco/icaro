package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestChats(t *testing.T) {
	forEachDriver(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		c := &Chat{Title: "bug to slack", Profile: "claude", Agent: "claude", CreatedBy: "local"}
		if err := s.CreateChat(ctx, c); err != nil {
			t.Fatal(err)
		}
		other := &Chat{Profile: "codex", Agent: "codex", ProjectID: "p1"}
		if err := s.CreateChat(ctx, other); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetChat(ctx, c.ID)
		if err != nil || got.Title != "bug to slack" || got.Agent != "claude" || got.CreatedAt.IsZero() {
			t.Fatalf("%+v %v", got, err)
		}
		if _, err := s.GetChat(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing chat: %v", err)
		}

		got.Workflow, got.SessionID, got.ConversationID, got.Title = "bug-to-slack", "s1", "conv-1", "renamed"
		if err := s.UpdateChat(ctx, got); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateChat(ctx, &Chat{ID: "nope"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("update missing: %v", err)
		}
		again, _ := s.GetChat(ctx, c.ID)
		if again.Workflow != "bug-to-slack" || again.SessionID != "s1" || again.ConversationID != "conv-1" || again.Title != "renamed" {
			t.Fatalf("%+v", again)
		}

		byS, err := s.GetChatBySession(ctx, "s1")
		if err != nil || byS.ID != c.ID {
			t.Fatalf("by session %+v %v", byS, err)
		}
		if _, err := s.GetChatBySession(ctx, ""); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if err := s.SetChatField(ctx, c.ID, ChatConversationID, "conv-2"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetChatField(ctx, c.ID, "profile", "x"); err == nil {
			t.Fatal("profile is not writable")
		}
		if err := s.SetChatField(ctx, "nope", ChatTitle, "x"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if got, _ := s.GetChat(ctx, c.ID); got.ConversationID != "conv-2" || got.Title != "renamed" {
			t.Fatalf("%+v", got)
		}

		// Messages: seq is assigned in order, and adding one bumps the chat.
		time.Sleep(2 * time.Millisecond)
		u := &ChatMessage{ChatID: other.ID, Role: RoleUser, Text: "hello", Status: TurnDone}
		if err := s.AddChatMessage(ctx, u); err != nil {
			t.Fatal(err)
		}
		a := &ChatMessage{ChatID: other.ID, Role: RoleAssistant, Status: TurnQueued, LogOffset: 10}
		if err := s.AddChatMessage(ctx, a); err != nil {
			t.Fatal(err)
		}
		if u.Seq != 1 || a.Seq != 2 {
			t.Fatalf("seq %d %d", u.Seq, a.Seq)
		}
		if err := s.AddChatMessage(ctx, &ChatMessage{ChatID: "nope", Role: RoleUser, Status: TurnDone}); err == nil {
			t.Fatal("message for a missing chat")
		}
		list, err := s.ListChats(ctx, 0)
		if err != nil || len(list) != 2 || list[0].ID != other.ID {
			t.Fatalf("chats %v %v", list, err)
		}
		if one, _ := s.ListChats(ctx, 1); len(one) != 1 {
			t.Fatalf("limit: %d", len(one))
		}

		// Compare-and-set turn updates.
		a.Status, a.Text = TurnRunning, strings.Repeat("é", MaxChatText)
		if ok, err := s.UpdateChatMessageIf(ctx, a, TurnQueued); err != nil || !ok {
			t.Fatalf("queued -> running: %v %v", ok, err)
		}
		if ok, _ := s.UpdateChatMessageIf(ctx, a, TurnQueued); ok {
			t.Fatal("a second claim won")
		}
		if _, err := s.UpdateChatMessageIf(ctx, a); err == nil {
			t.Fatal("no from statuses")
		}
		reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		fin := time.Now()
		a.Status, a.Text, a.InputTokens, a.OutputTokens, a.CacheRead, a.CacheWrite, a.CostMicro = TurnRateLimited, "limit", 5, 6, 7, 8, 9
		a.ResetAt, a.FinishedAt = &reset, &fin
		if ok, err := s.UpdateChatMessageIf(ctx, a, TurnRunning); err != nil || !ok {
			t.Fatal(ok, err)
		}
		m, err := s.GetChatMessage(ctx, a.ID)
		if err != nil || m.Status != TurnRateLimited || m.Text != "limit" || m.InputTokens != 5 || m.CostMicro != 9 || m.LogOffset != 10 ||
			m.ResetAt == nil || !m.ResetAt.Equal(reset) || m.FinishedAt == nil {
			t.Fatalf("%+v %v", m, err)
		}
		if _, err := s.GetChatMessage(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}

		// Text over the cap is cut on a rune boundary.
		long := &ChatMessage{ChatID: c.ID, Role: RoleAssistant, Status: TurnRunning, Text: "a" + strings.Repeat("é", MaxChatText)}
		if err := s.AddChatMessage(ctx, long); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetChatMessage(ctx, long.ID); len(got.Text) > MaxChatText || !strings.HasPrefix(got.Text, "aé") || strings.HasSuffix(got.Text, "\xc3") {
			t.Fatalf("capped text: %d bytes", len(got.Text))
		}

		// A restart interrupts turns in flight only.
		n, err := s.InterruptChatTurns(ctx, "the daemon restarted")
		if err != nil || n != 1 {
			t.Fatalf("interrupted %d %v", n, err)
		}
		if got, _ := s.GetChatMessage(ctx, long.ID); got.Status != TurnInterrupted || got.Error != "the daemon restarted" || got.FinishedAt == nil {
			t.Fatalf("%+v", got)
		}
		msgs, err := s.ListChatMessages(ctx, other.ID)
		if err != nil || len(msgs) != 2 || msgs[0].Role != RoleUser || msgs[1].Status != TurnRateLimited {
			t.Fatalf("%v %v", msgs, err)
		}

		// Drafts: one per chat, versioned.
		if _, err := s.GetChatDraft(ctx, c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		d, err := s.PutChatDraft(ctx, c.ID, "name: a\n", "", "user")
		if err != nil || d.Version != 1 || d.Issues != "[]" {
			t.Fatalf("%+v %v", d, err)
		}
		d, err = s.PutChatDraft(ctx, c.ID, "name: b\n", `[{"path":"/steps"}]`, "assistant")
		if err != nil || d.Version != 2 {
			t.Fatalf("%+v %v", d, err)
		}
		if got, _ := s.GetChatDraft(ctx, c.ID); got.YAML != "name: b\n" || got.Source != "assistant" || got.Version != 2 || !strings.Contains(got.Issues, "/steps") {
			t.Fatalf("%+v", got)
		}
		if _, err := s.PutChatDraft(ctx, "nope", "x", "", "user"); err == nil {
			t.Fatal("draft for a missing chat")
		}

		// Approvals: decided once.
		ap := &ChatApproval{ChatID: c.ID, Tool: "run_workflow", Request: `{"name":"a"}`, ExpiresAt: time.Now().Add(time.Minute)}
		if err := s.CreateChatApproval(ctx, ap); err != nil {
			t.Fatal(err)
		}
		if p, err := s.ListChatApprovals(ctx, c.ID, true); err != nil || len(p) != 1 {
			t.Fatalf("pending %v %v", p, err)
		}
		dec, err := s.DecideChatApproval(ctx, ap.ID, "approved", "local", "")
		if err != nil || dec.Decision != "approved" || dec.DecidedAt == nil || dec.Actor != "local" {
			t.Fatalf("%+v %v", dec, err)
		}
		if _, err := s.DecideChatApproval(ctx, ap.ID, "declined", "x", ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("second decision: %v", err)
		}
		if _, err := s.DecideChatApproval(ctx, "nope", "declined", "x", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing approval: %v", err)
		}
		stale := &ChatApproval{ChatID: c.ID, Tool: "run_workflow", Request: "{}", ExpiresAt: time.Now()}
		if err := s.CreateChatApproval(ctx, stale); err != nil {
			t.Fatal(err)
		}
		if n, err := s.ExpireChatApprovals(ctx); err != nil || n != 1 {
			t.Fatalf("expired %d %v", n, err)
		}
		if got, _ := s.GetChatApproval(ctx, stale.ID); got.Decision != "expired" || got.DecidedAt == nil {
			t.Fatalf("%+v", got)
		}
		if err := s.SetChatApprovalResult(ctx, ap.ID, "run-1"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetChatApprovalResult(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if p, _ := s.ListChatApprovals(ctx, c.ID, true); len(p) != 0 {
			t.Fatalf("still pending: %v", p)
		}
		if all, _ := s.ListChatApprovals(ctx, c.ID, false); len(all) != 2 || all[0].Result != "run-1" {
			t.Fatalf("all: %+v", all)
		}

		// Delete removes the children too.
		if err := s.DeleteChat(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteChat(ctx, c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := s.GetChatDraft(ctx, c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
		if all, _ := s.ListChatApprovals(ctx, c.ID, false); len(all) != 0 {
			t.Fatal(all)
		}
		if _, err := s.GetChatMessage(ctx, long.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	})
}

func TestChatsClosedStore(t *testing.T) {
	s := sqliteStore(t)
	_ = s.Close()
	ctx := context.Background()
	if _, err := s.ListChats(ctx, 1); err == nil {
		t.Error("list")
	}
	if _, err := s.ListChatMessages(ctx, "c"); err == nil {
		t.Error("messages")
	}
	if _, err := s.ListChatApprovals(ctx, "c", false); err == nil {
		t.Error("approvals")
	}
	if _, err := s.InterruptChatTurns(ctx, "x"); err == nil {
		t.Error("interrupt")
	}
	if _, err := s.UpdateChatMessageIf(ctx, &ChatMessage{}, TurnRunning); err == nil {
		t.Error("update")
	}
	if _, err := s.GetChatDraft(ctx, "c"); err == nil || errors.Is(err, ErrNotFound) {
		t.Error("draft")
	}
	if _, err := s.PutChatDraft(ctx, "c", "", "", "user"); err == nil {
		t.Error("put draft")
	}
	if _, err := s.DecideChatApproval(ctx, "a", "approved", "", ""); err == nil {
		t.Error("decide")
	}
	if err := s.DeleteChat(ctx, "c"); err == nil {
		t.Error("delete")
	}
	if _, err := s.ExpireChatApprovals(ctx); err == nil {
		t.Error("expire")
	}
}
