package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/chromedp"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mudler/nib/provenance"
	"github.com/mudler/nib/types"
	"github.com/mudler/xlog"
)

// browserActionTimeout bounds every single chromedp.Run sequence issued by a
// browser_* handler. Without it, a page whose body never becomes ready (or
// any other hung CDP action) blocks the tool call — and the model — until
// the whole session context is torn down. navigate/WaitReady is the main
// risk; 30s comfortably covers normal page loads without letting one call
// hang the session.
const browserActionTimeout = 30 * time.Second

type browserServer struct {
	cfg         types.BrowserConfig
	mu          sync.Mutex
	allocCancel context.CancelFunc
	ctxCancel   context.CancelFunc
	bctx        context.Context // the live browser context (nil until ensureBrowser)
	refs        map[string]int64

	// actionMu serializes the actual CDP action stream: struct fields above
	// are mutex-guarded independently, but chromedp.Run itself runs outside
	// any lock, so two concurrent tool calls could otherwise interleave CDP
	// commands on the same browser context. Held for the duration of each
	// handler's chromedp.Run sequence (acquired after the browser/live ctx is
	// already resolved, so it's never held across ensureBrowser).
	actionMu   sync.Mutex
	classifier provenance.Classifier
}

func newBrowserServer(cfg types.BrowserConfig) *browserServer {
	return &browserServer{cfg: cfg, refs: map[string]int64{}}
}

func (b *browserServer) profileDir() string {
	if b.cfg.ProfileDir != "" {
		return b.cfg.ProfileDir
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "dante-browser-profile")
}

// discoverChrome returns the Chrome/Chromium binary to drive: explicit path,
// else the first installed candidate for the platform, else "" (chromedp will
// then try its own default / a downloaded copy).
func discoverChrome(explicit string) string {
	if explicit != "" {
		return explicit
	}
	var cands []string
	switch runtime.GOOS {
	case "darwin":
		cands = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		}
	case "linux":
		cands = []string{"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser", "/usr/bin/brave-browser"}
	case "windows":
		cands = []string{`C:\Program Files\Google\Chrome\Application\chrome.exe`, `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`}
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// ensureBrowser lazily launches the headed Chrome on the dedicated persistent
// profile and returns the browser context. Reused across calls.
//
// Deliberately takes no ctx param: the browser is session-scoped — it must
// outlive any single request/tool call and is only torn down when
// StartBrowserMCPServer's session ctx is done (see the close() goroutine
// there). Parenting the allocator off a per-call ctx would tear the browser
// down as soon as that one call's ctx expired, which is wrong; parenting off
// a ctx we then never use would just be misleading. Per-call bounding is
// handled separately by browserActionTimeout on each handler's chromedp.Run.
func (b *browserServer) ensureBrowser() (context.Context, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bctx != nil {
		return b.bctx, nil
	}
	opts := append([]chromedp.ExecAllocatorOption{},
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.UserDataDir(b.profileDir()),
		chromedp.Flag("headless", false), // HEADED — visible + take-over-able
	)
	if p := discoverChrome(b.cfg.ChromePath); p != "" {
		opts = append(opts, chromedp.ExecPath(p))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	bctx, ctxCancel := chromedp.NewContext(allocCtx)
	// Kick the browser to actually start now (surfaces launch errors here).
	if err := chromedp.Run(bctx); err != nil {
		ctxCancel()
		allocCancel()
		return nil, err
	}
	b.allocCancel, b.ctxCancel, b.bctx = allocCancel, ctxCancel, bctx
	return b.bctx, nil
}

func (b *browserServer) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctxCancel != nil {
		b.ctxCancel()
	}
	if b.allocCancel != nil {
		b.allocCancel()
	}
	b.bctx = nil
}

// BrowserInput is the shared input shape for every browser_* tool. Only the
// fields relevant to a given tool are populated by the model for that call.
type BrowserInput struct {
	URL       string `json:"url,omitempty" jsonschema:"the URL to open (browser_navigate)"`
	Ref       string `json:"ref,omitempty" jsonschema:"element ref from the snapshot, e.g. @e5 (browser_click/browser_type)"`
	Text      string `json:"text,omitempty" jsonschema:"text to type (browser_type)"`
	Key       string `json:"key,omitempty" jsonschema:"key to press, e.g. Enter, Tab, Escape (browser_press)"`
	Direction string `json:"direction,omitempty" jsonschema:"up or down (browser_scroll)"`
	Full      bool   `json:"full,omitempty" jsonschema:"return the full page snapshot, not just interactive elements (browser_snapshot)"`
	Question  string `json:"question,omitempty" jsonschema:"what to look for in the screenshot (browser_vision)"`
}

// BrowserOutput is the structured result returned to the model for every
// browser_* tool that produces (or refreshes) a snapshot.
type BrowserOutput struct {
	Snapshot         string `json:"snapshot,omitempty"`
	ElementCount     int    `json:"element_count,omitempty"`
	SourceID         string `json:"source_id,omitempty"`
	RedactedFindings int    `json:"redacted_findings,omitempty"`
}

func (b *browserServer) protectedOutput(ctx context.Context, snapshot string, count int) BrowserOutput {
	if b.classifier == nil {
		return BrowserOutput{Snapshot: snapshot, ElementCount: count}
	}
	e := protectExternal(ctx, b.classifier, "browser", "active-page", snapshot)
	return BrowserOutput{Snapshot: e.ModelText(), ElementCount: count, SourceID: e.ID, RedactedFindings: len(e.Findings)}
}

// timeoutAwareError rewrites a context-deadline error from a per-call,
// browserActionTimeout-bounded chromedp.Run into the message the model
// should see and act on; any other error passes through unchanged.
func timeoutAwareError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("browser action timed out after %s; the page may still be loading — try browser_snapshot", browserActionTimeout)
	}
	return err
}

// snapshotAfterAction re-snapshots following a click/type/press/scroll. On
// failure it does NOT swallow the error into an empty-looking success: it
// returns a result that still reports the action succeeded but tells the
// model the follow-up snapshot failed, so it knows to call browser_snapshot
// itself rather than assume an empty page.
func snapshotAfterAction(b *browserServer, actx context.Context, verb string) (*mcp.CallToolResult, BrowserOutput) {
	text, n, err := b.snapshotNow(actx, true)
	if err != nil {
		return textResult(verb + "\npost-action snapshot failed; call browser_snapshot"), BrowserOutput{}
	}
	out := b.protectedOutput(actx, text, n)
	return textResult(verb + "\n" + out.Snapshot), out
}

// snapshotNow captures the current page's accessibility tree, renders it to
// the compact indented outline (buildSnapshot), and stores the resulting
// ref→backendDOMNodeID map on b.refs for subsequent browser_click/type calls.
func (b *browserServer) snapshotNow(ctx context.Context, compact bool) (string, int, error) {
	var raw []*accessibility.Node
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		raw, err = accessibility.GetFullAXTree().Do(ctx)
		return err
	})); err != nil {
		return "", 0, err
	}
	nodes := make([]axNode, 0, len(raw))
	for _, n := range raw {
		nn := axNode{NodeID: string(n.NodeID), BackendID: int64(n.BackendDOMNodeID), Ignored: n.Ignored}
		if n.Role != nil {
			nn.Role = trimQuotes(string(n.Role.Value))
		}
		if n.Name != nil {
			nn.Name = trimQuotes(string(n.Name.Value))
		}
		for _, c := range n.ChildIDs {
			nn.ChildIDs = append(nn.ChildIDs, string(c))
		}
		nodes = append(nodes, nn)
	}
	text, refs := buildSnapshot(nodes, compact)
	b.mu.Lock()
	b.refs = refs
	b.mu.Unlock()
	return text, len(refs), nil
}

// trimQuotes decodes the JSON-encoded AXValue.Value chromedp hands back for
// role/name (e.g. `"foo"`, or `"quote: \"hi\" —"` for names containing
// escapes/unicode). Falls back to a plain outer-quote strip if it isn't
// valid JSON, so a malformed/unexpected value still renders instead of
// erroring the whole snapshot.
func trimQuotes(s string) string {
	var out string
	if err := json.Unmarshal([]byte(s), &out); err == nil {
		return out
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// browserNavigate loads a URL in the (lazily-launched) headed browser and
// returns a fresh compact snapshot of the resulting page.
func (b *browserServer) browserNavigate(ctx context.Context, _ *mcp.CallToolRequest, in BrowserInput) (*mcp.CallToolResult, BrowserOutput, error) {
	url := strings.TrimSpace(in.URL)
	if url == "" {
		return nil, BrowserOutput{}, fmt.Errorf("browser_navigate needs a url")
	}
	if err := checkURLAllowed(url, b.cfg.AllowPrivateURLs); err != nil {
		return nil, BrowserOutput{}, err
	}
	bctx, err := b.ensureBrowser()
	if err != nil {
		return nil, BrowserOutput{}, fmt.Errorf("start browser: %w", err)
	}
	b.actionMu.Lock()
	defer b.actionMu.Unlock()
	actx, acancel := context.WithTimeout(bctx, browserActionTimeout)
	defer acancel()
	if err := chromedp.Run(actx, chromedp.Navigate(url), chromedp.WaitReady("body")); err != nil {
		return nil, BrowserOutput{}, timeoutAwareError(err)
	}
	text, n, err := b.snapshotNow(actx, true)
	if err != nil {
		return nil, BrowserOutput{}, timeoutAwareError(err)
	}
	out := b.protectedOutput(actx, text, n)
	return textResult(out.Snapshot), out, nil
}

// browserSnapshot re-reads the accessibility tree of the currently open page
// without navigating. in.Full selects the full outline vs. the default
// interactive-elements-only compact view.
func (b *browserServer) browserSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in BrowserInput) (*mcp.CallToolResult, BrowserOutput, error) {
	live, err := b.liveCtx()
	if err != nil {
		return nil, BrowserOutput{}, err
	}
	b.actionMu.Lock()
	defer b.actionMu.Unlock()
	actx, acancel := context.WithTimeout(live, browserActionTimeout)
	defer acancel()
	text, n, err := b.snapshotNow(actx, !in.Full)
	if err != nil {
		return nil, BrowserOutput{}, timeoutAwareError(err)
	}
	out := b.protectedOutput(actx, text, n)
	return textResult(out.Snapshot), out, nil
}

// browserClick resolves in.Ref against the last snapshot's ref map, clicks
// the corresponding DOM node, and returns a fresh snapshot (which also
// refreshes the ref map, since refs are only valid for one snapshot).
func (b *browserServer) browserClick(ctx context.Context, _ *mcp.CallToolRequest, in BrowserInput) (*mcp.CallToolResult, BrowserOutput, error) {
	live, backendID, err := b.liveRef(in.Ref)
	if err != nil {
		return nil, BrowserOutput{}, err
	}
	b.actionMu.Lock()
	defer b.actionMu.Unlock()
	actx, acancel := context.WithTimeout(live, browserActionTimeout)
	defer acancel()
	if err := b.clickBackendNode(actx, backendID); err != nil {
		return nil, BrowserOutput{}, timeoutAwareError(err)
	}
	// Re-snapshot so the model sees the result (and gets fresh refs).
	res, out := snapshotAfterAction(b, actx, "clicked "+in.Ref)
	return res, out, nil
}

// browserType resolves in.Ref against the last snapshot's ref map, focuses +
// clears the corresponding DOM node and types in.Text via a real CDP input
// event, then returns a fresh snapshot (which also refreshes the ref map,
// since refs are only valid for one snapshot).
func (b *browserServer) browserType(ctx context.Context, _ *mcp.CallToolRequest, in BrowserInput) (*mcp.CallToolResult, BrowserOutput, error) {
	if in.Text == "" {
		return nil, BrowserOutput{}, fmt.Errorf("browser_type needs text")
	}
	live, backendID, err := b.liveRef(in.Ref)
	if err != nil {
		return nil, BrowserOutput{}, err
	}
	b.actionMu.Lock()
	defer b.actionMu.Unlock()
	actx, acancel := context.WithTimeout(live, browserActionTimeout)
	defer acancel()
	if err := b.typeIntoBackendNode(actx, backendID, in.Text); err != nil {
		return nil, BrowserOutput{}, timeoutAwareError(err)
	}
	// Re-snapshot so the model sees the result (and gets fresh refs).
	res, out := snapshotAfterAction(b, actx, "typed into "+in.Ref)
	return res, out, nil
}

// browserPress sends a single named key event (Enter, Tab, Escape, an arrow,
// etc.) to whatever currently has focus on the page — there's no ref, unlike
// click/type. Re-snapshots after, since Enter often submits a form or
// navigates.
func (b *browserServer) browserPress(ctx context.Context, _ *mcp.CallToolRequest, in BrowserInput) (*mcp.CallToolResult, BrowserOutput, error) {
	key, err := keyForName(in.Key)
	if err != nil {
		return nil, BrowserOutput{}, err
	}
	live, err := b.liveCtx()
	if err != nil {
		return nil, BrowserOutput{}, err
	}
	b.actionMu.Lock()
	defer b.actionMu.Unlock()
	actx, acancel := context.WithTimeout(live, browserActionTimeout)
	defer acancel()
	// Capture the URL BEFORE the key so settleAfterKey can tell a navigating key
	// (Enter submitting a form) apart from the stale pre-navigation page, which
	// is still readyState=="complete" at the instant the key dispatch returns.
	var beforeURL string
	_ = chromedp.Run(actx, chromedp.Evaluate("location.href", &beforeURL))
	if err := chromedp.Run(actx, chromedp.KeyEvent(key)); err != nil {
		return nil, BrowserOutput{}, timeoutAwareError(err)
	}
	// Enter (and occasionally other keys) can trigger a native navigation;
	// give it a brief, bounded chance to actually happen before we
	// re-snapshot, or the snapshot risks describing stale pre-navigation DOM.
	settleAfterKey(actx, beforeURL)
	// Enter's default action in a focused form field is to submit the owning
	// form. A *synthesized* Enter fires that default only unreliably when the
	// tab is not foreground — verified against a real Chrome, submits ranged
	// 0–3 of 6 across focus/bringToFront tricks, versus 8 of 8 with an explicit
	// submit. So when Enter did not itself navigate, perform the implicit
	// submission deterministically, exactly as a real Enter would: submit the
	// focused element's form via requestSubmit (which still fires the submit
	// event and runs constraint validation, and an onsubmit preventDefault still
	// wins). The KeyEvent above already delivered keydown to any JS handler, so
	// JS-driven Enter still works — this only restores the native default action
	// the synthetic key dropped. Then settle again for the resulting navigation.
	if in.Key == "Enter" {
		var afterURL string
		_ = chromedp.Run(actx, chromedp.Evaluate("location.href", &afterURL))
		if afterURL == beforeURL {
			var submitted string
			if err := chromedp.Run(actx, chromedp.Evaluate(browserImplicitSubmitJS, &submitted)); err == nil && submitted == "submitted" {
				settleAfterKey(actx, beforeURL)
			}
		}
	}
	// Re-snapshot so the model sees the result (and gets fresh refs).
	res, out := snapshotAfterAction(b, actx, "pressed "+in.Key)
	return res, out, nil
}

// browserScroll scrolls the viewport up or down ~90% of its height — there's
// no ref; it acts on the whole page, not an element. Re-snapshots after,
// since scrolling reveals new content.
func (b *browserServer) browserScroll(ctx context.Context, _ *mcp.CallToolRequest, in BrowserInput) (*mcp.CallToolResult, BrowserOutput, error) {
	delta, err := scrollDelta(in.Direction)
	if err != nil {
		return nil, BrowserOutput{}, err
	}
	live, err := b.liveCtx()
	if err != nil {
		return nil, BrowserOutput{}, err
	}
	b.actionMu.Lock()
	defer b.actionMu.Unlock()
	actx, acancel := context.WithTimeout(live, browserActionTimeout)
	defer acancel()
	if err := chromedp.Run(actx, chromedp.Evaluate("window.scrollBy(0, "+delta+")", nil)); err != nil {
		return nil, BrowserOutput{}, timeoutAwareError(err)
	}
	// Re-snapshot so the model sees the result (and gets fresh refs).
	res, out := snapshotAfterAction(b, actx, "scrolled "+in.Direction)
	return res, out, nil
}

// browserVision captures a screenshot of the current page's viewport and
// returns it to the model as image content, alongside a short text label
// naming what the model was asked to look for. Unlike the other browser_*
// tools it does not refresh/return a snapshot — it's meant for the cases a
// text accessibility snapshot can't cover (CAPTCHAs, visual verification,
// complex layouts).
func (b *browserServer) browserVision(ctx context.Context, _ *mcp.CallToolRequest, in BrowserInput) (*mcp.CallToolResult, BrowserOutput, error) {
	live, err := b.liveCtx()
	if err != nil {
		return nil, BrowserOutput{}, err
	}
	b.actionMu.Lock()
	defer b.actionMu.Unlock()
	actx, acancel := context.WithTimeout(live, browserActionTimeout)
	defer acancel()
	var buf []byte
	if err := chromedp.Run(actx, chromedp.CaptureScreenshot(&buf)); err != nil {
		return nil, BrowserOutput{}, timeoutAwareError(err)
	}
	result := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "screenshot for: " + in.Question},
		&mcp.ImageContent{MIMEType: "image/png", Data: buf},
	}}
	return result, BrowserOutput{}, nil
}

// liveCtx returns the live browser ctx for actions that don't target a
// specific ref (browser_press, browser_scroll), erroring if no page is open.
func (b *browserServer) liveCtx() (context.Context, error) {
	b.mu.Lock()
	live := b.bctx
	b.mu.Unlock()
	if live == nil {
		return nil, fmt.Errorf("no page open — call browser_navigate first")
	}
	return live, nil
}

// liveRef returns the live browser ctx + the ref's backend id, erroring if no
// page is open or the ref is stale.
func (b *browserServer) liveRef(ref string) (context.Context, int64, error) {
	b.mu.Lock()
	live := b.bctx
	b.mu.Unlock()
	if live == nil {
		return nil, 0, fmt.Errorf("no page open — call browser_navigate first")
	}
	id, err := b.resolveRef(ref)
	return live, id, err
}

// textResult wraps a plain-text tool response in the MCP content shape.
func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// browserInputSchema builds the shared browser_* input schema from
// BrowserInput (keeping the per-field descriptions from the jsonschema tags)
// and stamps real enums onto the closed-value fields, direction and key —
// mirrors computerInputSchema. Called fresh per tool registration (see
// StartBrowserMCPServer) rather than shared, so the go-sdk can't
// alias-mutate one schema instance across tools.
func browserInputSchema() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[BrowserInput](nil)
	if err != nil {
		return nil, err
	}
	if p := s.Properties["direction"]; p != nil {
		p.Enum = []any{"up", "down"}
	}
	if p := s.Properties["key"]; p != nil {
		keys := make([]string, 0, len(pressAllowedKeys))
		for k := range pressAllowedKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		enum := make([]any, len(keys))
		for i, k := range keys {
			enum[i] = k
		}
		p.Enum = enum
	}
	return s, nil
}

// StartBrowserMCPServer starts a headed-Chrome-backed MCP server exposing the
// browser_* tools (navigate, snapshot, click, type, press, scroll, vision).
// Blocks until ctx is done, at which point the browser is torn down.
func StartBrowserMCPServer(ctx context.Context, transport mcp.Transport, cfg types.Config) error {
	bs := newBrowserServer(cfg.Browser)
	classifier, err := provenance.ClassifierForConfig(cfg)
	if err != nil {
		return err
	}
	bs.classifier = classifier
	go func() {
		<-ctx.Done()
		bs.close()
	}()

	server := mcp.NewServer(&mcp.Implementation{Name: "browser", Version: "v1.0.0"}, nil)

	addBrowserTool := func(name, description string, handler mcp.ToolHandlerFor[BrowserInput, BrowserOutput]) {
		tool := &mcp.Tool{Name: name, Description: description}
		// Build a fresh schema per tool registration — sharing one
		// *jsonschema.Schema pointer across tools risks the go-sdk (or a
		// caller) mutating one tool's schema and silently affecting all of
		// them.
		if schema, err := browserInputSchema(); err != nil {
			xlog.Warn("browser: could not build enum schema, falling back to inferred", "tool", name, "err", err)
		} else {
			tool.InputSchema = schema
		}
		mcp.AddTool(server, tool, handler)
	}

	addBrowserTool("browser_navigate",
		"Open a URL in a real, headed Chrome browser. ALWAYS call this first for any web task — it loads "+
			"the page and returns a snapshot of its interactive elements, each tagged with a ref like @e3. "+
			"For read-only lookups (answering a question, fetching a page's text) prefer web_search/web_fetch "+
			"instead — this tool is for tasks that require actually driving a page (clicking, typing, logging in).",
		bs.browserNavigate)
	addBrowserTool("browser_snapshot",
		"Re-read the accessibility tree of the currently open page without navigating, returning fresh @eN "+
			"refs for browser_click/browser_type. Use after an action whose result isn't otherwise clear, or "+
			"when refs may be stale. Set full=true for the whole-page outline instead of just interactive elements.",
		bs.browserSnapshot)
	addBrowserTool("browser_click",
		"Click the element identified by ref (an @eN id from the last browser_navigate/browser_snapshot/"+
			"browser_click/browser_type/browser_press/browser_scroll result). Returns a fresh snapshot.",
		bs.browserClick)
	addBrowserTool("browser_type",
		"Focus the element identified by ref (an @eN id from the last snapshot), clear it, and type text into "+
			"it. Returns a fresh snapshot. Use browser_press with key=Enter afterward to submit, if needed.",
		bs.browserType)
	addBrowserTool("browser_press",
		"Send a single named key (Enter, Tab, Escape, ArrowDown, PageUp, etc.) to whatever currently has focus "+
			"on the page — there is no ref. Returns a fresh snapshot. Useful to submit a form after browser_type.",
		bs.browserPress)
	addBrowserTool("browser_scroll",
		"Scroll the page viewport up or down by about 90% of its height. direction must be \"up\" or \"down\". "+
			"Returns a fresh snapshot revealing the newly visible content.",
		bs.browserScroll)
	addBrowserTool("browser_vision",
		"Take a screenshot of the current page for visual inspection — use when the accessibility snapshot "+
			"isn't enough (CAPTCHAs, visual verification, complex layouts). question says what to look for.",
		bs.browserVision)

	xlog.Info("browser MCP server ready")
	return server.Run(ctx, transport)
}
