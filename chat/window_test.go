package chat

import (
	"math"
	"testing"

	"github.com/mudler/nib/types"
)

func TestContextBudgetSubtractsTheReserve(t *testing.T) {
	cfg := types.CompactionConfig{ReserveTokens: 4096}
	if got := contextBudget(cfg, 262144); got != 258048 {
		t.Fatalf("budget = %d, want 258048", got)
	}
}

// A window at or below the reserve would otherwise produce a zero or negative
// budget and fire compaction on every single turn.
func TestContextBudgetFloorsAtZero(t *testing.T) {
	cfg := types.CompactionConfig{ReserveTokens: 4096}
	if got := contextBudget(cfg, 1000); got != 0 {
		t.Fatalf("budget = %d, want 0", got)
	}
}

// The reporter's exact configuration from issue #53: they set the true window
// and still overflowed, because nothing was held back.
func TestReportersConfigurationNowTriggersBeforeTheLimit(t *testing.T) {
	cfg := types.CompactionConfig{MaxContextTokens: 262144, Threshold: 0.8, ReserveTokens: 4096}
	// (262144 - 4096) * 0.8 = 206438.4
	if shouldAutoCompact(cfg, 262144, 206000) {
		t.Fatal("fired below the trigger")
	}
	if !shouldAutoCompact(cfg, 262144, 207000) {
		t.Fatal("did not fire above the trigger; the reporter's overflow would repeat")
	}
}

// A window that leaves no budget at all (the whole window is reserved) must not
// make compaction fire on every turn. Compaction stays off instead.
func TestShouldAutoCompactIsOffWhenTheReserveEatsTheWholeWindow(t *testing.T) {
	cfg := types.CompactionConfig{Threshold: 0.8, ReserveTokens: 4096}
	if shouldAutoCompact(cfg, 4096, 999999) {
		t.Fatal("a zero budget must disable auto-compaction, not fire it every turn")
	}
}

func TestRememberedWindowIsUsedForTheSameModel(t *testing.T) {
	s := &Session{llmModel: "qwen"}
	s.compaction = types.CompactionConfig{MaxContextTokens: 400000}

	s.rememberWindow(262144, "qwen")

	if got := s.contextWindow(); got != 262144 {
		t.Fatalf("contextWindow = %d, want the learned 262144", got)
	}
}

// The learned window is a fact about ONE model. Carrying a 262k window into a
// session that switched to an 8k model would suppress compaction exactly when
// it is most needed — and Reload can change the model just as SwitchModel can,
// which is why the rule is keyed on the name rather than on the method.
func TestRememberedWindowIsIgnoredAfterTheModelChanges(t *testing.T) {
	s := &Session{llmModel: "qwen"}
	s.compaction = types.CompactionConfig{MaxContextTokens: 128000}
	s.rememberWindow(262144, "qwen")

	s.llmModel = "tiny"

	if got := s.contextWindow(); got != 128000 {
		t.Fatalf("contextWindow = %d, want the configured 128000: the learned window belongs to another model", got)
	}
}

// Switching back is not a new fact, but the old fact is still true: the window
// was learned for "qwen" and "qwen" is current again.
func TestRememberedWindowAppliesAgainWhenTheModelComesBack(t *testing.T) {
	s := &Session{llmModel: "qwen"}
	s.compaction = types.CompactionConfig{MaxContextTokens: 128000}
	s.rememberWindow(262144, "qwen")

	s.llmModel = "tiny"
	s.llmModel = "qwen"

	if got := s.contextWindow(); got != 262144 {
		t.Fatalf("contextWindow = %d, want the learned 262144", got)
	}
}

func TestContextWindowFallsBackToConfigWhenNothingLearned(t *testing.T) {
	s := &Session{llmModel: "qwen"}
	s.compaction = types.CompactionConfig{MaxContextTokens: 128000}
	if got := s.contextWindow(); got != 128000 {
		t.Fatalf("contextWindow = %d, want 128000", got)
	}
}

// Learned wins in BOTH directions — the backend is the authority on its own
// limit. A user's smaller number is overridden too, which is a deliberate
// choice recorded in the spec.
func TestLearnedWindowOverridesASmallerConfiguredValue(t *testing.T) {
	s := &Session{llmModel: "qwen"}
	s.compaction = types.CompactionConfig{MaxContextTokens: 128000}
	s.rememberWindow(262144, "qwen")
	if got := s.contextWindow(); got != 262144 {
		t.Fatalf("contextWindow = %d, want the learned 262144", got)
	}
}

// A figure outside the plausibility band is a misparse, not a model. Below the
// floor it is almost certainly an output-token count picked up by the parser;
// above the ceiling it is a digit run strconv.Atoi clamped to MaxInt. Either
// one, stored, would be worse than the configured guess it replaced.
func TestRememberWindowRejectsImplausibleValues(t *testing.T) {
	s := &Session{llmModel: "qwen"}
	s.compaction = types.CompactionConfig{MaxContextTokens: 128000, ReserveTokens: 4096}

	for _, bad := range []int{0, -1, 100, 512, minPlausibleWindow - 1, maxPlausibleWindow + 1, math.MaxInt} {
		s.rememberWindow(bad, "qwen")
		if got := s.contextWindow(); got != 128000 {
			t.Fatalf("window %d was remembered; contextWindow = %d, want the configured 128000", bad, got)
		}
	}
}

// An unnamed model cannot be matched against llmModel later, so remembering a
// window for it would either never apply or apply to the wrong model.
func TestRememberWindowRejectsAnEmptyModelName(t *testing.T) {
	s := &Session{llmModel: ""}
	s.compaction = types.CompactionConfig{MaxContextTokens: 128000}
	s.rememberWindow(262144, "")
	if got := s.contextWindow(); got != 128000 {
		t.Fatalf("contextWindow = %d, want the configured 128000", got)
	}
}

// Both ends of the band are inclusive, and the low end matters most: a real
// 2048- or 4096-context model is precisely nib's audience. Refusing to learn
// from one would trade the overflow bug for a silent no-op on the models that
// overflow soonest. 4096 is deliberately equal to the default reserve here —
// the guard is about misparses, not about small models.
func TestRememberWindowAcceptsTheEdgesOfThePlausibilityBand(t *testing.T) {
	for _, good := range []int{minPlausibleWindow, 2048, 4096, maxPlausibleWindow} {
		s := &Session{llmModel: "tiny"}
		s.compaction = types.CompactionConfig{MaxContextTokens: 128000, ReserveTokens: 4096}
		s.rememberWindow(good, "tiny")
		if got := s.contextWindow(); got != good {
			t.Fatalf("window %d was not remembered; contextWindow = %d", good, got)
		}
	}
}

// A later, larger window for the same model replaces the earlier one: the
// backend is the authority and its most recent statement is the current truth.
func TestRememberWindowOverwritesTheEarlierValue(t *testing.T) {
	s := &Session{llmModel: "qwen"}
	s.compaction = types.CompactionConfig{MaxContextTokens: 128000}
	s.rememberWindow(262144, "qwen")
	s.rememberWindow(8192, "qwen")
	if got := s.contextWindow(); got != 8192 {
		t.Fatalf("contextWindow = %d, want the most recent 8192", got)
	}
}
