package chat

import (
	"math"
	"testing"

	"github.com/mudler/nib/types"
)

func TestContextBudgetSubtractsTheReserve(t *testing.T) {
	cfg := types.CompactionConfig{ReserveTokens: 4096}
	if got := ContextBudget(cfg, 262144); got != 258048 {
		t.Fatalf("budget = %d, want 258048", got)
	}
}

// The floor exists for a window that is itself absent or nonsensical. With the
// reserve clamped to a quarter of the window, a positive window always keeps a
// positive budget, so this is the only way to reach zero.
func TestContextBudgetFloorsAtZero(t *testing.T) {
	cfg := types.CompactionConfig{ReserveTokens: 4096}
	for _, window := range []int{0, -1, -100000} {
		if got := ContextBudget(cfg, window); got != 0 {
			t.Fatalf("budget(%d) = %d, want 0", window, got)
		}
	}
}

// Above 4×ReserveTokens the clamp does nothing at all: the reserve is the flat
// configured count, exactly as the spec chose. A reply does not get longer
// because the window did.
func TestContextBudgetKeepsTheFlatReserveOnALargeWindow(t *testing.T) {
	cfg := types.CompactionConfig{ReserveTokens: 4096}
	// 16384 is the boundary: window/4 == ReserveTokens, flat still applies.
	for _, tc := range []struct{ window, want int }{
		{16384, 12288},   // 16384 - 4096
		{128000, 123904}, // 128000 - 4096
		{262144, 258048}, // 262144 - 4096
	} {
		if got := ContextBudget(cfg, tc.window); got != tc.want {
			t.Fatalf("budget(%d) = %d, want %d: the flat reserve must apply unchanged here", tc.window, got, tc.want)
		}
	}
}

// Below the boundary the flat reserve is incoherent — at 4096 it would claim
// the entire window — so it is clamped to a quarter and compaction keeps
// working. This is not the percentage reserve the spec rejected: it applies
// only here, and the trigger is still Threshold × budget.
func TestContextBudgetClampsTheReserveOnASmallWindow(t *testing.T) {
	cfg := types.CompactionConfig{ReserveTokens: 4096}
	for _, tc := range []struct{ window, want int }{
		{4096, 3072},   // reserve clamped 4096 → 1024
		{2048, 1536},   // reserve clamped 4096 → 512
		{16383, 12288}, // just under the boundary: reserve clamped to 4095
	} {
		if got := ContextBudget(cfg, tc.window); got != tc.want {
			t.Fatalf("budget(%d) = %d, want %d", tc.window, got, tc.want)
		}
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

// A small model must keep its auto-compaction. An unclamped flat reserve would
// take the whole 4096-token window, leave a budget of 0, and switch compaction
// OFF for the model that overflows soonest — and once windows are learned from
// overflow errors, LEARNING a real 4096 window would be the thing that disabled
// compaction for the model that had just overflowed.
func TestShouldAutoCompactStillFiresOnASmallWindow(t *testing.T) {
	cfg := types.CompactionConfig{Threshold: 0.8, ReserveTokens: 4096}
	// budget 3072, trigger at int(3072*0.8) = 2457.
	if shouldAutoCompact(cfg, 4096, 2456) {
		t.Fatal("fired below the trigger")
	}
	if !shouldAutoCompact(cfg, 4096, 2457) {
		t.Fatal("a 4096-token model got no auto-compaction at all")
	}
	if shouldAutoCompact(cfg, 4096, 100) {
		t.Fatal("an early turn must not compact: the clamp is not a licence to fire every turn")
	}
}

// The flat reserve is untouched on a real window: the boundary between the two
// regimes is 4×ReserveTokens, and the large side must behave exactly as it did
// before the clamp existed.
func TestShouldAutoCompactUsesTheFlatReserveOnALargeWindow(t *testing.T) {
	cfg := types.CompactionConfig{Threshold: 0.8, ReserveTokens: 4096}
	// budget 123904, trigger at int(123904*0.8) = 99123.
	if shouldAutoCompact(cfg, 128000, 99122) {
		t.Fatal("fired below the trigger")
	}
	if !shouldAutoCompact(cfg, 128000, 99123) {
		t.Fatal("did not fire above the trigger")
	}
}

// Auto-compaction is off when there is no window at all: nothing was learned
// and MaxContextTokens is 0. This property used to live in
// compact_internal_test.go against cfg.MaxContextTokens directly; it moved here
// with contextWindow, which is now the only thing that reads that field.
func TestNoWindowAtAllDisablesAutoCompaction(t *testing.T) {
	s := &Session{llmModel: "qwen"}
	s.compaction = types.CompactionConfig{MaxContextTokens: 0, Threshold: 0.8, ReserveTokens: 4096}

	if got := s.contextWindow(); got != 0 {
		t.Fatalf("contextWindow = %d, want 0", got)
	}
	if shouldAutoCompact(s.compaction, s.contextWindow(), 999999) {
		t.Fatal("MaxContextTokens=0 with nothing learned must never trigger")
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
// it is most needed. Comparing model names rather than hooking the one method
// that switches models today is what keeps that true for any future path that
// changes the model, without anyone having to remember to clear the field.
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
