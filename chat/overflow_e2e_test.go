package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
