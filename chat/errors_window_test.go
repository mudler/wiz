package chat

import (
	"errors"
	"testing"
)

// The real messages the backends nib supports actually emit, verbatim from
// their sources (see the provenance note on each case and the comment at the
// top of errors.go). Paraphrasing a backend here would assert coverage nib
// does not have: an abbreviated message can keep matching a marker while
// losing the second figure that makes it parseable.
func TestLearnedWindowFromRealBackendPhrasings(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want int
	}{
		{
			// llama.cpp tools/server/server-context.cpp: string_format(
			// "request (%d tokens) exceeds the available context size (%d tokens), try increasing it").
			// LocalAI surfaces llama.cpp's text unchanged.
			name: "llama.cpp / LocalAI",
			msg:  "request (9739 tokens) exceeds the available context size (8192 tokens), try increasing it",
			want: 8192,
		},
		{
			// OpenAI's chat/completions 400. Here the limit comes FIRST and the
			// request size second, the opposite order to llama.cpp — which is
			// why the rule is "smaller figure", not "second figure".
			name: "OpenAI",
			msg:  "This model's maximum context length is 8192 tokens. However, your messages resulted in 9739 tokens. Please reduce the length of the messages.",
			want: 8192,
		},
		{
			// vLLM current, vllm/renderers/params.py. Note the near-miss: the
			// "512 output tokens" and "9739 input tokens" figures do NOT match
			// tokenCountRe, because the intervening word breaks `\s*tokens`.
			// The two that do match are the limit and the total, so the
			// smaller is still the window.
			name: "vLLM, current",
			msg:  "This model's maximum context length is 8192 tokens. However, you requested 512 output tokens and your prompt contains 9739 input tokens, for a total of 10251 tokens. Please reduce the length of the input prompt or the number of requested output tokens.",
			want: 8192,
		},
		{
			// vLLM's older OpenAI-compatible server. Still deployed widely
			// enough to matter, and the figures inside the parenthetical are
			// likewise unmatched, leaving the limit and the total.
			name: "vLLM, older OpenAI-compatible server",
			msg:  "This model's maximum context length is 8192 tokens. However, you requested 10251 tokens (9739 in the messages, 512 in the completion). Please reduce the length of the messages or completion.",
			want: 8192,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := learnedWindowFrom(errors.New(tc.msg))
			if !ok {
				t.Fatalf("no window learned from %q", tc.msg)
			}
			if got != tc.want {
				t.Fatalf("window = %d, want %d (the SMALLER figure is the limit)", got, tc.want)
			}
		})
	}
}

// One figure is unattributable: it could be the limit or the request size, and
// mistaking a request size for the window would suppress compaction exactly
// when it is most needed.
//
// This exact string is what errors.go used to document as vLLM's message. It
// is only a fragment of it, and the fragment teaches nothing — kept here so
// the gap is asserted rather than assumed away.
func TestLearnedWindowNeedsBothFigures(t *testing.T) {
	if got, ok := learnedWindowFrom(errors.New("maximum context length is 8192 tokens")); ok {
		t.Fatalf("learned %d from a single figure; it is unattributable", got)
	}
}

// An error that is not an overflow teaches nothing, however many numbers it has.
func TestLearnedWindowIgnoresUnrelatedErrors(t *testing.T) {
	if _, ok := learnedWindowFrom(errors.New("read 4096 tokens from 8192 bytes of nonsense")); ok {
		t.Fatal("learned a window from an error that is not a context overflow")
	}
	if _, ok := learnedWindowFrom(nil); ok {
		t.Fatal("learned a window from a nil error")
	}
}

// The wiring guard. humanizeError's own text drops the second figure
// ("model allows 8192)" carries no "tokens"), so if learnedWindowFrom read
// only the outermost message, a humanized error would teach nothing. Recovery
// runs before humanizeError today; if that order ever changes, nothing else in
// the suite would notice that nib had silently stopped learning windows.
func TestLearnedWindowSurvivesHumanizeError(t *testing.T) {
	raw := errors.New("request (9739 tokens) exceeds the available context size (8192 tokens), try increasing it")
	humanized := humanizeError(raw)
	if humanized == raw {
		t.Fatal("humanizeError did not recognise the overflow; this test is not exercising what it claims")
	}
	got, ok := learnedWindowFrom(humanized)
	if !ok {
		t.Fatalf("no window learned from the humanized error %q", humanized)
	}
	if got != 8192 {
		t.Fatalf("window = %d, want 8192", got)
	}
}

func TestIsContextOverflowMatchesTheMarkers(t *testing.T) {
	if !isContextOverflow(errors.New("exceeds the available context size")) {
		t.Fatal("marker not recognised")
	}
	if isContextOverflow(errors.New("connection refused")) {
		t.Fatal("unrelated error classified as an overflow")
	}
	if isContextOverflow(nil) {
		t.Fatal("nil classified as an overflow")
	}
}
