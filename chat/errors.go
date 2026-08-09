package chat

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// contextOverflowMarkers are substrings different backends emit when a request
// exceeds the model's context window. Only a fragment of each message is
// matched here, because the wording around it varies. The full messages, which
// learnedWindowFrom parses for the model's stated limit, are:
//
//   - llama.cpp / LocalAI (tools/server/server-context.cpp):
//     "request (9739 tokens) exceeds the available context size (8192 tokens), try increasing it"
//   - OpenAI:
//     "This model's maximum context length is 8192 tokens. However, your messages resulted in 9739 tokens. Please reduce the length of the messages."
//   - vLLM, current (vllm/renderers/params.py):
//     "This model's maximum context length is 8192 tokens. However, you requested 512 output tokens and your prompt contains 9739 input tokens, for a total of 10251 tokens. Please reduce the length of the input prompt or the number of requested output tokens."
//   - vLLM, older OpenAI-compatible server:
//     "This model's maximum context length is 8192 tokens. However, you requested 10251 tokens (9739 in the messages, 512 in the completion). Please reduce the length of the messages or completion."
//
// This comment used to abbreviate vLLM's message to "maximum context length is
// 8192 tokens". That fragment is enough to trip a marker but carries only ONE
// figure, so it teaches learnedWindowFrom nothing — an abbreviation that reads
// as a complete message invites tests asserting coverage nib does not have.
// Record the whole thing.
var contextOverflowMarkers = []string{
	"exceeds the available context size",
	"maximum context length",
	"context length",
	"context window",
	"context size",
}

// tokenCountRe pulls token counts (e.g. "8192 tokens") out of a backend error
// so we can report how far over the limit the request was.
var tokenCountRe = regexp.MustCompile(`(\d{3,})\s*tokens`)

// FriendlyError wraps a noisy backend error with a short, actionable message
// while preserving the original via Unwrap (so errors.Is/As still work).
type FriendlyError struct {
	err error
	msg string
}

func (e *FriendlyError) Error() string { return e.msg }
func (e *FriendlyError) Unwrap() error { return e.err }

// humanizeError rewrites known, verbose backend failures into a concise,
// actionable message. Unrecognized errors are returned unchanged, so callers
// can wrap the result unconditionally.
func humanizeError(err error) error {
	if err == nil {
		return nil
	}
	if isContextOverflow(err) {
		return &FriendlyError{err: err, msg: contextOverflowMessage(err.Error())}
	}
	return err
}

// isContextOverflow reports whether err is a backend complaining that the
// request did not fit the model's context window. Factored out of
// humanizeError so the recovery path and the message path cannot drift apart
// by consulting different marker lists.
func isContextOverflow(err error) bool {
	return err != nil && hasOverflowMarker(err.Error())
}

// hasOverflowMarker holds the single walk over contextOverflowMarkers. Both
// isContextOverflow and learnedWindowFrom go through it, so there is exactly
// one marker list and one matching rule in the package.
func hasOverflowMarker(msg string) bool {
	low := strings.ToLower(msg)
	for _, marker := range contextOverflowMarkers {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// learnedWindowFrom extracts the model's real context window from an overflow
// error, which is the one moment a backend reliably states it. The OpenAI
// /v1/models schema carries no context length and backends that expose one do
// so inconsistently, so this error is nib's only trustworthy source; inferring
// a window from a model name would be a guess presented as a fact.
//
// Two figures are required. tokenCountRe can match a single number, and with
// one number there is no way to tell whether it is the limit or the request
// size — treating a request size as the window would raise the compaction
// trigger above the real limit and suppress compaction exactly when it is most
// needed. Fewer than two figures teaches nothing.
//
// The window is the SMALLER figure regardless of the order the backend printed
// them, matching contextOverflowMessage.
//
// The whole unwrap chain is tried, not just err.Error(). humanizeError wraps
// the backend error in a FriendlyError whose own text drops the second figure
// — contextOverflowMessage prints "model allows 262144)" with no "tokens" for
// tokenCountRe to anchor on — so reading only the outermost message would
// learn nothing from a humanized error. Recovery runs before humanizeError
// today, but that ordering is one edit away from silently never learning a
// window again, and no test would catch it. Parsing every level makes the
// order irrelevant.
func learnedWindowFrom(err error) (int, bool) {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if n, ok := windowFromMessage(e.Error()); ok {
			return n, true
		}
	}
	return 0, false
}

// windowFromMessage applies the two-figure rule to one error message.
func windowFromMessage(msg string) (int, bool) {
	if !hasOverflowMarker(msg) {
		return 0, false
	}
	m := tokenCountRe.FindAllStringSubmatch(msg, -1)
	if len(m) < 2 {
		return 0, false
	}
	a, _ := strconv.Atoi(m[0][1])
	b, _ := strconv.Atoi(m[1][1])
	if b < a {
		a = b
	}
	if a <= 0 {
		return 0, false
	}
	return a, true
}

// contextOverflowMessage builds the user-facing text for a context-window
// overflow, folding in the token counts when the backend reported them. The
// larger count is the request size and the smaller is the model's limit,
// regardless of the order the backend printed them.
func contextOverflowMessage(raw string) string {
	detail := ""
	if m := tokenCountRe.FindAllStringSubmatch(raw, -1); len(m) >= 2 {
		needs, _ := strconv.Atoi(m[0][1])
		allows, _ := strconv.Atoi(m[1][1])
		if allows > needs {
			needs, allows = allows, needs
		}
		detail = fmt.Sprintf(" (needs ~%d tokens, model allows %d)", needs, allows)
	}
	return "the request is larger than the model's context window" + detail +
		". Increase the backend's context size, or reduce the enabled tools/MCP servers and clear the conversation (\"clear\"), then retry."
}
