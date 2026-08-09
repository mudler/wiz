package chat

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// contextOverflowMarkers are substrings different backends emit when a request
// exceeds the model's context window. The phrasing varies:
//   - llama.cpp / LocalAI: "request (9739 tokens) exceeds the available context size (8192 tokens)"
//   - OpenAI:              "This model's maximum context length is 8192 tokens. However, your messages resulted in 9739 tokens"
//   - vLLM:                "maximum context length is 8192 tokens"
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
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
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
func learnedWindowFrom(err error) (int, bool) {
	if !isContextOverflow(err) {
		return 0, false
	}
	m := tokenCountRe.FindAllStringSubmatch(err.Error(), -1)
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
