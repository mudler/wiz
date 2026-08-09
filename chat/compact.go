package chat

import (
	"context"
	"fmt"
	"strings"

	"github.com/mudler/nib/types"

	"github.com/mudler/cogito"
	openai "github.com/sashabaranov/go-openai"
)

// compactInstruction is the prompt prefix used to summarize the older portion
// of a conversation during compaction.
const compactInstruction = "You are compacting a conversation to save context. " +
	"Summarize the conversation below, preserving decisions made, facts established, " +
	"file paths, identifiers, and any open tasks or unresolved questions. " +
	"Be concise but complete. Output only the summary."

// splitForCompaction partitions msgs into a head (to be summarized) and a tail
// (kept verbatim). keepRecent is the desired tail length; the boundary is moved
// backward so an assistant tool_calls message is never separated from its tool
// result messages (which would make the next API call invalid). It returns
// (nil, nil) when there is nothing worth compacting.
func splitForCompaction(msgs []openai.ChatCompletionMessage, keepRecent int) (head, tail []openai.ChatCompletionMessage) {
	if keepRecent < 1 {
		keepRecent = 1
	}
	start := len(msgs) - keepRecent
	if start < 1 {
		return nil, nil
	}
	// Never begin the tail on a tool result, and never leave a dangling
	// assistant tool_calls at the end of the head.
	for start > 0 && (msgs[start].Role == "tool" || len(msgs[start-1].ToolCalls) > 0) {
		start--
	}
	if start < 1 {
		return nil, nil
	}
	return msgs[:start], msgs[start:]
}

// shouldAutoCompact reports whether the last request's prompt tokens crossed the
// configured fraction of the BUDGET — the window minus the reserve. Auto-
// compaction is off when Disabled or when no window is available (configured or
// learned).
//
// The window is passed in rather than read from cfg because it may have been
// learned from the backend, which is more trustworthy than any configured
// guess.
func shouldAutoCompact(cfg types.CompactionConfig, window, promptTokens int) bool {
	if cfg.Disabled || window <= 0 {
		return false
	}
	budget := contextBudget(cfg, window)
	// Defensive only: contextBudget clamps the reserve to a quarter of the
	// window, so a positive window always has a positive budget and the
	// check above already rejected the rest. It stays because a zero budget
	// would make the trigger fire on every single turn, which is the one
	// outcome worse than never firing.
	if budget <= 0 {
		return false
	}
	threshold := cfg.Threshold
	if threshold <= 0 || threshold > 1 {
		threshold = 0.8
	}
	return promptTokens >= int(float64(budget)*threshold)
}

// minPlausibleWindow and maxPlausibleWindow bound what rememberWindow will
// believe. The band rejects a MISPARSE, not a small model: a 2048- or
// 4096-context model is exactly nib's audience, so the floor sits below any
// real served model and above any plausible misparse of an output-token count
// (the largest default max_tokens backends print in overflow errors is in the
// hundreds — vLLM's "512 output tokens" is the known example, and it is only
// skipped today because tokenCountRe's \s* cannot span the word "output").
//
// The ceiling exists because strconv.Atoi clamps an absurdly long digit run to
// MaxInt instead of erroring, so garbage lands far ABOVE any floor rather than
// below it. 1<<30 is roughly a hundred times the largest window any model has
// ever advertised (~10M tokens), so it cannot reject a real backend, while
// still catching the clamp.
const (
	minPlausibleWindow = 1024
	maxPlausibleWindow = 1 << 30
)

// rememberWindow records a context window a backend stated for a specific
// model. Values outside the plausibility band are discarded rather than
// stored, because an implausible figure is a parse artefact and storing one
// would be worse than the configured guess it replaced: too small and
// compaction fires every turn, too large and it never fires at all.
func (s *Session) rememberWindow(window int, model string) {
	if window < minPlausibleWindow || window > maxPlausibleWindow || model == "" {
		return
	}
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	s.learnedWindow, s.learnedWindowModel = window, model
}

// contextWindow returns the window compaction budgets against: the learned one
// when it belongs to the model currently in use, otherwise the configured
// MaxContextTokens.
//
// Keyed on the model NAME rather than on whichever method changed the model.
// A window learned from one backend's overflow error is a fact about ONE
// model, so it is meaningless except read as a pair with llmModel: carrying a
// 262k window into an 8k model would suppress compaction precisely when it is
// most needed. Comparing the names makes that safe for every path that can
// change the model, present or future, without anyone having to remember to
// clear the field from it. (Today only NewSession and SetModel assign
// llmModel; Reload does not.)
func (s *Session) contextWindow() int {
	s.modelMu.RLock()
	learned, forModel, current := s.learnedWindow, s.learnedWindowModel, s.llmModel
	s.modelMu.RUnlock()

	if learned > 0 && forModel == current {
		return learned
	}
	return s.compaction.MaxContextTokens
}

// canRecoverFromOverflow reports whether a failed run's error is one nib may
// act on: a context overflow the user did not cancel.
//
// The cancellation half is not redundant with isContextOverflow. Today cogito's
// retry loop calls backoffOrCancel after every failed attempt and returns
// ctx.Err() when the context is done, so an interrupt that lands during the
// request reaches here as "context canceled" and fails the overflow check
// anyway. That is cogito's mapping, not nib's guarantee: returning the last
// real error instead would be a perfectly reasonable change, and cancellation
// can also land in the window between ExecuteTools returning an overflow and
// this check running. A cancelled turn means the user pressed Ctrl+C, and
// re-sending the turn is the opposite of what they asked for — so the rule is
// stated here rather than inferred from another package's error mapping.
//
// It is a free function rather than a method because it reads no session state:
// that keeps the boundary testable without a live turn, which is the only way
// the interrupt case can be exercised at all (see the note above).
func canRecoverFromOverflow(turnCtx context.Context, err error) bool {
	return turnCtx.Err() == nil && isContextOverflow(err)
}

// overflowRetries reports how many context-overflow recoveries the current turn
// performed. Read by tests; the cap itself is enforced in SendMessage.
func (s *Session) overflowRetries() int {
	s.overflowMu.Lock()
	defer s.overflowMu.Unlock()
	return s.overflowRetried
}

// contextBudget is the window minus the reserve held back for the response,
// where the reserve is never allowed to claim more than a quarter of the
// window.
//
// The clamp is not the percentage reserve the spec rejected. The reserve stays
// a flat cfg.ReserveTokens for every window of 4×ReserveTokens or more — 16384
// and up at the 4096 default — and the trigger is still Threshold × budget, not
// a percentage of the window. The clamp bites only where the flat number is
// incoherent relative to the window it is being subtracted from: without it a
// 4096-token model reserves its entire window, the budget is 0, and auto-
// compaction switches OFF for the model that overflows soonest. Worse, once a
// window is learned from an overflow error, LEARNING a real 4096 window would
// be what disabled compaction for the model that just overflowed — the exact
// inverse of the point of learning it.
func contextBudget(cfg types.CompactionConfig, window int) int {
	reserve := min(cfg.ReserveTokens, window/4)
	b := window - reserve
	if b < 0 {
		return 0
	}
	return b
}

// estimateTokens is a cheap byte/4 approximation, used when no real usage figure
// is available (e.g. right after a rebuild, before the next live turn). It only
// counts m.Content and tool calls, ignoring m.MultiContent (multimedia parts).
func estimateTokens(msgs []openai.ChatCompletionMessage) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return n / 4
}

// ContextTokens reports the current conversation size in tokens for display:
// the last request's reported prompt tokens, or a byte/4 estimate when the
// backend hasn't reported usage yet (e.g. before the first turn). This is the
// same signal the auto-compaction trigger watches.
func (s *Session) ContextTokens() int {
	if s.fragment.Status != nil && s.fragment.Status.LastUsage.PromptTokens > 0 {
		return s.fragment.Status.LastUsage.PromptTokens
	}
	return estimateTokens(s.fragment.Messages)
}

// formatTokenCount returns the bare magnitude string for a token count, e.g.
// 950 → "950", 12000 → "12k", 47200 → "47.2k". A trailing ".0" is trimmed.
// Returns "" for zero/negative so callers can omit the segment.
func formatTokenCount(n int) string {
	if n <= 0 {
		return ""
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%.1f", float64(n)/1000.0)
	s = strings.TrimSuffix(s, ".0")
	return s + "k"
}

// compactedNotice is the line inserted into the visible transcript when older
// turns are summarized. It has no emoji: it renders in the TUI exactly like a
// render helper, and the calm no-emoji voice tui.TestNoEmojiInRenderHelpers
// guards applies to it even though that test cannot see it from here. Its own
// guard is TestCompactionTranscriptNoticeHasNoEmoji, in this package.
//
// It counts messages, not tokens, so unlike the CLI and TUI compaction notices
// it has nothing approximate to mark: len() of a slice is exact.
func compactedNotice(removed int) string {
	return fmt.Sprintf("Compacted %d earlier messages", removed)
}

// HumanTokens formats a token count compactly (e.g. 47200 → "47.2k").
func HumanTokens(n int) string {
	return formatTokenCount(n)
}

// renderMessages flattens messages to plain "role: content" lines for the
// summarization prompt, skipping system boilerplate and rendering tool calls
// inline. A message carrying both content and tool calls renders both. Like
// estimateTokens, this ignores m.MultiContent (multimedia parts).
func renderMessages(msgs []openai.ChatCompletionMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		if m.Content != "" {
			fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "%s: [tool call %s(%s)]\n", m.Role, tc.Function.Name, tc.Function.Arguments)
		}
	}
	return b.String()
}

// CompactHistory summarizes the older portion of the conversation via the LLM
// and rebuilds the fragment as [summary] + recent tail, keeping the display
// copy consistent. It returns byte/4 token estimates of the conversation before
// and after compaction (before==after signals a no-op). On summary failure it
// returns the error WITHOUT mutating session state (atomic swap).
func (s *Session) CompactHistory() (before, after int, err error) {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return s.compactHistory(ctx)
}

// compactHistory is the context-aware implementation of CompactHistory. The
// passed-in ctx governs the summarization LLM call, allowing callers (e.g.
// auto-compaction) to make it cancellable via a per-turn context.
func (s *Session) compactHistory(ctx context.Context) (before, after int, err error) {
	msgs := s.fragment.Messages

	before = estimateTokens(msgs)

	head, tail := splitForCompaction(msgs, s.compaction.KeepRecent)
	headContent := renderMessages(head)
	if strings.TrimSpace(headContent) == "" {
		return before, before, nil // nothing to compact
	}

	prompt := compactInstruction + "\n\n--- CONVERSATION ---\n" + headContent
	llm, _ := s.currentLLM()
	res, aerr := llm.Ask(ctx, cogito.NewFragment().AddMessage(cogito.UserMessageRole, prompt))
	// Compaction is not free, and it fires exactly when a session has already
	// grown expensive — so leaving it out would understate the runs that cost
	// the most.
	//
	// LastUsage, not CumulativeUsage: Ask is a single call on a throwaway
	// fragment that no ExecuteTools run ever stamped, so CumulativeUsage is
	// zero here and reading it would count nothing at all.
	//
	// Counted before BOTH exits below on purpose. What the backend served it
	// billed, whether the summary then came back empty or the call came back an
	// error, and a rejected summary must not also erase the spend — the same
	// rule the interrupted-turn path in SendMessage follows. Placing this after
	// either early return would make compaction the one path where paid-for
	// tokens vanish, precisely on the sessions that spend the most. Clients
	// that discard the fragment on error simply leave Status nil and add
	// nothing, so the guard is what keeps this honest rather than optimistic.
	if res.Status != nil {
		s.addUsage(res.Status.LastUsage)
	}
	if aerr != nil {
		return before, before, fmt.Errorf("compaction summary failed: %w", aerr)
	}
	last := res.LastMessage()
	if last == nil || strings.TrimSpace(last.Content) == "" {
		return before, before, fmt.Errorf("compaction produced an empty summary")
	}

	// Build the new state up front; swap only after success (atomic).
	summaryMsg := openai.ChatCompletionMessage{
		Role:    "user",
		Content: "[Earlier conversation compacted]\n\n" + last.Content,
	}
	newFragMsgs := append([]openai.ChatCompletionMessage{summaryMsg}, tail...)

	displayTail := []openai.ChatCompletionMessage{}
	for _, m := range tail {
		if (m.Role == "user" || m.Role == "assistant") && strings.TrimSpace(m.Content) != "" {
			displayTail = append(displayTail, openai.ChatCompletionMessage{Role: m.Role, Content: m.Content})
		}
	}
	removed := len(s.messages) - len(displayTail)
	if removed < 0 {
		removed = 0
	}
	newMessages := append([]openai.ChatCompletionMessage{{
		Role:    "assistant",
		Content: compactedNotice(removed),
	}}, displayTail...)

	s.historyMu.Lock()
	newFrag := cogito.NewFragment(newFragMsgs...)
	if s.fragment.Status != nil {
		newFrag.Status = s.fragment.Status // preserve running token counters
	}
	s.fragment = newFrag
	s.messages = newMessages
	s.historyMu.Unlock()

	after = estimateTokens(newFrag.Messages)
	return before, after, nil
}
