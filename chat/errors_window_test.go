package chat

import (
	"errors"
	"testing"
)

// The four phrasings documented at chat/errors.go:11-14, verbatim. If a backend
// nib claims to support stops yielding a window, this is where it shows.
func TestLearnedWindowFromRealBackendPhrasings(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want int
	}{
		{
			name: "llama.cpp / LocalAI",
			msg:  "request (9739 tokens) exceeds the available context size (8192 tokens)",
			want: 8192,
		},
		{
			name: "OpenAI",
			msg:  "This model's maximum context length is 8192 tokens. However, your messages resulted in 9739 tokens",
			want: 8192,
		},
		{
			name: "vLLM",
			msg:  "maximum context length is 8192 tokens, however you requested 9739 tokens",
			want: 8192,
		},
		{
			name: "the reporter's own numbers, issue #53",
			msg:  "request larger than the model's context window (needs ~368203 tokens, model allows 262144 tokens)",
			want: 262144,
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
