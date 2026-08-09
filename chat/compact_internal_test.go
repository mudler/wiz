package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/mudler/cogito"
	"github.com/mudler/nib/types"
	openai "github.com/sashabaranov/go-openai"
)

func TestContextTokensPrefersReportedUsage(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{{Role: "user", Content: strings.Repeat("a", 40)}}
	s := &Session{}
	s.fragment = cogito.NewFragment(msgs...)

	// No reported usage yet → byte/4 estimate (40/4 = 10).
	if got := s.ContextTokens(); got != 10 {
		t.Fatalf("estimate path: ContextTokens = %d, want 10", got)
	}

	// Once the backend reports prompt tokens, that real figure wins.
	s.fragment.Status.LastUsage.PromptTokens = 1234
	if got := s.ContextTokens(); got != 1234 {
		t.Fatalf("reported path: ContextTokens = %d, want 1234", got)
	}
}

func TestSplitForCompactionKeepsToolGroupsIntact(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "assistant", ToolCalls: []openai.ToolCall{{Function: openai.FunctionCall{Name: "ls", Arguments: "{}"}}}},
		{Role: "tool", Content: "tool-result", ToolCallID: "x"},
		{Role: "assistant", Content: "a2"},
	}
	head, tail := splitForCompaction(msgs, 2)
	if len(head) != 2 {
		t.Fatalf("head len = %d, want 2", len(head))
	}
	if tail[0].Role != "assistant" || len(tail[0].ToolCalls) == 0 {
		t.Fatalf("tail must start at the assistant tool_calls message, got %+v", tail[0])
	}
	if len(head) > 0 && len(head[len(head)-1].ToolCalls) > 0 {
		t.Fatal("head must not end on an assistant tool_calls message")
	}
}

func TestSplitForCompactionNothingToCompact(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
	}
	head, tail := splitForCompaction(msgs, 8)
	if head != nil || tail != nil {
		t.Fatalf("expected (nil,nil) when too short, got head=%v tail=%v", head, tail)
	}
}

func TestShouldAutoCompact(t *testing.T) {
	// No ReserveTokens here, so the budget is the whole window and these
	// thresholds mean exactly what they meant before the signature changed.
	cfg := types.CompactionConfig{MaxContextTokens: 1000, Threshold: 0.8}
	if shouldAutoCompact(cfg, 1000, 799) {
		t.Fatal("799 < 800 should not trigger")
	}
	if !shouldAutoCompact(cfg, 1000, 800) {
		t.Fatal("800 >= 800 should trigger")
	}
	if shouldAutoCompact(types.CompactionConfig{Disabled: true, MaxContextTokens: 1000, Threshold: 0.8}, 1000, 999999) {
		t.Fatal("Disabled must never trigger")
	}
	if shouldAutoCompact(types.CompactionConfig{MaxContextTokens: 0}, 0, 999999) {
		t.Fatal("a zero window must never trigger")
	}
}

func TestEstimateAndHumanTokens(t *testing.T) {
	got := estimateTokens([]openai.ChatCompletionMessage{{Role: "user", Content: strings.Repeat("a", 40)}})
	if got != 10 { // 40 bytes / 4
		t.Fatalf("estimateTokens = %d, want 10", got)
	}
	if HumanTokens(950) != "950" {
		t.Fatalf("HumanTokens(950) = %q, want 950", HumanTokens(950))
	}
	if HumanTokens(47200) != "47.2k" {
		t.Fatalf("HumanTokens(47200) = %q, want 47.2k", HumanTokens(47200))
	}
}

// fakeSummaryLLM is a minimal cogito.LLM that returns a canned summary from Ask.
type fakeSummaryLLM struct {
	reply string
	err   error
	calls int
}

func (f *fakeSummaryLLM) Ask(ctx context.Context, fr cogito.Fragment) (cogito.Fragment, error) {
	f.calls++
	if f.err != nil {
		return cogito.Fragment{}, f.err
	}
	fr = fr.AddMessage(cogito.AssistantMessageRole, f.reply)
	if fr.Status != nil {
		fr.Status.LastUsage = cogito.LLMUsage{TotalTokens: 1}
	}
	return fr, nil
}

func (f *fakeSummaryLLM) CreateChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (cogito.LLMReply, cogito.LLMUsage, error) {
	return cogito.LLMReply{}, cogito.LLMUsage{}, nil
}

func newCompactTestSession(llm cogito.LLM, keepRecent int, frag []openai.ChatCompletionMessage, disp []openai.ChatCompletionMessage) *Session {
	s := &Session{
		ctx:        context.Background(),
		llm:        llm,
		compaction: types.CompactionConfig{KeepRecent: keepRecent, MaxContextTokens: 1000, Threshold: 0.8},
	}
	s.fragment = cogito.NewFragment(frag...)
	s.messages = disp
	return s
}

func TestCompactHistoryRebuildsAndSyncsDisplay(t *testing.T) {
	frag := []openai.ChatCompletionMessage{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "u3"},
		{Role: "assistant", Content: "a3"},
	}
	disp := []openai.ChatCompletionMessage{
		{Role: "user", Content: "u1"}, {Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"}, {Role: "assistant", Content: "a2"},
		{Role: "user", Content: "u3"}, {Role: "assistant", Content: "a3"},
	}
	llm := &fakeSummaryLLM{reply: "SUMMARY-TEXT"}
	s := newCompactTestSession(llm, 2, frag, disp)

	before, after, err := s.CompactHistory()
	if err != nil {
		t.Fatalf("CompactHistory error: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("expected 1 summary call, got %d", llm.calls)
	}
	if len(s.fragment.Messages) != 3 {
		t.Fatalf("fragment len = %d, want 3", len(s.fragment.Messages))
	}
	if !strings.Contains(s.fragment.Messages[0].Content, "SUMMARY-TEXT") {
		t.Fatalf("first message must contain the summary, got %q", s.fragment.Messages[0].Content)
	}
	if s.fragment.Messages[2].Content != "a3" {
		t.Fatalf("tail not preserved, got %q", s.fragment.Messages[2].Content)
	}
	got := s.GetMessages()
	if !strings.Contains(got[0].Content, "Compacted") {
		t.Fatalf("display[0] should be the compaction marker, got %q", got[0].Content)
	}
	if got[len(got)-1].Content != "a3" {
		t.Fatalf("display tail not preserved, got %q", got[len(got)-1].Content)
	}
	if before == 0 || after == 0 {
		t.Fatalf("expected non-zero before/after, got %d/%d", before, after)
	}
}

func TestCompactHistoryTooShortIsNoOp(t *testing.T) {
	frag := []openai.ChatCompletionMessage{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
	}
	llm := &fakeSummaryLLM{reply: "X"}
	s := newCompactTestSession(llm, 8, frag, frag)
	before, after, err := s.CompactHistory()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if before != after {
		t.Fatalf("no-op should report before==after, got %d/%d", before, after)
	}
	if llm.calls != 0 {
		t.Fatalf("no LLM call expected for a no-op, got %d", llm.calls)
	}
}

func TestCompactHistoryErrorIsAtomic(t *testing.T) {
	frag := []openai.ChatCompletionMessage{
		{Role: "user", Content: "u1"}, {Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"}, {Role: "assistant", Content: "a2"},
	}
	llm := &fakeSummaryLLM{err: context.DeadlineExceeded}
	s := newCompactTestSession(llm, 1, frag, frag)
	snapshot := len(s.fragment.Messages)
	_, _, err := s.CompactHistory()
	if err == nil {
		t.Fatal("expected an error when the summary call fails")
	}
	if len(s.fragment.Messages) != snapshot {
		t.Fatalf("fragment must be unchanged on error, len %d != %d", len(s.fragment.Messages), snapshot)
	}
}

func TestCompactHistoryEmptySummaryIsAtomic(t *testing.T) {
	frag := []openai.ChatCompletionMessage{
		{Role: "user", Content: "u1"}, {Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"}, {Role: "assistant", Content: "a2"},
	}
	llm := &fakeSummaryLLM{reply: ""} // model returns an empty summary
	s := newCompactTestSession(llm, 1, frag, frag)
	snapshot := len(s.fragment.Messages)
	_, _, err := s.CompactHistory()
	if err == nil {
		t.Fatal("expected an error when the summary is empty")
	}
	if len(s.fragment.Messages) != snapshot {
		t.Fatalf("fragment must be unchanged on empty-summary error, len %d != %d", len(s.fragment.Messages), snapshot)
	}
	if llm.calls != 1 {
		t.Fatalf("expected the LLM to have been called once, got %d", llm.calls)
	}
}
