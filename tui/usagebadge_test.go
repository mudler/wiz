package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mudler/nib/chat"
	"github.com/mudler/nib/types"
	"github.com/mudler/xlog"
)

// Nothing spent yet means nothing to show, matching how contextBadge hides
// itself before the first turn.
func TestUsageBadgeHiddenWhenZero(t *testing.T) {
	m := Model{}
	if got := m.usageBadge(); got != "" {
		t.Fatalf("usageBadge on a fresh session = %q, want empty", got)
	}
}

// Plain words, matching contextBadge's "ctx 8k (6%)" phrasing and the calm
// no-emoji voice TestNoEmojiInRenderHelpers guards.
func TestUsageBadgeFormatsBothDirections(t *testing.T) {
	m := Model{sessionUsage: chat.SessionUsage{PromptTokens: 312000, CompletionTokens: 18400}}
	got := m.usageBadge()
	if !strings.Contains(got, "312k") || !strings.Contains(got, "18.4k") {
		t.Fatalf("usageBadge = %q, want both directions via HumanTokens", got)
	}
	if containsEmoji(got) {
		t.Fatalf("usageBadge emitted emoji: %q", got)
	}
}

// The badge names both directions, so both slots must carry a number. A
// provider that reports only one of them used to render "session 1.2k in /  out"
// — the same hole chat.HumanTokensOrZero was written to close for the exit
// summary, which the badge re-opened by formatting with chat.HumanTokens.
func TestUsageBadgeFillsAZeroDirection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage chat.SessionUsage
	}{
		{"no completion tokens", chat.SessionUsage{PromptTokens: 1200}},
		{"no prompt tokens", chat.SessionUsage{CompletionTokens: 8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Model{sessionUsage: tc.usage}.usageBadge()
			if got == "" {
				t.Fatal("a session with spend on one side renders no badge at all")
			}
			if !strings.Contains(got, "0") {
				t.Fatalf("the zero direction dropped its number: %q", got)
			}
			if strings.Contains(got, "  ") {
				t.Fatalf("badge has a hole where a count should be: %q", got)
			}
		})
	}
}

// The context badge predicts compaction and is therefore actionable; session
// totals are informational and are dropped WHOLE rather than truncated. ttyd in
// a browser is the reporter's normal case, so this is the common path, not an
// edge case.
func TestNarrowFooterDropsUsageAndKeepsContext(t *testing.T) {
	m := Model{
		width:         30,
		contextTokens: 47200,
		sessionUsage:  chat.SessionUsage{PromptTokens: 312000, CompletionTokens: 18400},
	}
	m.cfg.Compaction.MaxContextTokens = 128000

	got := m.footerBadges(20)
	if strings.Contains(got, "312k") {
		t.Fatalf("usage survived a narrow footer: %q", got)
	}
	if !strings.Contains(got, "ctx") {
		t.Fatalf("the context badge was dropped instead of the usage badge: %q", got)
	}
}

// With room for both, both render.
func TestWideFooterShowsBothBadges(t *testing.T) {
	m := Model{
		width:         120,
		contextTokens: 47200,
		sessionUsage:  chat.SessionUsage{PromptTokens: 312000, CompletionTokens: 18400},
	}
	m.cfg.Compaction.MaxContextTokens = 128000

	got := m.footerBadges(20)
	if !strings.Contains(got, "ctx") || !strings.Contains(got, "312k") {
		t.Fatalf("a wide footer should carry both badges: %q", got)
	}
}

// The badge predicts auto-compaction, so it has to be drawn against the same
// number the trigger uses: the window less the reserve. Against the raw window
// it read 78% at the moment compaction fired, which is the one reading it must
// never give.
func TestContextBadgeSubtractsTheReserve(t *testing.T) {
	m := Model{width: 120, contextTokens: 100000}
	m.cfg.Compaction = types.CompactionConfig{
		MaxContextTokens: 128000, Threshold: 0.8, ReserveTokens: 4096,
	}

	if got := m.contextBudget(); got != 123904 {
		t.Fatalf("contextBudget = %d, want 128000-4096 = 123904", got)
	}
	got := m.contextBadge()
	if !strings.Contains(got, "(80%)") {
		t.Fatalf("contextBadge = %q, want 80%% of the 123904-token budget", got)
	}
	if strings.Contains(got, "(78%)") {
		t.Fatalf("contextBadge = %q; still budgeting against the raw window", got)
	}
}

// With compaction disabled there is no moment for the badge to predict, so it
// must not highlight — the highlight is a warning about something that is about
// to happen, and with Disabled set nothing is. The percentage still shows: it is
// real headroom against a real budget.
func TestContextBadgeDoesNotWarnWhenCompactionIsDisabled(t *testing.T) {
	// Identical to TestContextBadgeSubtractsTheReserve's numbers — 80% of the
	// budget, which is exactly the trigger — so the only difference is Disabled.
	base := types.CompactionConfig{
		MaxContextTokens: 128000, Threshold: 0.8, ReserveTokens: 4096,
	}

	on := Model{width: 120, contextTokens: 100000}
	on.cfg.Compaction = base
	if !on.contextBadgeWarns(100000, on.contextBudget()) {
		t.Fatal("the badge does not warn with compaction ON at the trigger; this test is not comparing against a warning at all")
	}

	base.Disabled = true
	off := Model{width: 120, contextTokens: 100000}
	off.cfg.Compaction = base
	if off.contextBadgeWarns(100000, off.contextBudget()) {
		t.Fatal("the badge warns at 80% while compaction is disabled, predicting a compaction that cannot fire")
	}
	if got := off.contextBadge(); !strings.Contains(got, "(80%)") {
		t.Fatalf("contextBadge = %q, want the percentage still shown", got)
	}
}

// overflowingOpenAI always answers with the llama.cpp-shaped 400 that states the
// model's real window, so a session driven through it learns 262144 whatever it
// was configured with.
func overflowingOpenAI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "request (368203 tokens) exceeds the available context size (262144 tokens)",
				"type":    "invalid_request_error",
			},
		})
	}
}

// The badge follows the window the session is really budgeting against, not the
// configured guess.
//
// This is the case that made the old badge actively misleading rather than
// merely imprecise: a config of 400k against a backend that serves 262k drew a
// calm 52% while compaction was already firing. A real session is the only
// honest way to reach that state from out here — the learned window is set from
// inside a failed turn, and nothing in chat exports a setter for it.
func TestContextBadgeFollowsTheLearnedWindow(t *testing.T) {
	xlog.SetLogger(xlog.NewLogger(xlog.LogLevel("error"), ""))

	srv := httptest.NewServer(overflowingOpenAI())
	t.Cleanup(srv.Close)

	compaction := types.CompactionConfig{
		// 400000 configured against a backend that states 262144: the two must
		// differ, or "follows the learned window" would pass on the fallback.
		MaxContextTokens: 400000, Threshold: 0.8, KeepRecent: 2, ReserveTokens: 4096,
	}
	s, err := chat.NewSession(context.Background(), types.Config{
		Model:        "fake-model",
		APIKey:       "fake-key",
		BaseURL:      srv.URL + "/v1",
		LogLevel:     "error",
		ApprovalMode: "auto",
		AgentOptions: types.AgentOptions{Iterations: 10, MaxAttempts: 3, MaxRetries: 1},
		Compaction:   compaction,
	}, chat.Callbacks{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The turn cannot succeed against a backend that only overflows; the point
	// is that the window it stated was kept.
	if _, err := s.SendMessage("hello"); err == nil {
		t.Fatal("the turn succeeded against an always-overflowing backend; the fixture proves nothing")
	}
	if got := s.ContextWindow(); got != 262144 {
		t.Fatalf("session window = %d, want the learned 262144: nothing was learned, so the badge cannot be tested", got)
	}

	m := Model{width: 120, contextTokens: 209000, session: s}
	m.cfg.Compaction = compaction

	got := m.contextBadge()
	if !strings.Contains(got, "(80%)") {
		t.Fatalf("contextBadge = %q, want 80%% of the learned window's budget (262144-4096)", got)
	}
	if strings.Contains(got, "(52%)") {
		t.Fatalf("contextBadge = %q; still budgeting against the configured 400000 while compaction fires", got)
	}
}

// billingOpenAI is a minimal non-streaming OpenAI-compatible endpoint that
// replies with a plain stop message AND reports token usage. The usage block is
// the whole point: a handler that omitted it would let the refresh test pass
// with the refresh line deleted.
func billingOpenAI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "fake", "object": "chat.completion", "model": "fake",
			"choices": []any{map[string]any{
				"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": 120, "completion_tokens": 8, "total_tokens": 128,
			},
		})
	}
}

// newSpentSession returns a real session that has completed a turn against a
// fake backend, so its usage counter is genuinely non-zero.
//
// A real session rather than a hand-set field on purpose: chat's addUsage is
// unexported, so from package tui the only honest way to get a session with
// spend on it is to drive one through SendMessage.
func newSpentSession(t *testing.T) *chat.Session {
	t.Helper()
	xlog.SetLogger(xlog.NewLogger(xlog.LogLevel("error"), ""))

	srv := httptest.NewServer(billingOpenAI())
	t.Cleanup(srv.Close)

	s, err := chat.NewSession(context.Background(), types.Config{
		Model:        "fake-model",
		APIKey:       "fake-key",
		BaseURL:      srv.URL + "/v1",
		LogLevel:     "error",
		ApprovalMode: "auto",
		AgentOptions: types.AgentOptions{Iterations: 10, MaxAttempts: 3, MaxRetries: 3},
	}, chat.Callbacks{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.SendMessage("hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if s.Usage().TotalTokens == 0 {
		t.Fatal("the fake backend reported no usage; the fixture cannot prove anything")
	}
	return s
}

// The wiring, not the arithmetic: the badge renderers above all build
// Model{sessionUsage: ...} literals, so they would keep passing if every
// m.sessionUsage = m.session.Usage() refresh were deleted and the badge went
// permanently blank in the real TUI. This drives a refresh site through Update
// and asserts on the RETURNED model, which is what actually reaches View.
func TestResponseMsgRefreshesSessionUsage(t *testing.T) {
	m := newQueueTestModel()
	m.session = newSpentSession(t)
	m.loading = true

	next, _ := m.Update(responseMsg{content: "done"})
	nm := next.(Model)

	if nm.sessionUsage.TotalTokens == 0 {
		t.Fatal("sessionUsage is still zero after a finished run: the footer refresh is missing")
	}
	if nm.sessionUsage != m.session.Usage() {
		t.Fatalf("sessionUsage = %+v, want the session's own snapshot %+v",
			nm.sessionUsage, m.session.Usage())
	}
	if nm.usageBadge() == "" {
		t.Fatal("the badge is blank on a model that has spent tokens")
	}
}

// Three surfaces report one run: the footer, the exit summary and usage.json.
// quit() closes the session, which writes usage.json from the LIVE counter,
// while RunTUI prints the summary from the model's cached field — so a quit
// that does not refresh that field lets them disagree. The reported case is a
// second Ctrl+C during the first turn: usage.json holds the real spend and the
// terminal prints nothing, because the summary is "" for zero.
//
// Deliberately never assigning sessionUsage here: this model has spent tokens
// and has a stale (zero) cache, exactly like the interrupted first turn.
func TestQuitRefreshesUsageBeforeClosingTheSession(t *testing.T) {
	m := newQueueTestModel()
	m.session = newSpentSession(t)
	m.cancel = func() {}

	next, _ := m.quit()
	nm := next.(Model)

	if nm.SessionUsage() != m.session.Usage() {
		t.Fatalf("SessionUsage() = %+v, want the session's own snapshot %+v — "+
			"the summary and usage.json would disagree about the same run",
			nm.SessionUsage(), m.session.Usage())
	}
	if chat.FormatSessionSummary(nm.SessionUsage()) == "" {
		t.Fatal("the exit summary is empty for a session that spent tokens")
	}
}

// RunTUI reads the spend off the final model to print the exit summary, and it
// only has the exported accessor to read it with. An accessor that returned a
// zero value would silently turn the summary off.
func TestSessionUsageAccessorReportsWhatTheModelHolds(t *testing.T) {
	m := newQueueTestModel()
	m.session = newSpentSession(t)
	m.loading = true

	next, _ := m.Update(responseMsg{content: "done"})
	nm := next.(Model)
	if got := nm.SessionUsage(); got != nm.sessionUsage {
		t.Fatalf("SessionUsage() = %+v, want the model's own field %+v", got, nm.sessionUsage)
	}
	if nm.SessionUsage().TotalTokens == 0 {
		t.Fatal("SessionUsage() reports nothing spent, so the exit summary would never print")
	}
}
