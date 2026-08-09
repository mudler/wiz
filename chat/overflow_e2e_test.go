package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mudler/nib/types"
	"github.com/mudler/xlog"
)

// A backend that rejects the first request with a real llama.cpp-shaped
// overflow error and serves the second normally.
//
// Everything above this file stops at cogito's LLM interface; this drives a real
// NewSession over a real HTTP boundary, so the 400 has to survive go-openai's
// APIError decoding and cogito's error wrapping before nib's marker match ever
// sees it. That translation is exactly what a fake LLM cannot prove.
func TestOverflowRecoveryOverHTTP(t *testing.T) {
	xlog.SetLogger(xlog.NewLogger(xlog.LogLevel("error"), ""))

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "request (368203 tokens) exceeds the available context size (262144 tokens)",
					"type":    "invalid_request_error",
				},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-1", "object": "chat.completion", "created": 1, "model": "m",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "recovered"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11},
		})
	}))
	defer srv.Close()

	cfg := types.Config{
		Model:        "m",
		APIKey:       "k",
		BaseURL:      srv.URL + "/v1",
		LogLevel:     "error",
		ApprovalMode: "auto",
		// MaxRetries is 1, not the 3 the other e2e fixtures use. MaxRetries
		// drives cogito's per-decision retry loop, so at 3 the single 400 is
		// absorbed INSIDE one ExecuteTools run: the run returns success, nib's
		// recovery never fires, and this test would pass on cogito's retry
		// while proving nothing about the code it names.
		AgentOptions: types.AgentOptions{Iterations: 10, MaxAttempts: 3, MaxRetries: 1},
		Compaction: types.CompactionConfig{
			// Deliberately NOT the 262144 the backend states. contextWindow
			// falls back to MaxContextTokens, so equal numbers would make the
			// "learned the window" assertion below pass with nothing learned.
			MaxContextTokens: 400000, Threshold: 0.8, KeepRecent: 2, ReserveTokens: 4096,
		},
	}
	s, err := NewSession(context.Background(), cfg, Callbacks{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	// Seed enough history that compaction has a head to summarise.
	s.fragment = s.fragment.
		AddMessage("user", "u1").AddMessage("assistant", "a1").
		AddMessage("user", "u2").AddMessage("assistant", "a2")

	got, err := s.SendMessage("what changed?")
	if err != nil {
		t.Fatalf("the turn failed instead of recovering: %v", err)
	}
	if got == "" {
		t.Fatal("empty response after recovery")
	}
	// The turn succeeding is not enough on its own: a backend that simply
	// answered on a later call would look identical from out here. This is the
	// assertion that says the answer came from nib's recovery path.
	if n := s.overflowRetries(); n != 1 {
		t.Fatalf("overflow retries = %d, want exactly 1: the answer did not come from the recovery path", n)
	}
	if s.contextWindow() != 262144 {
		t.Fatalf("contextWindow = %d, want the learned 262144", s.contextWindow())
	}
	if n := calls.Load(); n < 2 {
		t.Fatalf("backend saw %d requests; the retry never reached it", n)
	}
}

// recordedRequest is one chat request as the backend saw it: the roles in
// order, and the text of every system message in it.
type recordedRequest struct {
	roles  []string
	system string
}

func (r recordedRequest) hasRole(role string) bool {
	return slices.Contains(r.roles, role)
}

// requestLog collects recordedRequests across goroutines. The mutex is not
// decoration: the handler runs on httptest's goroutines and this suite runs
// under -race.
type requestLog struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

// record parses one request body and appends it, returning the 1-based index of
// the request just seen so the handler can script its reply.
func (l *requestLog) record(body []byte) int {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)

	rec := recordedRequest{}
	var sys []string
	for _, m := range req.Messages {
		rec.roles = append(rec.roles, m.Role)
		if m.Role == "system" {
			sys = append(sys, m.Content)
		}
	}
	rec.system = strings.Join(sys, "\n\n")

	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, rec)
	return len(l.reqs)
}

func (l *requestLog) all() []recordedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.reqs)
}

// The retried turn is still the same assistant.
//
// compactHistory always puts message 0 in the head it summarises, and
// renderMessages skips system content — so the system prompt was dropped and
// not even represented in the summary. SendMessage's guard that re-adds it sits
// ABOVE the goal loop, which the recovery's `continue` re-enters from inside:
// the retry went out with no identity, no working directory, no skills index
// and none of the tool guidance, silently, on the one path whose stated purpose
// is producing a completed turn.
//
// Asserted over a real HTTP boundary because that is the only place the question
// is settled: what matters is the message list that actually left the process,
// after cogito's own normalisation, not what nib's fragment happened to hold.
//
// The seeding ORDER is the whole test. Both other overflow fixtures seed history
// BEFORE SendMessage appends the system prompt, which leaves the system message
// inside the kept tail where compaction never touches it — from there the bug is
// invisible. A real session adds the system prompt on its FIRST turn, so it sits
// at message 0 with history accumulating after it, which is exactly where
// splitForCompaction puts it in the head. Seed system-first or prove nothing.
func TestOverflowRetryKeepsTheSystemPrompt(t *testing.T) {
	xlog.SetLogger(xlog.NewLogger(xlog.LogLevel("error"), ""))

	log := &requestLog{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := log.record(body)

		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "request (368203 tokens) exceeds the available context size (262144 tokens)",
					"type":    "invalid_request_error",
				},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-1", "object": "chat.completion", "created": 1, "model": "m",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "recovered"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11},
		})
	}))
	defer srv.Close()

	const prompt = "you are nib, working in /tmp/nowhere, with a skills index and tool guidance"
	cfg := types.Config{
		Model:        "m",
		APIKey:       "k",
		BaseURL:      srv.URL + "/v1",
		LogLevel:     "error",
		ApprovalMode: "auto",
		// Reload assigns the system prompt only when cfg.Prompt is non-empty, so
		// without this the session has NO system prompt and every assertion below
		// would pass vacuously. The guard after NewSession says so out loud.
		Prompt: prompt,
		// MaxRetries 1: at 3 cogito absorbs the single 400 inside one
		// ExecuteTools run and nib's recovery never fires. See
		// TestOverflowRecoveryOverHTTP.
		AgentOptions: types.AgentOptions{Iterations: 10, MaxAttempts: 3, MaxRetries: 1},
		Compaction: types.CompactionConfig{
			MaxContextTokens: 400000, Threshold: 0.8, KeepRecent: 2, ReserveTokens: 4096,
		},
	}
	s, err := NewSession(context.Background(), cfg, Callbacks{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	if s.systemPrompt == "" {
		t.Fatal("the fixture has no system prompt; every assertion below would pass vacuously")
	}
	// Seed the way a real session reaches this state: the system prompt goes in
	// FIRST (the first turn's own add), then history accumulates after it.
	s.ensureSystemPrompt()
	s.fragment = s.fragment.
		AddMessage("user", "u1").AddMessage("assistant", "a1").
		AddMessage("user", "u2").AddMessage("assistant", "a2")
	if got := s.fragment.Messages[0].Role; got != "system" {
		t.Fatalf("seeded fragment starts with %q, not the system prompt: the bug would be hidden", got)
	}

	if _, err := s.SendMessage("what changed?"); err != nil {
		t.Fatalf("the turn failed instead of recovering: %v", err)
	}
	if n := s.overflowRetries(); n != 1 {
		t.Fatalf("overflow retries = %d, want exactly 1: no retry was exercised", n)
	}

	reqs := log.all()
	// Three requests: the overflow, compaction's summarising call, the retry.
	if len(reqs) < 3 {
		t.Fatalf("backend saw %d requests, want at least 3 (overflow, summary, retry)", len(reqs))
	}
	if !reqs[0].hasRole("system") || !strings.Contains(reqs[0].system, prompt) {
		t.Fatalf("the FIRST request already lacked the system prompt (roles=%v): the fixture is wrong, not the code", reqs[0].roles)
	}
	retry := reqs[len(reqs)-1]
	if !retry.hasRole("system") {
		t.Fatalf("the retried turn carried no system message at all; roles=%v", retry.roles)
	}
	if !strings.Contains(retry.system, prompt) {
		t.Fatalf("the retried turn's system block does not carry the session's system prompt; got %q", retry.system)
	}
}
