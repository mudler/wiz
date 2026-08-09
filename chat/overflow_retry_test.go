package chat

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mudler/cogito"
	"github.com/mudler/nib/types"
	openai "github.com/sashabaranov/go-openai"
)

// overflowLLM fails the first N completion calls with a backend overflow error,
// then succeeds. Ask always succeeds, so compaction's summarising call works.
type overflowLLM struct {
	mu       sync.Mutex
	failures int // remaining calls that should overflow
	calls    int
	asks     int
}

func (o *overflowLLM) CreateChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (cogito.LLMReply, cogito.LLMUsage, error) {
	if err := ctx.Err(); err != nil {
		return cogito.LLMReply{}, cogito.LLMUsage{}, err
	}
	o.mu.Lock()
	o.calls++
	fail := o.failures > 0
	if fail {
		o.failures--
	}
	o.mu.Unlock()

	if fail {
		return cogito.LLMReply{}, cogito.LLMUsage{PromptTokens: 50, TotalTokens: 50},
			errors.New("request (368203 tokens) exceeds the available context size (262144 tokens)")
	}
	return cogito.LLMReply{
		ChatCompletionResponse: openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message:      openai.ChatCompletionMessage{Role: "assistant", Content: "ok"},
				FinishReason: openai.FinishReasonStop,
			}},
		},
	}, cogito.LLMUsage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11}, nil
}

// Ask is compaction's summariser. Every call returns a DIFFERENT summary, which
// is not decoration: a real summariser never returns byte-identical text for
// two different inputs, and a fake that did would make the second compaction
// look like a no-op to estimateTokens (same fragment shape, same lengths). The
// `before == after` guard would then be what stopped an uncapped retry, and
// TestOverflowRetriesAtMostOnce would keep passing with the cap deleted —
// hiding the infinite loop the cap exists to prevent. Varying the text puts the
// cap, and only the cap, on the hook.
func (o *overflowLLM) Ask(ctx context.Context, f cogito.Fragment) (cogito.Fragment, error) {
	o.mu.Lock()
	o.asks++
	n := o.asks
	o.mu.Unlock()

	summary := "summary " + strconv.Itoa(n) + " of earlier turns" + strings.Repeat(" with more detail", n)
	out := f.AddMessage("assistant", summary)
	if out.Status != nil {
		out.Status.LastUsage = cogito.LLMUsage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22}
	}
	return out, nil
}

func newOverflowSession(t *testing.T, llm cogito.LLM) *Session {
	t.Helper()
	s := &Session{
		ctx:          context.Background(),
		llm:          llm,
		llmModel:     "qwen",
		systemPrompt: "you are the overflow-test assistant",
		// MaxRetries is 1, not the brief's 3. MaxRetries is what drives cogito's
		// per-decision retry loop, so at 3 a single overflowing call is absorbed
		// INSIDE one ExecuteTools run: the run returns success, nib's recovery
		// never fires, and every test here passes on cogito's retry rather than
		// on the code under test. At 1, one completion call is one ExecuteTools
		// run, so `failures` maps to attempts and the assertions mean what they
		// say. It also drops the suite from ~27s of backoff to under a second.
		cogitoOptions: types.AgentOptions{Iterations: 10, MaxAttempts: 3, MaxRetries: 1},
		agentManager:  cogito.NewAgentManager(),
		agentLogs:     newAgentLogStore(),
		inject:        make(chan openai.ChatCompletionMessage, 8),
		compaction:    types.CompactionConfig{MaxContextTokens: 262144, Threshold: 0.8, KeepRecent: 2, ReserveTokens: 4096},
	}
	// Enough history that compaction has something to summarise; without a
	// head to compact the retry is correctly skipped and the test would prove
	// nothing.
	s.fragment = cogito.NewFragment(
		openai.ChatCompletionMessage{Role: "user", Content: "u1"},
		openai.ChatCompletionMessage{Role: "assistant", Content: "a1"},
		openai.ChatCompletionMessage{Role: "user", Content: "u2"},
		openai.ChatCompletionMessage{Role: "assistant", Content: "a2"},
	)
	return s
}

// The headline: an overflow no longer ends the turn.
func TestOverflowCompactsAndRetriesOnce(t *testing.T) {
	llm := &overflowLLM{failures: 1}
	s := newOverflowSession(t, llm)

	got, err := s.SendMessage("what changed?")
	if err != nil {
		t.Fatalf("SendMessage returned an error after an overflow that compaction could fix: %v", err)
	}
	if got == "" {
		t.Fatal("no response after the retry")
	}
	// Without this the test cannot tell nib's recovery from a backend that
	// simply answered on a later call.
	if s.overflowRetries() != 1 {
		t.Fatalf("overflow retries = %d; the answer did not come from the recovery path", s.overflowRetries())
	}
}

// The window the backend stated is kept, so later turns budget against the
// truth rather than a guess.
func TestOverflowLearnsTheWindow(t *testing.T) {
	llm := &overflowLLM{failures: 1}
	s := newOverflowSession(t, llm)
	// The configured window is deliberately NOT 262144: contextWindow falls
	// back to MaxContextTokens, so leaving the two equal would make this pass
	// without anything ever being learned.
	s.compaction.MaxContextTokens = 8192

	if _, err := s.SendMessage("what changed?"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := s.contextWindow(); got != 262144 {
		t.Fatalf("contextWindow = %d, want the learned 262144", got)
	}
}

// THE test. Compaction cannot always help; a loop re-summarises into the same
// wall and burns tokens on every pass.
func TestOverflowRetriesAtMostOnce(t *testing.T) {
	llm := &overflowLLM{failures: 99} // never recovers
	s := newOverflowSession(t, llm)

	if _, err := s.SendMessage("what changed?"); err == nil {
		t.Fatal("expected the turn to fail when compaction cannot help")
	}

	llm.mu.Lock()
	calls := llm.calls
	llm.mu.Unlock()

	// cogito may retry internally per attempt, so assert on ATTEMPTS rather
	// than a raw call count: exactly two ExecuteTools passes, no more.
	if calls < 2 {
		t.Fatalf("only %d completion calls; the retry never happened", calls)
	}
	if s.overflowRetries() != 1 {
		t.Fatalf("overflow retries = %d, want exactly 1", s.overflowRetries())
	}
}

// billedOverflowLLM models the shape a failed run really has: a call the
// backend served and billed, then the overflow.
//
// A run whose every call errored has no spend for nib to lose — cogito's
// counting wrapper only records usage when the call returned nil error (see
// usage_counter.go), which is the same reason usage_test.go's interruptedLLM
// scripts a served call rather than a plain error. So the first call returns
// usage with an empty reply: cogito counts the 900 tokens, finds no choices,
// and retries into the overflow that fails the run with the spend already
// stamped onto the fragment.
type billedOverflowLLM struct {
	overflowLLM
	seq int // completion calls so far
}

func (b *billedOverflowLLM) CreateChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (cogito.LLMReply, cogito.LLMUsage, error) {
	if err := ctx.Err(); err != nil {
		return cogito.LLMReply{}, cogito.LLMUsage{}, err
	}
	b.mu.Lock()
	b.seq++
	b.calls++
	n := b.seq
	b.mu.Unlock()

	switch n {
	case 1: // served and billed, but unusable — cogito retries
		return cogito.LLMReply{}, cogito.LLMUsage{PromptTokens: 860, CompletionTokens: 40, TotalTokens: 900}, nil
	case 2: // the overflow that fails the run
		return cogito.LLMReply{}, cogito.LLMUsage{},
			errors.New("request (368203 tokens) exceeds the available context size (262144 tokens)")
	default: // the retry, after compaction
		return cogito.LLMReply{
			ChatCompletionResponse: openai.ChatCompletionResponse{
				Choices: []openai.ChatCompletionChoice{{
					Message:      openai.ChatCompletionMessage{Role: "assistant", Content: "ok"},
					FinishReason: openai.FinishReasonStop,
				}},
			},
		}, cogito.LLMUsage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11}, nil
	}
}

// Both attempts spent real tokens. #54 established that a failed run's spend
// must still be counted, and a retry is two runs.
func TestOverflowCountsBothAttemptsTokens(t *testing.T) {
	llm := &billedOverflowLLM{}
	s := newOverflowSession(t, llm)
	// Two calls per decision, so the billed call and the overflow land in the
	// SAME (failed) ExecuteTools run.
	s.cogitoOptions.MaxRetries = 2

	if _, err := s.SendMessage("what changed?"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	// The failed attempt was billed 900, the compaction summary 22, the
	// successful attempt 11. Below 900 means the failed attempt's spend was
	// dropped on the way to the retry.
	if got := s.Usage().TotalTokens; got < 900 {
		t.Fatalf("TotalTokens = %d; the failed attempt's spend was dropped", got)
	}
	if s.overflowRetries() != 1 {
		t.Fatalf("overflow retries = %d, want 1", s.overflowRetries())
	}
}

// A cancelled turn means the user pressed Ctrl+C. Re-sending is the opposite of
// what they asked for.
//
// The interrupt is fired from INSIDE the LLM call rather than from a racing
// goroutine. SendMessage installs the turn's cancel function on entry (Interrupt
// is a no-op while s.turnCancel is nil, and endTurn nils it again), so an
// Interrupt raced from outside can land before it exists and cancel nothing —
// the test would then pass for the wrong reason. Interrupting from within the
// call guarantees the turn is genuinely in flight.
type interruptingOverflowLLM struct {
	overflowLLM
	s *Session
}

func (i *interruptingOverflowLLM) CreateChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (cogito.LLMReply, cogito.LLMUsage, error) {
	i.s.Interrupt()
	return i.overflowLLM.CreateChatCompletion(ctx, req)
}

// The interrupt boundary, tested directly. Going through SendMessage cannot
// prove it: cogito's decision loop calls backoffOrCancel after every failed
// attempt and returns ctx.Err() when the context is done, so an interrupt
// during the request arrives as "context canceled" and would be skipped by the
// overflow check alone. Deleting the cancellation half of the guard therefore
// leaves TestOverflowDoesNotRetryAnInterruptedTurn passing — verified. This is
// the test that holds the rule.
func TestCanRecoverFromOverflowRefusesACancelledTurn(t *testing.T) {
	overflow := errors.New("request (368203 tokens) exceeds the available context size (262144 tokens)")

	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !canRecoverFromOverflow(live, overflow) {
		t.Fatal("a live turn that overflowed must be recoverable")
	}

	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if canRecoverFromOverflow(cancelled, overflow) {
		t.Fatal("an interrupted turn was marked recoverable; Ctrl+C would be answered with a re-send")
	}

	if canRecoverFromOverflow(live, errors.New("connection refused")) {
		t.Fatal("a non-overflow failure was marked recoverable")
	}
	if canRecoverFromOverflow(live, nil) {
		t.Fatal("a nil error was marked recoverable")
	}
}

func TestOverflowDoesNotRetryAnInterruptedTurn(t *testing.T) {
	llm := &interruptingOverflowLLM{overflowLLM: overflowLLM{failures: 99}}
	s := newOverflowSession(t, llm)
	llm.s = s

	if _, err := s.SendMessage("what changed?"); err == nil {
		t.Fatal("expected the interrupted turn to fail")
	}
	if s.overflowRetries() != 0 {
		t.Fatalf("overflow retries = %d on an interrupted turn, want 0", s.overflowRetries())
	}
}

// Nothing to summarise means the retry would send a byte-identical request.
func TestOverflowSkipsRetryWhenCompactionIsANoOp(t *testing.T) {
	llm := &overflowLLM{failures: 99}
	s := newOverflowSession(t, llm)
	// An empty starting fragment, not the brief's single message: SendMessage
	// prepends the system prompt and appends the user turn, so a one-message
	// fragment reaches splitForCompaction as THREE messages and leaves a head
	// to summarise. Starting empty lands on exactly KeepRecent messages, which
	// is the state that really has nothing to compact.
	s.fragment = cogito.NewEmptyFragment()

	if _, err := s.SendMessage("what changed?"); err == nil {
		t.Fatal("expected an error")
	}
	if s.overflowRetries() != 0 {
		t.Fatalf("overflow retries = %d when there was nothing to compact, want 0", s.overflowRetries())
	}
}

// summaryFailingLLM overflows on completions AND on the summarising call:
// compaction is an LLM call built from the same oversized history, so the
// history that broke the turn can break the summary too.
type summaryFailingLLM struct {
	overflowLLM
}

func (f *summaryFailingLLM) Ask(ctx context.Context, frag cogito.Fragment) (cogito.Fragment, error) {
	return frag, errors.New("request (368203 tokens) exceeds the available context size (262144 tokens)")
}

// When compaction's own call fails, the user gets the ORIGINAL overflow — the
// one they can act on — and no retry is attempted.
func TestOverflowReportsOriginalErrorWhenCompactionFails(t *testing.T) {
	llm := &summaryFailingLLM{overflowLLM: overflowLLM{failures: 99}}
	s := newOverflowSession(t, llm)

	_, err := s.SendMessage("what changed?")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !isContextOverflow(err) {
		t.Fatalf("error = %v; want the original context-overflow error", err)
	}
	if strings.Contains(err.Error(), "compaction summary failed") {
		t.Fatalf("error = %v; the summariser's failure masked the overflow the user can act on", err)
	}
	if s.overflowRetries() != 0 {
		t.Fatalf("overflow retries = %d after a failed compaction, want 0", s.overflowRetries())
	}
}
