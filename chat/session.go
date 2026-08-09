package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mudler/nib/hooks"
	"github.com/mudler/nib/manage"
	wizmcp "github.com/mudler/nib/mcp"
	"github.com/mudler/nib/specialist"
	"github.com/mudler/nib/trace"
	"github.com/mudler/nib/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mudler/cogito"
	"github.com/mudler/cogito/clients"
	"github.com/mudler/xlog"
	openai "github.com/sashabaranov/go-openai"
)

// Session represents a chat session with the AI assistant
type Session struct {
	ctx          context.Context
	turnMu       sync.Mutex
	turnCancel   context.CancelFunc
	llm          cogito.LLM // guarded by modelMu
	clients      []*mcp.ClientSession
	mcpClient    *mcp.Client
	cfgClients   map[string]*mcp.ClientSession // config/plugin MCP servers, by name
	cfgServers   map[string]types.MCPServer    // desired set, for diffing
	skillsClient *mcp.ClientSession            // the load_skill server
	// historyMu guards the parallel conversation state — fragment (the real
	// model context handed to cogito) and messages (the {role,content} log
	// surfaced to the UI). SendMessage mutates both as a turn progresses; the
	// lock lets ExportHistory take a consistent copy from another goroutine
	// while a turn is running.
	historyMu           sync.Mutex
	fragment            cogito.Fragment
	messages            []openai.ChatCompletionMessage
	callbacks           Callbacks
	systemPrompt        string
	loadedSkills        string // eager-loaded /skill instructions, re-applied across reloads
	skills              []types.Skill
	cogitoOptions       types.AgentOptions
	compaction          types.CompactionConfig
	allowedTools        map[string]bool  // Tools that don't need approval this session
	toolAllow           map[string]bool  // if non-empty, the only built-in tools exposed to the model
	allowedBashPrefixes map[string]bool  // bash first-word grants ("git" → simple `git …` auto-approved)
	autoApprove         bool             // approval_mode: auto — approve every tool call
	allowAllTurn        bool             // user chose "allow all this turn"; reset each top-level turn
	approvalMode        string           // raw approval_mode: "" / "prompt" / "strict" / "allowlist" / "auto"
	readOnlyCommands    readOnlyCommands // bash commands auto-approved in prompt mode
	hooks               *hooks.Dispatcher

	agentMu    sync.Mutex
	agentStart map[string]time.Time // sub-agent ID -> spawn time, for elapsed

	// inject is the per-session message-injection channel handed to cogito. While
	// a run parks (background work pending, or simply waiting for the user), the
	// loop blocks on this channel; sending a message wakes it and continues the
	// SAME run. Shell-job completions, scheduled wake-ups, and mid-run user
	// follow-ups all flow through here. Buffered so non-blocking sends rarely drop.
	inject chan openai.ChatCompletionMessage
	// runLive is set while ExecuteTools is in flight (between SendMessage start
	// and return), so Inject can tell whether there is a live run to inject into.
	runMu   sync.Mutex
	runLive bool
	// userInjected tracks texts delivered via InjectUser during the current
	// run; undelivered collects the ones still sitting in the injection
	// channel when the run returned — the model never saw them, so the
	// end-of-run drain hands them back via TakeUndelivered instead of
	// discarding them. Both guarded by runMu.
	userInjected []string
	undelivered  []string

	// goal is the active session goal (the /goal stop-gate). While non-empty,
	// SendMessage re-runs the turn until the model calls goal_done. goalDone is
	// set by that tool within a run. Both guarded by runMu.
	goal     string
	goalDone bool

	// shellJobs lets the pending-work predicate keep the run parked while a
	// backgrounded shell command is still running (cogito only knows about
	// sub-agents). May be nil (e.g. headless CLI without a job registry).
	shellJobs *wizmcp.ShellJobs

	agentManager *cogito.AgentManager
	agentDefs    []cogito.AgentDefinition
	agentModels  map[string]bool // models configured per agent type (for the LLM-model guard)
	agentLogs    *agentLogStore  // per-sub-agent activity log (for the agent_logs tool)

	// modelMu guards the (llm, llmModel) pair, which SetModel replaces together
	// when the user switches model mid-session. Not turnMu: that one is held
	// only across beginTurn/endTurn/Interrupt (it guards turnCancel, not the
	// turn), so taking it here would order nothing against a running request.
	// Readers snapshot the pair through currentLLM/Model, which is what keeps a
	// turn on one client from start to finish while the switch applies from the
	// next one. The rest of the endpoint state below (apiKey, baseURL,
	// metadata, reasoningEffort) is fixed at construction and read lock-free.
	modelMu  sync.RWMutex
	llmModel string // guarded by modelMu

	// learnedWindow is the context window a backend stated in an overflow
	// error, and learnedWindowModel is the model it was learned for. They are
	// guarded by modelMu because they are only ever meaningful as a pair with
	// llmModel, and reading them under a different lock would let a model
	// switch land between the two reads.
	learnedWindow      int
	learnedWindowModel string
	apiKey             string
	baseURL            string
	transcribeModel    string
	visionModel        string
	videoModel         string
	workingDir         string
	metadata           map[string]string // global per-request metadata; merged with per-agent overrides
	reasoningEffort    string            // OpenAI reasoning_effort sent on every request (e.g. "none")

	configurator  *manage.Configurator
	reloadMu      sync.Mutex
	pendingReload bool

	// computerEnabled records whether desktop control (computer_use) was armed
	// for this session. It gates cogito's Phase-A tool-image forwarding so
	// screenshots are only fed back to the model when a computer transport is
	// actually registered. Fixed at construction (Computer config is runtime-only).
	computerEnabled bool

	// prefixWarm records whether this session has actually issued a request that
	// prefilled its prompt prefix (system prompt + tool schemas) on the server.
	// On CPU hardware that prefill costs tens of seconds on the first request and
	// nothing afterwards, so a UI reads this to label the cold turn rather than
	// leaving the user staring at a silent minute.
	//
	// An atomic rather than a mutex-guarded bool on purpose: PrefixWarm is read
	// from the UI goroutine while a turn holds runMu/historyMu, and Warm
	// deliberately avoids holding runMu across its network call.
	prefixWarm atomic.Bool

	// usage is this session's running token total. See chat/usage.go for why it
	// carries its own lock rather than sharing historyMu.
	usage sessionUsage

	// prunedMu guards the tool-output pruning state below. The manipulator reads
	// it from inside cogito's loop, and nothing here should assume which
	// goroutine that is; Reload writes the policy from the turn goroutine.
	prunedMu sync.Mutex
	// pruning is the tool-output pruning policy for this session.
	pruning types.ToolOutputPruningConfig
	// prunedIDs maps each tool_call_id already replaced by a stub to the clause
	// that stub carries. It is what makes pruning monotonic across calls, and it
	// is why a prune notice fires on a transition rather than on every LLM call.
	//
	// The clause is stored rather than re-derived because the reason a result
	// was dropped can change under the policy's feet — a result swept for budget
	// becomes a stale read as soon as the model edits the file it had read — and
	// a stub whose wording changed between calls would move the prompt prefix
	// just as un-stubbing it would.
	prunedIDs map[string]string

	// overflowRetried counts context-overflow recoveries in the CURRENT turn.
	// It exists so a retry can never become a loop, and so tests can assert on
	// attempts rather than on a raw completion-call count that cogito's own
	// internal retries make unstable.
	overflowMu      sync.Mutex
	overflowRetried int

	tracer *trace.Recorder // non-nil when session tracing is enabled

	// traceDir is where usage.json is written on Close. Empty = tracing off, so
	// an untraced session leaves nothing behind. Kept separate from tracer
	// because usage.json is written beside the transcript rather than through
	// the recorder: it shares no state with it, and holding the directory here
	// keeps the report reachable independently of the recorder's lifetime.
	traceDir string
}

// PrefixWarm reports whether this session has already issued a request that
// SHOULD have prefilled its prompt prefix on the server — not that the server's
// cache still holds it. A LocalAI restart or a KV eviction leaves this
// stale-true. That direction is deliberate: a stale true costs a missing
// "preparing" label, while a false negative would only cost a redundant one, so
// callers should treat it as a hint for labelling and never as a guarantee.
//
// It is set only where the model was genuinely reached — a completed
// SendMessage turn or a successful Warm — and never by the paths that bail
// before that (an attachments failure, or an all-blocked attachment set with no
// text). It is safe to call from another goroutine while a turn is running.
//
// SetModel resets it to false: unlike a restart or an eviction, a switch is a
// cold prefix the session knows about. Nothing else resets it, and a rebuilt
// session is a new *Session that starts false.
func (s *Session) PrefixWarm() bool {
	return s.prefixWarm.Load()
}

// AgentLog returns the captured activity log for a sub-agent (for the
// agent_logs tool / UI inspection).
func (s *Session) AgentLog(agentID string) string {
	if s.agentLogs == nil {
		return ""
	}
	return s.agentLogs.dump(agentID)
}

// resolveAgentModel picks the model for a sub-agent: the requested model when
// wiz actually serves it (the main model, or one configured for an agent type),
// otherwise the main model. This honors per-agent model overrides from config
// while ignoring model names the LLM invents via the spawn_agent `model` arg.
func resolveAgentModel(requested, main string, configured map[string]bool) string {
	if requested != "" && (requested == main || configured[requested]) {
		return requested
	}
	return main
}

// newAgentLLM builds the LLM client for a spawned sub-agent. mainModel is the
// session model this turn runs against; requested is what the spawn_agent tool
// asked for, which the LLM may fill with a name the endpoint doesn't serve
// (e.g. "sonar") and 404 the sub-agent. Honor a requested model only when wiz
// actually serves it (the main model, or one configured for an agent type),
// otherwise fall back to the main model. This keeps per-agent model overrides
// from config working while ignoring invented names.
func (s *Session) newAgentLLM(mainModel, requested string, temperature float32, metadata map[string]string) cogito.LLM {
	chosen := resolveAgentModel(requested, mainModel, s.agentModels)
	if requested != "" && chosen != requested {
		xlog.Warn("sub-agent requested an unserved model; using the main model",
			"requested", requested, "model", chosen)
	}
	// metadata is this agent type's override; overlay it on the global
	// session metadata (per-key: agent wins, global-only keys inherited).
	agentLLM := clients.NewLocalAILLM(chosen, s.apiKey, s.baseURL)
	agentLLM.SetTemperature(temperature)
	agentLLM.SetMetadata(mergeMetadata(s.metadata, metadata))
	agentLLM.SetReasoningEffort(s.reasoningEffort)
	return agentLLM
}

// mergeMetadata overlays per-agent metadata on top of the global metadata,
// returning a new map (or nil when both are empty). Per-agent keys win; keys
// present only in the global map are inherited. Inputs are never mutated.
func mergeMetadata(global, override map[string]string) map[string]string {
	if len(global) == 0 && len(override) == 0 {
		return nil
	}
	merged := make(map[string]string, len(global)+len(override))
	for k, v := range global {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// agentModelSet collects the non-empty per-agent-type model overrides so the
// agent LLM factory can tell a configured model apart from an invented one.
func agentModelSet(defs []cogito.AgentDefinition) map[string]bool {
	m := make(map[string]bool, len(defs))
	for _, d := range defs {
		if d.Model != "" {
			m[d.Model] = true
		}
	}
	return m
}

// toCogitoDefinitions converts wiz agent-type config into cogito definitions.
func toCogitoDefinitions(cfgs []types.AgentTypeConfig) []cogito.AgentDefinition {
	defs := make([]cogito.AgentDefinition, 0, len(cfgs))
	for _, t := range cfgs {
		defs = append(defs, cogito.AgentDefinition{
			Name:         t.Name,
			Description:  t.Description,
			SystemPrompt: t.SystemPrompt,
			Tools:        t.Tools,
			Model:        t.Model,
			Temperature:  t.Temperature,
			Metadata:     t.Metadata,
			Iterations:   t.Iterations,
			MaxAttempts:  t.MaxAttempts,
			MaxRetries:   t.MaxRetries,
		})
	}
	return defs
}

// NewSession creates a new chat session.
//
// If cfg.TraceDir is set and the recorder cannot be opened, NewSession fails
// rather than returning a session that quietly records nothing — a caller that
// asked for a trace should not have to discover later that it never got one.
// app.Run preflights the directory, so in practice only embedders calling this
// directly reach the failure.
func NewSession(ctx context.Context, cfg types.Config, callbacks Callbacks, transports ...mcp.Transport) (*Session, error) {
	// LocalAIClient, not OpenAIClient: LocalAI (and vLLM) put reasoning/thinking
	// text in a "reasoning" response field, which go-openai's SDK — and so
	// OpenAIClient — doesn't know about (it only binds the older, now-deprecated
	// "reasoning_content" key). LocalAIClient parses both.
	llmClient := clients.NewLocalAILLM(cfg.Model, cfg.APIKey, cfg.BaseURL)
	llmClient.SetMetadata(cfg.Metadata)
	llmClient.SetReasoningEffort(cfg.ReasoningEffort)
	var llm cogito.LLM = llmClient

	// Session tracing: wrap the LLM so every call is appended to the transcript.
	// Tracing is only ever on because someone asked for it, so a recorder that
	// cannot open is a failure rather than a downgrade. This used to be an
	// xlog.Warn, which the default "error" log level discarded — the session
	// then ran untraced and said nothing, which is exactly what was reported.
	// app.Run preflights this so the usual failure never reaches here; what is
	// left is the dir vanishing between check and open, and embedders calling
	// NewSession directly.
	var tracer *trace.Recorder
	if cfg.TraceDir != "" {
		rec, err := trace.NewRecorder(cfg.TraceDir)
		if err != nil {
			return nil, fmt.Errorf("cannot write trace to %s: %w", cfg.TraceDir, err)
		}
		tracer = rec
		llm = trace.NewRecordingLLM(llm, rec, cfg.Model, "")
	}

	agentManager := cogito.NewAgentManager()

	client := mcp.NewClient(&mcp.Implementation{Name: "aish", Version: "v1.0.0"}, nil)
	clients := []*mcp.ClientSession{}

	for _, transport := range transports {
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			// A single MCP server that fails to start (e.g. a plugin whose
			// binary isn't on PATH) must not prevent the whole session from
			// coming up. Skip it and continue with the rest.
			xlog.Warn("Skipping MCP server that failed to connect", "error", err)
			continue
		}
		clients = append(clients, session)
	}

	s := &Session{
		ctx:                 ctx,
		llm:                 llm,
		clients:             clients,
		fragment:            cogito.NewEmptyFragment(),
		messages:            []openai.ChatCompletionMessage{},
		callbacks:           callbacks,
		inject:              make(chan openai.ChatCompletionMessage, 16),
		cogitoOptions:       cfg.AgentOptions,
		compaction:          cfg.Compaction,
		pruning:             cfg.ToolOutputPruning,
		allowedTools:        make(map[string]bool),
		toolAllow:           make(map[string]bool),
		allowedBashPrefixes: make(map[string]bool),
		agentStart:          make(map[string]time.Time),
		agentManager:        agentManager,
		agentLogs:           newAgentLogStore(),
		llmModel:            cfg.Model,
		apiKey:              cfg.APIKey,
		baseURL:             cfg.BaseURL,
		transcribeModel:     cfg.TranscribeModel,
		visionModel:         cfg.VisionModel,
		videoModel:          cfg.VideoModel,
		workingDir:          cfg.WorkingDir,
		metadata:            cfg.Metadata,
		reasoningEffort:     cfg.ReasoningEffort,
		mcpClient:           client,
		computerEnabled:     cfg.Computer.Enabled,
		cfgClients:          map[string]*mcp.ClientSession{},
		cfgServers:          map[string]types.MCPServer{},
		configurator:        manage.NewIn(cfg.BaseDir),
		tracer:              tracer,
		traceDir:            cfg.TraceDir,
	}
	// Resume/rehydration: seed a prior conversation so the very next SendMessage
	// continues with full memory of it, behaving identically to a session that
	// organically reached this state. We seed BOTH parallel fields:
	//   - s.messages: the {role,content} log the UI reads back.
	//   - s.fragment: the real model context handed to cogito on the next turn.
	// The seeded history MUST NOT contain the system prompt (ExportHistory never
	// emits one, and Config documents the contract). SendMessage prepends the
	// current system prompt at the START of every turn via
	// s.fragment.AddMessage("system", …) — so we deliberately seed the fragment
	// WITHOUT a system message and let that per-turn add supply exactly one. That
	// is what makes a resumed turn structurally identical to a fresh multi-turn
	// session (whose fragment likewise carries no leading system message before
	// the turn's own add), and it avoids a duplicated or missing system prompt on
	// the first resumed turn. Token counters reset to zero and are recomputed from
	// the first resumed request's usage, which reflects the full seeded context.
	if len(cfg.InitialHistory) > 0 {
		seed := slices.Clone(cfg.InitialHistory)
		s.messages = seed
		s.fragment = cogito.NewFragment(seed...)
	}
	for _, name := range cfg.AllowedTools {
		s.allowedTools[name] = true
	}
	for _, name := range cfg.BuiltinTools {
		s.toolAllow[name] = true
	}
	s.autoApprove = cfg.ApprovalMode == "auto"
	s.approvalMode = cfg.ApprovalMode
	s.readOnlyCommands = newReadOnlyCommands(cfg.ReadOnlyCommands)
	// Wire reloadable state (skills server, config MCP clients, agents, hooks,
	// system prompt) through the same path used for live reloads.
	if err := s.Reload(cfg); err != nil {
		xlog.Warn("self-config: initial reload", "error", err)
	}
	s.hooks.Fire(ctx, hooks.EventSessionStart, "", map[string]any{"event": "SessionStart"})
	return s, nil
}

// LoadSkill appends a named skill's instructions to the session system prompt
// (eager load via /skill), so subsequent turns include it without a load_skill
// tool call. Returns a short notice for the transcript.
func (s *Session) LoadSkill(name string) (string, error) {
	for _, sk := range s.skills {
		if sk.Name == name {
			suffix := "\n\n# Skill: " + sk.Name + "\n" + sk.Instructions
			s.loadedSkills += suffix
			s.systemPrompt += suffix
			return fmt.Sprintf("Loaded skill %q: %s", sk.Name, sk.Description), nil
		}
	}
	return "", fmt.Errorf("unknown skill %q", name)
}

// decideToolCall resolves a tool-call request: PreToolUse hooks first (a hook
// may block/approve/adjust), then the session allow-list, then the user gate.
// emitSubAgentToolLine surfaces a sub-agent's tool call as a compact inline
// thread line via OnToolResult, from the tool-CALL callback where the AgentID is
// reliable. cogito propagates the tool-call callback into spawned sub-agents
// (with state.AgentID set) but NOT the tool-result callback, so OnToolResult
// never fires for a sub-agent's tools on its own — keying the thread off the
// result callback would show nothing. It is a no-op for the root agent
// (agentID == "", whose tools stream with their output via the result
// callback), for denied calls, and when no callback is registered.
func (s *Session) emitSubAgentToolLine(approved bool, agentID, name, args string) {
	if !approved || agentID == "" || s.callbacks.OnToolResult == nil {
		return
	}
	s.callbacks.OnToolResult(ToolResult{Name: name, Arguments: args, AgentID: agentID})
}

func (s *Session) decideToolCall(req ToolCallRequest) cogito.ToolCallDecision {
	if s.hooks != nil {
		decisions := s.hooks.Fire(s.ctx, hooks.EventPreToolUse, req.Name, map[string]any{
			"event":     "PreToolUse",
			"tool":      req.Name,
			"arguments": req.Arguments,
			"reasoning": req.Reasoning,
			"agent_id":  req.AgentID,
		})
		if td := hooks.CombineToolDecisions(decisions); td.Decided {
			adjustment := td.Adjustment
			if !td.Approve && adjustment == "" {
				adjustment = td.Reason
			}
			return cogito.ToolCallDecision{Approved: td.Approve, Adjustment: adjustment}
		}
	}

	if s.autoApprove || s.allowAllTurn {
		return cogito.ToolCallDecision{Approved: true}
	}
	if s.allowedTools[req.Name] {
		return cogito.ToolCallDecision{Approved: true}
	}
	if req.Name == "bash" {
		if p, ok := BashGrantPrefix(req.Arguments); ok && s.allowedBashPrefixes[p] {
			return cogito.ToolCallDecision{Approved: true}
		}
	}
	// In the default prompt mode, auto-approve calls that only observe state.
	// Not applied in allowlist (explicitly restrictive), strict (prompt for
	// everything), or auto (already approved above). Hooks above still win.
	if (s.approvalMode == "" || s.approvalMode == "prompt") &&
		IsReadOnly(req.Name, req.Arguments, s.readOnlyCommands) {
		return cogito.ToolCallDecision{Approved: true}
	}
	if s.callbacks.OnToolCall == nil {
		return cogito.ToolCallDecision{Approved: true}
	}
	resp := s.callbacks.OnToolCall(req)
	if resp.Approved && resp.AllowAllTurn {
		s.allowAllTurn = true
	}
	if resp.Approved && resp.AlwaysAllow {
		switch {
		case resp.AlwaysPrefix == "":
			s.allowedTools[req.Name] = true
		case req.Name == "bash":
			// Mint a prefix grant only when the approved request itself
			// derives that prefix — the response string is presentation-only
			// and cannot widen the grant. A non-bash request or a mismatched
			// prefix mints nothing at all: the call stays approved, but we
			// deliberately do not fall back to a whole-tool grant the user
			// never saw offered.
			if p, ok := BashGrantPrefix(req.Arguments); ok && p == resp.AlwaysPrefix {
				if s.allowedBashPrefixes == nil {
					s.allowedBashPrefixes = make(map[string]bool)
				}
				s.allowedBashPrefixes[p] = true
			}
		}
	}
	return cogito.ToolCallDecision{Approved: resp.Approved, Adjustment: resp.Adjustment}
}

// ToolCallDenied reports whether the given tool call would be denied (used to
// verify PreToolUse hook gating end-to-end).
func (s *Session) ToolCallDenied(req ToolCallRequest) bool {
	return !s.decideToolCall(req).Approved
}

// AgentManager exposes the sub-agent registry so the UI can list and detach agents.
func (s *Session) AgentManager() *cogito.AgentManager {
	return s.agentManager
}

// KillAgent cancels a running sub-agent by id (its context is cancelled, which
// stops the agent and any LLM call it has in flight). Returns false when the id
// is unknown. Safe to call on an already-finished agent.
func (s *Session) KillAgent(id string) bool {
	if s.agentManager == nil {
		return false
	}
	a, ok := s.agentManager.Get(id)
	if !ok {
		return false
	}
	if a.Cancel != nil {
		a.Cancel()
	}
	return true
}

// emitAgentEvent maps a cogito sub-agent state into a chat.AgentEvent and
// forwards it to the registered OnAgentEvent callback (if any). It is shared by
// the spawn (Status=running) and completion callbacks so the mapping lives in
// one place. s.callbacks.OnAgentEvent is set once in NewSession and never
// reassigned, so reading it from cogito's spawn goroutines is safe.
func (s *Session) emitAgentEvent(a *cogito.AgentState) {
	ev := AgentEvent{
		ID:     a.ID,
		Type:   a.Type,
		Task:   a.Task,
		Status: AgentStatus(a.Status),
		Result: a.Result,
		Err:    a.Error,
	}
	switch ev.Status {
	case AgentStatusRunning:
		s.agentMu.Lock()
		if s.agentStart == nil {
			s.agentStart = make(map[string]time.Time)
		}
		s.agentStart[a.ID] = time.Now()
		s.agentMu.Unlock()
	case AgentStatusCompleted, AgentStatusFailed:
		var au cogito.LLMUsage
		ev.ToolCount, au = agentUsageFull(a)
		ev.TotalTokens = au.TotalTokens
		// Sub-agent tokens are spent on the same bill as the main loop, so the
		// session total owns them too.
		//
		// Counting here rather than in the main loop is what makes this correct
		// exactly once: cogito hands sub-agents the UNWRAPPED parent LLM (see
		// prepareAgentTools, which captures agentLLM before ExecuteTools wraps
		// it in a counting LLM), so a sub-agent's spend never lands in the
		// parent run's CumulativeUsage that SendMessage adds. Without this the
		// tokens are invisible; adding it in both places would double them.
		//
		// The failed branch is counted deliberately, but today it contributes
		// exactly zero, and this is the one place that is easy to misread:
		// cogito assigns agent.Fragment only on the success branch and drops
		// the fragment on error, so for a real failure agentUsageFull reads
		// through a nil fragment and returns an empty LLMUsage (which is why
		// its doc says a failed agent never gets one). The call stays because
		// the intent is honest — tokens burned before a failure were still
		// billed — and it starts reporting the moment cogito preserves a failed
		// agent's fragment, with no change needed here. Until then a failed
		// sub-agent's spend is under-reported, which is the safe direction for
		// a figure a benchmark harness reads: too low never invents spend.
		s.addUsage(au)
		s.agentMu.Lock()
		if start, ok := s.agentStart[a.ID]; ok {
			ev.Elapsed = time.Since(start)
			delete(s.agentStart, a.ID)
		}
		s.agentMu.Unlock()
	}
	if s.callbacks.OnAgentEvent != nil {
		s.callbacks.OnAgentEvent(ev)
	}
	if s.hooks != nil {
		s.hooks.Fire(s.ctx, hooks.EventAgentEvent, string(a.Status), map[string]any{
			"event":  "AgentEvent",
			"id":     a.ID,
			"type":   a.Type,
			"status": string(a.Status),
		})
	}
}

// agentUsageFull extracts a finished sub-agent's executed-tool count and full
// cumulative token usage. Safe against a nil agent, fragment or status — a
// failed agent never gets a fragment.
//
// The full LLMUsage (not just the total) exists because the session counter
// tracks prompt and completion tokens separately: folding a sub-agent in with
// only its total would leave the two component figures silently short of it.
func agentUsageFull(a *cogito.AgentState) (toolCount int, usage cogito.LLMUsage) {
	if a == nil || a.Fragment == nil || a.Fragment.Status == nil {
		return 0, cogito.LLMUsage{}
	}
	return len(a.Fragment.Status.ToolsCalled), a.Fragment.Status.CumulativeUsage
}

func (s *Session) ClearHistory() {
	s.historyMu.Lock()
	s.messages = []openai.ChatCompletionMessage{}
	s.fragment = cogito.NewEmptyFragment()
	s.historyMu.Unlock()
}

// ExportHistory returns a copy of the full conversation messages (the same
// []openai.ChatCompletionMessage that backs the model context), suitable for
// JSON serialization and persistence. Feed the result back via
// types.Config.InitialHistory to resume the conversation losslessly — the model
// then continues with real memory of it, not a summary.
//
// The returned slice is a copy, so mutating it (or its serialization) never
// touches the live session. It EXCLUDES the system prompt: s.messages only ever
// records user/assistant turns (the system prompt is regenerated per
// model/locale and re-applied to the fragment on every turn), so there is no
// system message to strip. Safe to call from another goroutine while a turn is
// running; it takes the same lock SendMessage holds while appending.
func (s *Session) ExportHistory() []openai.ChatCompletionMessage {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	return slices.Clone(s.messages)
}

// SetGoal sets (or replaces) the active session goal. While a goal is set, a
// turn re-runs until the model calls goal_done or the user interrupts. Call
// between turns, not during a live run: the goal_done tool is wired at the
// start of a turn, so arming a goal mid-run would not expose goal_done.
func (s *Session) SetGoal(goal string) {
	s.runMu.Lock()
	s.goal = goal
	s.runMu.Unlock()
}

// Goal returns the active session goal, or "" if none.
func (s *Session) Goal() string {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.goal
}

// ClearGoal removes the active session goal.
func (s *Session) ClearGoal() {
	s.runMu.Lock()
	s.goal = ""
	s.runMu.Unlock()
}

// beginTurn starts a per-turn cancellable context derived from the session
// context and stores its cancel func so Interrupt can cancel just this turn.
func (s *Session) beginTurn() context.Context {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	ctx, cancel := context.WithCancel(s.ctx)
	s.turnCancel = cancel
	return ctx
}

// endTurn releases the current turn context.
func (s *Session) endTurn() {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.turnCancel != nil {
		s.turnCancel()
		s.turnCancel = nil
	}
}

// Interrupt cancels the in-flight turn (and any sub-agents spawned within it),
// leaving the session alive. Safe to call when no turn is running.
func (s *Session) Interrupt() {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.turnCancel != nil {
		s.turnCancel()
	}
}

// SetShellJobs wires the shared shell-job registry into the session so the
// pending-work predicate keeps a run parked while a backgrounded shell command
// is still running, and so finished shell jobs inject a completion notice into
// the live run. Registers the completion hook. Call once at setup.
func (s *Session) SetShellJobs(jobs *wizmcp.ShellJobs) {
	s.shellJobs = jobs
	if jobs == nil {
		return
	}
	jobs.SetOnJobDone(func(info wizmcp.ShellJobInfo) {
		// Only backgrounded jobs interest the live run: a plain foreground
		// command is consumed inline by the tool call that started it.
		if !info.Backgrounded {
			return
		}
		notice := "shell job " + info.ID + " " + info.Status
		if so, se, ok := jobs.Output(info.ID); ok {
			if tail := jobTail(so + se); tail != "" {
				notice += ":\n" + tail
			}
		}
		s.Inject(notice)
	})
}

// InjectUser delivers a user-typed follow-up into the live run (see Inject),
// and additionally tracks it: if the run returns before consuming it (the
// model never saw it), the text is reported by TakeUndelivered so the caller
// can re-dispatch it as a fresh turn instead of losing it to the end-of-run
// drain. System notices (shell-job completions, wake-ups) keep using Inject —
// re-running a stale notice as a fresh turn would re-trigger finished work.
func (s *Session) InjectUser(msg string) bool {
	s.runMu.Lock()
	s.userInjected = append(s.userInjected, msg)
	s.runMu.Unlock()
	if s.Inject(msg) {
		return true
	}
	// Nothing was sent: untrack so the drain can't misreport it later.
	s.runMu.Lock()
	if i := slices.Index(s.userInjected, msg); i >= 0 {
		s.userInjected = slices.Delete(s.userInjected, i, i+1)
	}
	s.runMu.Unlock()
	return false
}

// TakeUndelivered returns (and clears) user-typed follow-ups that were
// injected into a run that ended before consuming them. Call after a run
// returns to re-dispatch them as fresh turns.
func (s *Session) TakeUndelivered() []string {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	out := s.undelivered
	s.undelivered = nil
	return out
}

// Inject delivers msg into the live run's message-injection channel, waking a
// parked loop so the assistant continues in the SAME run. Non-blocking: returns
// false when there is no live run, the channel is full, or msg is empty. Used
// for mid-run user follow-ups, shell-job completions, and scheduled wake-ups.
func (s *Session) Inject(msg string) bool {
	if strings.TrimSpace(msg) == "" {
		return false
	}
	s.runMu.Lock()
	live := s.runLive
	s.runMu.Unlock()
	if !live {
		return false
	}
	select {
	case s.inject <- openai.ChatCompletionMessage{Role: "user", Content: msg}:
		return true
	default:
		return false
	}
}

// RunLive reports whether a run is currently in flight (between SendMessage
// start and return), including while it is parked. The TUI uses this as the
// authoritative signal for whether a typed message should be queued into the
// live run or start a new turn, rather than its own loading/parked UI flags
// which can briefly desync across park/resume events.
func (s *Session) RunLive() bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.runLive
}

// jobTail returns the trailing portion of a job's output for an injection
// notice, trimmed and capped.
func jobTail(s string) string {
	const limit = 600
	s = strings.TrimSpace(s)
	if len(s) > limit {
		s = "…" + s[len(s)-limit:]
	}
	return s
}

// toolEnabled reports whether a built-in tool is exposed to the model. With an
// empty BuiltinTools allowlist (the default) every tool is exposed; otherwise only the
// named ones. Approval gating is separate (see allowedTools).
func (s *Session) toolEnabled(name string) bool {
	return len(s.toolAllow) == 0 || s.toolAllow[name]
}

// mcpToolFilter returns the filter cogito uses to gate MCP-sourced tool calls.
// Tools from a user-configured MCP server (s.cfgClients) always pass — the
// allowlist restricts nib's own built-in and self-config tools, never a
// server the user explicitly added. Everything else falls back to toolEnabled.
//
// This MUST be rebuilt fresh on every call (it already is — called once per
// SendMessage). Never cache/memoize the returned closure or the cfgSessions map
// across turns: ReconcileMCPServers Close()s and replaces session pointers on
// reconnect/reconfigure, so a cached map would hold stale pointers, fail the
// identity check for the new sessions, and silently fall back to toolEnabled —
// reintroducing exactly the bug this method fixes.
func (s *Session) mcpToolFilter() func(*mcp.ClientSession, string) bool {
	cfgSessions := make(map[*mcp.ClientSession]bool, len(s.cfgClients))
	for _, sess := range s.cfgClients {
		cfgSessions[sess] = true
	}
	return func(sess *mcp.ClientSession, name string) bool {
		return cfgSessions[sess] || s.toolEnabled(name)
	}
}

// buildUserFragment appends the user turn to the fragment, attaching multimodal
// parts. cogito's AddMessage routes image parts into image_url MultiContent and
// audio/video into the fragment's transient PendingNativeParts (send-once).
func buildUserFragment(f cogito.Fragment, text string, parts []ContentPart) cogito.Fragment {
	if len(parts) == 0 {
		return f.AddMessage("user", text)
	}
	mm := make([]cogito.Multimedia, 0, len(parts))
	for _, p := range parts {
		mm = append(mm, p) // ContentPart implements cogito.TypedMultimedia
	}
	return f.AddMessage("user", text, mm...)
}

// toolOptions returns the cogito options that shape the tool schemas a run
// advertises: the MCP sessions and their filter, the built-in tools (each gated
// by the BuiltinTools allowlist via toolEnabled), the media tools, the goal
// tool, the self-config tools, agent spawning, and the sink-state setting
// (cogito appends its own "reply" tool unless it is disabled).
//
// SendMessage and Warm both call this so a priming request advertises exactly
// the tools a real turn advertises. The tool schemas are serialized into the
// prompt by the server's chat template, so any difference here changes the
// cached prefix and silently wastes the prime.
//
// goal is passed in rather than read via s.Goal(): Goal() takes runMu, and Warm
// reads the goal under runMu before releasing it to make the long-running
// Prefill call. Reading it here instead would either re-take a held lock or
// force Warm to hold runMu across the network call, blocking Interrupt.
//
// mainModel is passed in for the same reason plus one of its own: the caller
// snapshots it alongside the LLM client it will run with (see currentLLM), so
// the sub-agents a turn spawns resolve against the same model the turn itself
// is using, even if SetModel lands halfway through.
func (s *Session) toolOptions(turnCtx context.Context, goal, mainModel string) []cogito.Option {
	opts := []cogito.Option{
		cogito.WithMCPs(s.allClients()...),
		// Disable cogito's sink-state "reply" tool so ExecuteTools is the whole
		// turn: when the LLM stops calling tools it records its text reply as the
		// final answer (read via LastMessage below). This matches cogito's own
		// examples (examples/chat, examples/sub-agents) and avoids a redundant
		// follow-up Ask that returns empty on many models. This also removes a
		// tool from the advertised set, so it belongs here and not with the
		// run-behavior options.
		cogito.DisableSinkState,
		cogito.WithAgentDefinitions(s.agentDefs...),
		cogito.WithAgentLLMFactory(func(model string, temperature float32, metadata map[string]string) cogito.LLM {
			return s.newAgentLLM(mainModel, model, temperature, metadata)
		}),
	}

	// Built-in tools, each gated by the BuiltinTools allowlist (toolEnabled). The
	// agent-spawning tools (spawn_agent/check_agent/get_agent_result) ride
	// EnableAgentSpawning; the rest are individual registrations. The agent
	// manager/factory stay wired regardless — harmless without the tools.
	if s.toolEnabled("spawn_agent") {
		opts = append(opts, cogito.EnableAgentSpawning)
	}
	if s.toolEnabled("ask_user") {
		opts = append(opts, cogito.WithTools(askUserToolDefinition(func(req AskRequest) string {
			if s.callbacks.OnAskUser != nil {
				return s.callbacks.OnAskUser(req)
			}
			return ""
		})))
	}
	if s.toolEnabled("agent_logs") {
		opts = append(opts, cogito.WithTools(agentLogsToolDefinition(s.AgentLog)))
	}
	if s.toolEnabled("schedule_wakeup") {
		opts = append(opts, cogito.WithTools(scheduleWakeupToolDefinition(func(req WakeupRequest) string {
			if s.callbacks.OnScheduleWakeup != nil {
				return s.callbacks.OnScheduleWakeup(req)
			}
			return "Scheduling is not available in this session."
		}, func() bool {
			return s.agentManager.HasRunning() || (s.shellJobs != nil && s.shellJobs.HasRunning())
		})))
	}
	if s.toolEnabled("cron") {
		opts = append(opts, cogito.WithTools(cronToolDefinition(func(req CronRequest) string {
			if s.callbacks.OnCronCreate != nil {
				return s.callbacks.OnCronCreate(req)
			}
			return "Scheduling is not available in this session."
		})))
	}
	if s.toolEnabled("cron_list") {
		opts = append(opts, cogito.WithTools(cronListToolDefinition(func() string {
			if s.callbacks.OnCronList != nil {
				return s.callbacks.OnCronList()
			}
			return "No scheduler available."
		})))
	}
	if s.toolEnabled("cron_delete") {
		opts = append(opts, cogito.WithTools(cronDeleteToolDefinition(func(id string) string {
			if s.callbacks.OnCronDelete != nil {
				return s.callbacks.OnCronDelete(id)
			}
			return "No scheduler available."
		})))
	}

	// Media understanding tools, gated by the allowlist. Each delegates to a
	// specialist client with the tool's dedicated model and scopes the path to
	// the session working dir, mirroring how host file tools resolve paths.
	if s.toolEnabled("read_image") {
		opts = append(opts, cogito.WithTools(readImageToolDefinition(
			func(path, question string) (string, error) {
				return specialist.New(s.baseURL, s.apiKey).Describe(
					turnCtx, resolveWorkspacePath(s.workingDir, path), s.visionModel, question)
			})))
	}
	if s.toolEnabled("transcribe_audio") {
		opts = append(opts, cogito.WithTools(transcribeAudioToolDefinition(
			func(path string) (string, error) {
				return specialist.New(s.baseURL, s.apiKey).Transcribe(
					turnCtx, resolveWorkspacePath(s.workingDir, path), s.transcribeModel)
			})))
	}
	if s.toolEnabled("read_video") {
		opts = append(opts, cogito.WithTools(readVideoToolDefinition(
			func(path, question string) (string, error) {
				return specialist.New(s.baseURL, s.apiKey).DescribeVideo(
					turnCtx, resolveWorkspacePath(s.workingDir, path), s.videoModel, question)
			})))
	}

	// Register the goal_done tool only while a goal is active, so it never
	// appears as a no-op tool in ordinary turns. The callback records
	// completion: it sets the per-run flag (read by the stop-gate) and clears
	// the goal so it does not re-arm on the next message. The callback body takes
	// runMu, which is correct: it fires during a run, not during option assembly.
	if goal != "" {
		opts = append(opts, cogito.WithTools(goalDoneToolDefinition(func(justification string) string {
			s.runMu.Lock()
			s.goalDone = true
			s.goal = ""
			s.runMu.Unlock()
			return "Goal marked complete: " + justification
		})))
	}

	// Wire the native self-configuration tools so the assistant can manage its
	// own plugins, skills, and MCP servers. requestReload re-wires the live
	// session on the next turn after any mutating op.
	for _, d := range selfConfigToolDefs(s.configurator, s.requestReload) {
		if s.toolEnabled(d.name) {
			opts = append(opts, cogito.WithTools(d.def))
		}
	}

	// Restrict built-in host tools (read/write/edit/bash/glob/grep/web_*) to the
	// allowlist too, but never MCP servers the user explicitly configured —
	// see mcpToolFilter. Installed only when an allowlist is set (empty = all).
	if len(s.toolAllow) > 0 {
		opts = append(opts, cogito.WithMCPToolFilter(s.mcpToolFilter()))
	}

	return opts
}

func (s *Session) SendMessage(text string, parts ...ContentPart) (string, error) {
	if s.hooks != nil {
		s.hooks.Fire(s.ctx, hooks.EventUserPromptSubmit, "", map[string]any{"event": "UserPromptSubmit", "prompt": text})
	}
	turnCtx := s.beginTurn()
	s.applyPendingReload()
	s.allowAllTurn = false
	// The overflow retry is capped per TURN, not per session: a later turn that
	// overflows deserves its own recovery attempt.
	s.overflowMu.Lock()
	s.overflowRetried = 0
	s.overflowMu.Unlock()
	defer s.endTurn()

	// Mark the run live so Inject can target it; clear on return. Drain the
	// injection channel so stale entries can't leak into the next run — but
	// user-typed follow-ups (InjectUser) still sitting there were never seen
	// by the model: hand those back via TakeUndelivered instead of silently
	// discarding them with the system notices.
	s.runMu.Lock()
	s.runLive = true
	s.runMu.Unlock()
	defer func() {
		s.runMu.Lock()
		s.runLive = false
		pendingUser := s.userInjected
		s.userInjected = nil
		s.runMu.Unlock()
		var undelivered []string
		for {
			select {
			case msg := <-s.inject:
				if i := slices.Index(pendingUser, msg.Content); i >= 0 {
					pendingUser = slices.Delete(pendingUser, i, i+1)
					undelivered = append(undelivered, msg.Content)
				}
			default:
				s.runMu.Lock()
				s.undelivered = append(s.undelivered, undelivered...)
				s.runMu.Unlock()
				return
			}
		}
	}()
	s.ensureSystemPrompt()

	s.historyMu.Lock()
	s.fragment = buildUserFragment(s.fragment, text, parts)
	s.messages = append(s.messages, openai.ChatCompletionMessage{
		Role:    "user",
		Content: text,
	})
	s.historyMu.Unlock()

	// Snapshot the client and its model once for the whole turn: SetModel can
	// swap them from another goroutine at any point in here (turnMu does not
	// hold a turn), and a turn that changed model halfway would send its
	// remaining requests to a different model than the one it started on.
	llm, mainModel := s.currentLLM()

	// Build cogito options from config
	cogitoOpts := []cogito.Option{
		cogito.WithContext(turnCtx),
		cogito.WithIterations(s.cogitoOptions.Iterations),
		cogito.WithMaxAttempts(s.cogitoOptions.MaxAttempts),
		cogito.WithMaxRetries(s.cogitoOptions.MaxRetries),
		cogito.WithStatusCallback(func(status string) {
			if s.callbacks.OnStatus != nil {
				s.callbacks.OnStatus(status)
			}
		}),
		cogito.WithReasoningCallback(func(reasoning string) {
			if s.callbacks.OnReasoning != nil {
				s.callbacks.OnReasoning(reasoning)
			}
		}),
		// Feed tool-returned images (e.g. computer_use screenshots) back to the
		// model only when desktop control is armed for this session.
		cogito.WithToolImageForwarding(s.computerEnabled),
		cogito.WithToolCallBack(func(tool *cogito.ToolChoice, state *cogito.SessionState) cogito.ToolCallDecision {
			// Capture sub-agent activity so the agent_logs tool can surface what
			// a backgrounded sub-agent is doing.
			if state.AgentID != "" {
				s.agentLogs.recordCall(state.AgentID, tool)
			}
			args, err := json.Marshal(tool.Arguments)
			if err != nil {
				return cogito.ToolCallDecision{Approved: false}
			}
			decision := s.decideToolCall(ToolCallRequest{
				Name:      tool.Name,
				Arguments: string(args),
				Reasoning: tool.Reasoning,
				AgentID:   state.AgentID,
			})
			s.emitSubAgentToolLine(decision.Approved, state.AgentID, tool.Name, string(args))
			return decision
		}),
		cogito.WithToolCallResultCallback(func(status cogito.ToolStatus) {
			s.agentLogs.recordResult(status) // no-op for root-agent tool calls
			if s.hooks != nil {
				s.hooks.Fire(s.ctx, hooks.EventPostToolUse, status.Name, map[string]any{
					"event":  "PostToolUse",
					"tool":   status.Name,
					"result": status.Result,
				})
			}
			if s.callbacks.OnToolResult != nil {
				argsJSON := ""
				if b, err := json.Marshal(status.ToolArguments.Arguments); err == nil {
					argsJSON = string(b)
				}
				s.callbacks.OnToolResult(ToolResult{
					Name:      status.Name,
					Result:    status.Result,
					Arguments: argsJSON,
					AgentID:   s.agentLogs.agentFor(status.ToolArguments.ID),
				})
			}
		}),
		// Park/inject/resume: keep the run alive (parked on the injection channel)
		// whenever a sub-agent or a backgrounded shell job is still running, and
		// let the user inject mid-run follow-ups. cogito auto-injects sub-agent
		// completion results; shell-job completions and wake-ups inject via s.inject.
		cogito.WithMessageInjectionChan(s.inject),
		cogito.WithPendingWork(func() bool {
			return s.agentManager.HasRunning() || s.shellJobs.HasRunning()
		}),
		cogito.WithOnPark(func(reply string) {
			// cogito hands us the no-tool reply text recorded in the fragment
			// right before the loop blocked — the parked reply the UI surfaces.
			if s.callbacks.OnParked != nil {
				s.callbacks.OnParked(reply)
			}
		}),
		cogito.WithOnResume(func() {
			if s.callbacks.OnResumed != nil {
				s.callbacks.OnResumed()
			}
		}),
	}

	// Step-boundary commentary: the assistant text that accompanied a tool
	// selection ("I'll search for X now…"), delivered before the tools run so
	// a UI can commit it in chronological order relative to OnToolResult.
	if s.callbacks.OnStepContent != nil {
		cogitoOpts = append(cogitoOpts, cogito.WithStepContentCallback(func(content string) {
			s.callbacks.OnStepContent(content)
		}))
	}

	// Live token streaming: opt into cogito's streaming decision/answer path only
	// when a consumer wants per-token deltas. Left unset (e.g. the CLI), the path
	// is identical to before. The step-boundary callbacks still fire either way.
	if s.callbacks.OnStream != nil {
		cogitoOpts = append(cogitoOpts, cogito.WithStreamCallback(func(ev cogito.StreamEvent) {
			s.callbacks.OnStream(StreamEvent{
				Kind:     string(ev.Type),
				Content:  ev.Content,
				ToolName: ev.ToolName,
				ToolArgs: ev.ToolArgs,
			})
		}))
	}

	cogitoOpts = append(cogitoOpts,
		// Rewrites what goes on the wire, never s.fragment. Installed here
		// rather than in toolOptions because toolOptions is shared with Warm,
		// whose contract is that a priming request advertises exactly the tool
		// schemas a real turn advertises — a message manipulator is neither.
		cogito.WithMessagesManipulator(s.pruneMessages),
		cogito.WithAgentManager(s.agentManager),
		cogito.WithAgentSpawnCallback(func(a *cogito.AgentState) {
			s.emitAgentEvent(a)
		}),
		cogito.WithAgentCompletionCallback(func(a *cogito.AgentState) {
			s.emitAgentEvent(a)
		}),
	)

	// Tool schemas — shared with Warm so a priming request advertises exactly
	// the tools this turn advertises. See toolOptions.
	cogitoOpts = append(cogitoOpts, s.toolOptions(turnCtx, s.Goal(), mainModel)...)

	// Add ForceReasoning only if enabled in config
	if s.cogitoOptions.ForceReasoning {
		cogitoOpts = append(cogitoOpts, cogito.WithForceReasoning())
	}

	// Run the agent loop. With sink-state disabled, ExecuteTools runs the whole
	// turn and leaves the final natural-language answer as the last message.
	//
	// Stop-gate (/goal): while a goal is active, re-run after each stop until
	// the model calls goal_done (which clears the goal and sets goalDone) or the
	// turn is interrupted. The user can chat/steer mid-pursuit via the existing
	// inject path; Ctrl+C cancels turnCtx and clears the goal.
	var err error
	var response string
	for {
		s.runMu.Lock()
		s.goalDone = false
		s.runMu.Unlock()

		// ExecuteTools takes the fragment by value, so it operates on a copy;
		// only the reassignment of the result needs the lock (below), not the
		// whole call — holding it across the call would block ExportHistory for
		// the entire turn.
		var newFragment cogito.Fragment
		newFragment, err = cogito.ExecuteTools(llm, s.fragment, cogitoOpts...)
		if err != nil && !errors.Is(err, cogito.ErrNoToolSelected) {
			// Interrupt (turnCtx cancelled) surfaces here as a context error;
			// clear the goal so the user's stop sticks and it doesn't re-arm.
			// Other (transient) errors leave the goal active intentionally.
			if turnCtx.Err() != nil {
				s.ClearGoal()
			}
			// The tokens this run already burned are real and already billed,
			// so record them before bailing. cogito stamps CumulativeUsage in a
			// defer onto the fragment it returns, including on its error paths,
			// so an interrupt that lands after several tool-loop calls arrives
			// here with the whole spend in hand. Dropping it would let a user
			// erase thousands of paid-for tokens by pressing Ctrl+C — in a
			// counter that exists to account for spend, and in the usage.json a
			// benchmark harness reads.
			//
			// The turn deliberately is NOT counted here: the user never got the
			// exchange they asked for. Tokens and turns diverge on this path,
			// which is the point of keeping them separate counters.
			if newFragment.Status != nil {
				s.addUsage(newFragment.Status.CumulativeUsage)
			}

			// Context-overflow recovery. The backend has just told us the
			// model's real window — the one moment it does so reliably — so
			// keep it, then compact and try the turn again.
			//
			// Exactly once. If the overflow comes from the system prompt plus
			// tool schemas alone, or from a tail splitForCompaction must keep
			// intact, the second attempt fails identically and a loop would
			// re-summarise into the same wall, burning tokens every pass.
			//
			// Never after an interrupt: a cancelled turnCtx means the user
			// pressed Ctrl+C, and re-sending is the opposite of what they asked
			// for. See canRecoverFromOverflow.
			if canRecoverFromOverflow(turnCtx, err) {
				if w, ok := learnedWindowFrom(err); ok {
					s.rememberWindow(w, mainModel)
				}
				s.overflowMu.Lock()
				first := s.overflowRetried == 0
				s.overflowMu.Unlock()

				if first {
					cb, ca, cerr := s.compactHistory(turnCtx)
					switch {
					case cerr != nil:
						// Report the ORIGINAL overflow, not the summariser's
						// failure: the first is the one the user can act on.
						xlog.Warn("overflow recovery: compaction failed", "error", cerr)
					case cb == ca:
						// Nothing to summarise, so a retry would send a
						// byte-identical request.
					default:
						s.overflowMu.Lock()
						s.overflowRetried++
						s.overflowMu.Unlock()
						// compactHistory rebuilt the fragment as [summary] +
						// tail, and renderMessages skips system content, so the
						// system prompt was dropped without even being
						// represented in the summary. SendMessage's own guard
						// sits ABOVE this loop, so the `continue` below would
						// re-send the turn with no identity, no working
						// directory, no skills index and none of the tool
						// guidance. Re-apply the same guard here: it is a
						// no-op whenever the prompt survived in the kept tail.
						s.ensureSystemPrompt()
						// Announced here, AFTER compaction, and only on the
						// branch that reaches the `continue` below. The status
						// promises a retry, and the two branches above are the
						// cases where no retry happens: the user would be told
						// nib was retrying and then handed the bare overflow
						// error, with no corrective status to withdraw the
						// promise. Compaction is a whole LLM call, so this does
						// cost the user a few silent seconds before the line
						// appears — the alternative is a line that lies.
						if s.callbacks.OnStatus != nil {
							s.callbacks.OnStatus("Context window exceeded — compacting and retrying…")
						}
						if s.callbacks.OnCompactDone != nil {
							s.callbacks.OnCompactDone(cb, ca)
						}
						continue
					}
				}
			}

			// Reached only when no retry is happening: either this turn never
			// qualified for one, or it already spent its single recovery and
			// overflowed again. In that second case the message must not advise
			// clearing the conversation, because compaction just did the
			// equivalent and the retry has already been made — see
			// contextOverflowRetriedMessage.
			err = humanizeTurnError(err, s.overflowRetries() > 0)
			if s.callbacks.OnError != nil {
				s.callbacks.OnError(err)
			}
			return "", err
		}

		// ExecuteTools returned an answer, so a request completed against the
		// model and the server has prefilled this session's prompt prefix. This
		// is the true boundary: everything above can bail without a request
		// (hooks, reload, fragment assembly, option building), and the paths in
		// SendWithAttachments that return before SendMessage never get here.
		//
		// An interrupted turn deliberately does NOT count. Cancellation after the
		// request reached the server may or may not have populated the cache, and
		// the two mistakes are not symmetric: staying cold costs at worst one
		// extra "preparing the model" label, while claiming warm too early costs
		// the user a silent minute with no explanation.
		if turnCtx.Err() == nil {
			s.prefixWarm.Store(true)
		}

		response = newFragment.LastMessage().Content

		// Each ExecuteTools run reports its own cumulative usage, so the whole
		// tool loop is counted rather than just the final call. The goal
		// stop-gate can run this loop more than once per SendMessage, which is
		// why the turn is counted after the loop and the tokens here.
		//
		// Status is a pointer and a fragment that never reached a backend leaves
		// it nil, so this guards rather than assuming a run always populated it.
		if newFragment.Status != nil {
			s.addUsage(newFragment.Status.CumulativeUsage)
		}

		s.historyMu.Lock()
		s.fragment = newFragment
		s.messages = append(s.messages, openai.ChatCompletionMessage{
			Role:    "assistant",
			Content: response,
		})
		s.historyMu.Unlock()

		s.runMu.Lock()
		done := s.goalDone
		goal := s.goal
		s.runMu.Unlock()

		// Stop when there is no goal, the model declared it done, or the turn
		// was interrupted. Interrupt also clears the goal (user stopped it).
		if goal == "" || done {
			break
		}
		if turnCtx.Err() != nil {
			s.ClearGoal()
			break
		}

		// Goal still active and unconfirmed: re-feed it and run again.
		reminder := goalReminder(goal)
		s.historyMu.Lock()
		s.fragment = s.fragment.AddMessage("user", reminder)
		s.messages = append(s.messages, openai.ChatCompletionMessage{
			Role:    "user",
			Content: reminder,
		})
		s.historyMu.Unlock()
		if s.callbacks.OnStatus != nil {
			s.callbacks.OnStatus("Goal not yet met — continuing…")
		}
	}

	if s.callbacks.OnResponse != nil {
		s.callbacks.OnResponse(response)
	}

	if s.hooks != nil {
		s.hooks.Fire(s.ctx, hooks.EventStop, "", map[string]any{"event": "Stop"})
	}

	// Auto-compaction: if the last request crossed the configured fraction of
	// the context window, summarize older turns. Never fail the user's turn.
	promptTokens := 0
	if s.fragment.Status != nil {
		promptTokens = s.fragment.Status.LastUsage.PromptTokens
	}
	if shouldAutoCompact(s.compaction, s.contextWindow(), promptTokens) {
		if s.callbacks.OnStatus != nil {
			s.callbacks.OnStatus("Compacting conversation…")
		}
		cb, ca, cerr := s.compactHistory(turnCtx)
		if cerr != nil {
			xlog.Warn("auto-compaction failed", "error", cerr)
		} else if cb != ca && s.callbacks.OnCompactDone != nil {
			s.callbacks.OnCompactDone(cb, ca)
		}
	}

	// One turn per completed SendMessage, counted after the goal loop rather
	// than inside it: a goal that took five ExecuteTools runs is still one
	// exchange the user asked for.
	//
	// Tokens are counted on every run — including the interrupted and failed
	// ones, which return above and add their spend there — while Turns counts
	// only exchanges that completed. So an interrupted session can hold tokens
	// with a lower turn count, and that asymmetry is deliberate: the backend
	// was paid either way, but the user only got the answers it finished.
	s.countTurn()

	return response, nil
}

// Warm issues the same request the next SendMessage would build — same system
// prompt, same tool schemas, same model — capped at one output token, so the
// server prefills and caches the prompt prefix.
//
// It does not touch s.fragment or s.messages and fires no callbacks: nothing
// enters the transcript and the UI never sees it. It honors ctx, so a user who
// sends a message mid-prime cancels it rather than queueing behind it.
//
// The prefix must match the real request to hit the cache, which is why this
// builds through toolOptions rather than assembling its own list. Note that a
// nil error only means the request was accepted — the server may or may not
// have retained the prefix.
func (s *Session) Warm(ctx context.Context) error {
	// Take runMu only to read the goal, and release it before the network call.
	// Holding it across a prime would block Interrupt and the injection path for
	// the whole prefill. Do not "fix" a prime/turn race by widening this without
	// measuring what it blocks.
	s.runMu.Lock()
	goal := s.goal
	s.runMu.Unlock()

	s.historyMu.Lock()
	sys := s.systemPrompt
	s.historyMu.Unlock()

	f := cogito.NewEmptyFragment()
	if sys != "" {
		f = f.AddMessage("system", sys)
	}
	// A minimal user turn: the prime and the real first message diverge only at
	// this content, so the server reuses the whole system-plus-tools prefix.
	f = f.AddMessage("user", "hi")

	// One snapshot for the prime, for the same reason SendMessage takes one:
	// the prefix is only worth caching for the model that will actually be
	// asked for it.
	llm, mainModel := s.currentLLM()

	opts := append([]cogito.Option{cogito.WithContext(ctx)}, s.toolOptions(ctx, goal, mainModel)...)
	// cogito.Prefill rejects force reasoning outright: with it on, the real
	// turn's first request is a reasoning call this prime does not reproduce.
	// Pass the flag through so that refusal surfaces here rather than letting
	// Warm cache a prefix nothing will ask for and report success.
	if s.cogitoOptions.ForceReasoning {
		opts = append(opts, cogito.WithForceReasoning())
	}

	if err := cogito.Prefill(ctx, llm, f, opts...); err != nil {
		return err
	}
	// Priming the prefix is Warm's entire purpose, so a successful prime is by
	// definition the prefix having been sent to the model. A failed or cancelled
	// prime leaves the session cold, as it should.
	s.prefixWarm.Store(true)
	return nil
}

// GetMessages returns all messages in the conversation
func (s *Session) GetMessages() []Message {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	messages := []Message{}
	for _, msg := range s.messages {
		messages = append(messages, Message{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}
	return messages
}

// allClients returns every connected MCP client (built-ins + skills + config
// servers). Called from the turn goroutine. Reload mutates these only at turn
// start (and is deferred while background sub-agents run, see
// applyPendingReload), so it does not race with detached agents and no lock is
// needed.
//
// The order here is load-bearing, not cosmetic: it decides the order of the
// tool definitions in the request, which the chat template renders into the
// prompt ahead of the user's turn. Ranging cfgClients (a map) directly made
// that order random per turn, so roughly every other request moved the token
// prefix and lost the server's KV cache for the whole system+tools block — a
// full reprocess, seconds of it on CPU. Config servers are therefore emitted
// by sorted name, which is stable across turns *and* across restarts.
func (s *Session) allClients() []*mcp.ClientSession {
	out := append([]*mcp.ClientSession{}, s.clients...)
	if s.skillsClient != nil {
		out = append(out, s.skillsClient)
	}
	for _, name := range slices.Sorted(maps.Keys(s.cfgClients)) {
		out = append(out, s.cfgClients[name])
	}
	return out
}

// ReconcileMCPServers connects newly-desired config MCP servers and closes ones
// no longer desired (or whose command/args changed). Connect failures are
// logged and skipped so one bad server never breaks the session. Called from
// Reload at turn start (deferred while background sub-agents run), so closing a
// client session here cannot race with a detached agent still using it.
func (s *Session) ReconcileMCPServers(desired map[string]types.MCPServer) error {
	for name, sess := range s.cfgClients {
		if d, ok := desired[name]; !ok || !reflect.DeepEqual(d, s.cfgServers[name]) {
			_ = sess.Close()
			delete(s.cfgClients, name)
			delete(s.cfgServers, name)
		}
	}
	for name, srv := range desired {
		if _, ok := s.cfgClients[name]; ok {
			continue
		}
		transport := wizmcp.TransportForServer(srv)
		// Deliberately pass s.ctx (the session's long-lived context) directly,
		// with no per-connect timeout. The go-sdk's mcp.Client.Connect stores the
		// context it is given for the connection's ENTIRE lifetime (not just the
		// initial handshake): it derives a cancellable context from it and uses
		// that to drive the background "hanging GET" SSE listener and reconnect
		// machinery. A timeout- (or otherwise short-lived) context here would tear
		// down that background listener shortly after connecting, so a later tool
		// call that needs to re-establish its SSE stream fails with the real,
		// user-reported error chain: `hanging GET: failed to reconnect ...
		// context canceled` on a server that is otherwise connected and healthy.
		// This mirrors the built-in host-tool connect path in NewSession, which
		// also hands Connect the raw session context.
		sess, err := s.mcpClient.Connect(s.ctx, transport, nil)
		if err != nil {
			xlog.Warn("self-config: MCP server failed to connect", "name", name, "error", err)
			continue
		}
		s.cfgClients[name] = sess
		s.cfgServers[name] = srv
	}
	return nil
}

// SetSkills rebuilds the in-memory skills MCP server so load_skill advertises
// the given skills, swapping its client. An empty list tears the server down.
// Called from Reload at turn start (deferred while background sub-agents run),
// so closing the old skills client cannot race with a detached agent.
func (s *Session) SetSkills(skills []types.Skill) error {
	if s.skillsClient != nil {
		_ = s.skillsClient.Close()
		s.skillsClient = nil
	}
	s.skills = skills
	if len(skills) == 0 {
		return nil
	}
	serverT, clientT := mcp.NewInMemoryTransports()
	go func() {
		if err := wizmcp.StartSkillsMCPServer(s.ctx, serverT, skills); err != nil {
			xlog.Warn("self-config: skills MCP server error", "error", err)
		}
	}()
	sess, err := s.mcpClient.Connect(s.ctx, clientT, nil)
	if err != nil {
		return err
	}
	s.skillsClient = sess
	return nil
}

// Reload re-wires every reloadable part of the session from cfg. It closes MCP
// client sessions and mutates session state read by the turn goroutine and by
// detached sub-agents, so it must run at turn start in the turn goroutine and is
// deferred while background sub-agents run (see applyPendingReload). It must not
// run concurrently with a running turn or a live detached agent.
func (s *Session) Reload(cfg types.Config) error {
	_ = s.ReconcileMCPServers(cfg.MCPServers)
	_ = s.SetSkills(cfg.Skills)
	s.agentDefs = toCogitoDefinitions(cfg.Agents)
	s.agentModels = agentModelSet(s.agentDefs)
	s.hooks = hooks.New(cfg.Hooks)
	if cfg.Prompt != "" {
		s.systemPrompt = cfg.GetPrompt() + s.loadedSkills
	}
	s.compaction = cfg.Compaction
	// Guarded, unlike its neighbours: the manipulator reads the policy from
	// inside cogito's loop, so a reload that lands while any part of a turn is
	// still winding down would otherwise be a data race.
	s.prunedMu.Lock()
	s.pruning = cfg.ToolOutputPruning
	s.prunedMu.Unlock()
	return nil
}

// requestReload marks the session dirty; the next SendMessage applies it.
func (s *Session) requestReload() {
	s.reloadMu.Lock()
	s.pendingReload = true
	s.reloadMu.Unlock()
}

// applyPendingReload, if a reload was requested, recomputes the effective config
// and re-wires the session. Runs at the start of a turn, in the turn goroutine.
// It DEFERS the reload while background sub-agents are still running: Reload
// closes MCP client sessions and mutates session state (s.hooks, s.skillsClient,
// s.cfgClients, s.agentDefs) that detached agents — which outlive their spawning
// turn — continue to read, so reconfiguring under them would race or use a closed
// session. The dirty flag stays set, so the reload is retried on a later turn
// once no background agents are live. The flag is cleared only after a
// SUCCESSFUL reload, so a failed reload is retried on the next turn.
func (s *Session) applyPendingReload() {
	s.reloadMu.Lock()
	pending := s.pendingReload
	s.reloadMu.Unlock()
	if !pending || s.configurator == nil {
		return
	}
	if s.agentManager != nil && s.agentManager.HasRunning() {
		return // defer; pendingReload stays set, retried next turn
	}
	eff, err := s.configurator.EffectiveConfig()
	if err != nil {
		xlog.Warn("self-config: effective config", "error", err)
		return // leave flag set, retried next turn
	}
	if err := s.Reload(eff); err != nil {
		xlog.Warn("self-config: reload", "error", err)
		return // leave flag set, retried next turn
	}
	s.reloadMu.Lock()
	s.pendingReload = false
	s.reloadMu.Unlock()
}

// Close closes the session and cleans up resources
func (s *Session) Close() error {
	var firstErr error
	for _, client := range s.clients {
		if err := client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.skillsClient != nil {
		_ = s.skillsClient.Close()
	}
	for _, c := range s.cfgClients {
		_ = c.Close()
	}
	// The durable half of the exit summary. In tmux-widget mode the pane
	// vanishes before anyone can read the printed line, and a benchmark harness
	// wants to parse this rather than scrape a terminal.
	//
	// A warning, not a returned error, and deliberately unlike the hard failure
	// in NewSession: the session is already over, so there is nothing left to
	// refuse — failing here would only turn a lost report into a lost shutdown.
	//
	// Usage() snapshots under its own lock, so the numbers are always
	// self-consistent, but a Close that races a still-running turn (a second
	// Ctrl+C in the TUI goes straight here) will miss whatever that turn folds
	// in afterwards: the file is a floor on the spend and never an
	// overstatement, matching SessionUsage's documented contract.
	if s.traceDir != "" {
		if err := trace.WriteUsage(s.traceDir, s.Usage()); err != nil {
			xlog.Warn("trace: failed to write usage.json", "dir", s.traceDir, "error", err)
		}
	}
	// s.tracer is nil when tracing is disabled; Close is nil-safe.
	if err := s.tracer.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// ensureSystemPrompt adds the system prompt to the persistent fragment only if
// it is not already there. s.fragment persists across turns, so re-adding it
// every turn would accumulate N identical system messages; cogito merges those
// into a position-0 block that grows each turn and defeats the server's
// prompt-prefix KV cache (full re-prefill every message). Add it once.
//
// It is a method rather than four inline lines because there are now TWO places
// that must hold this invariant, and only one of them is obvious. SendMessage
// calls it once per turn, above the goal loop. compactHistory REPLACES the
// fragment with [summary] + tail — and renderMessages skips system content, so
// the prompt is neither kept nor summarised — which means the overflow
// recovery's `continue` re-enters the loop with the identity, working
// directory, skills index and tool guidance all gone, silently, on the one path
// whose whole purpose is to complete the turn. Any future caller that rebuilds
// the fragment mid-turn has the same duty; giving the rule a name is what makes
// it possible to discharge.
//
// Appending is enough even though the prompt belongs at position 0: cogito's
// normalizeSystemMessages hoists every system message into a single deduped
// block at the front before the request goes out.
func (s *Session) ensureSystemPrompt() {
	if s.systemPrompt == "" {
		return
	}
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if !fragmentHasSystemContent(s.fragment, s.systemPrompt) {
		s.fragment = s.fragment.AddMessage("system", s.systemPrompt)
	}
}

// fragmentHasSystemContent reports whether the fragment already carries a system
// message with exactly this content, so SendMessage can add the system prompt
// once instead of duplicating it every turn (which would defeat the model
// server's prompt-prefix cache).
func fragmentHasSystemContent(f cogito.Fragment, content string) bool {
	for _, m := range f.Messages {
		if m.Role == "system" && m.Content == content {
			return true
		}
	}
	return false
}

// Model returns the model the session is currently using. Safe to call from
// another goroutine while a turn is running.
func (s *Session) Model() string {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()
	return s.llmModel
}

// currentLLM returns the client and the model name it was built for as one
// snapshot, so a caller can never pair a client with the wrong model name.
// Callers that need both for a whole turn must take the snapshot once, up
// front: SetModel can land at any point inside a turn.
func (s *Session) currentLLM() (cogito.LLM, string) {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()
	return s.llm, s.llmModel
}

// SetModel switches the session to a different model, rebuilding the LLM client
// the same way NewSession does: same endpoint and credentials, same
// per-request metadata and reasoning effort, same trace wrapper when tracing is
// on. Relabelling the existing client would not switch anything, and rebuilding
// without re-applying that configuration would silently drop it.
//
// Conversation history is kept: nib is built around persistent context, and
// /compact exists when history needs trimming. Sub-agents follow automatically,
// because newAgentLLM resolves against the session model. The prefix-warm flag
// does not: the new model has never seen this session's prefix.
//
// A turn already in flight finishes on the client it started with (see
// currentLLM); the switch applies from the next turn. Safe to call from another
// goroutine while a turn is running.
func (s *Session) SetModel(name string) {
	// Built outside the lock: every input is construction-time state, so
	// nothing here needs to be ordered against a reader.
	c := clients.NewLocalAILLM(name, s.apiKey, s.baseURL)
	c.SetMetadata(s.metadata)
	c.SetReasoningEffort(s.reasoningEffort)

	var llm cogito.LLM = c
	if s.tracer != nil {
		llm = trace.NewRecordingLLM(llm, s.tracer, name, "")
	}

	s.modelMu.Lock()
	s.llm = llm
	s.llmModel = name
	s.modelMu.Unlock()

	// The new model has never been asked for this session's prefix, so it is
	// genuinely cold. Unlike a server restart or a KV eviction, which the
	// session cannot see, a switch is something we know about, and PrefixWarm
	// prefers a redundant "preparing" label over a silent minute.
	s.prefixWarm.Store(false)
}

// ListModels returns the model IDs the configured endpoint advertises, so a UI
// can offer them for /model. A failing endpoint surfaces as an error rather
// than an empty list.
func (s *Session) ListModels(ctx context.Context) ([]string, error) {
	cfg := openai.DefaultConfig(s.apiKey)
	cfg.BaseURL = s.baseURL
	resp, err := openai.NewClientWithConfig(cfg).ListModels(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

// ModelListTimeout bounds the endpoint lookup behind /model and /models. Both
// front ends run that lookup on the goroutine that draws the prompt, so an
// endpoint that accepts the connection and then never answers would otherwise
// freeze the UI with no way out.
//
// It is deliberately short. In the TUI this is the only synchronous network
// call on the Update goroutine, so the whole interface, Ctrl+C included, stops
// responding for as long as it runs: the bound is a responsiveness budget, not
// a patience budget. A listing that a local endpoint cannot produce in three
// seconds is a listing the user is better off not waiting for, and the
// degraded outcome is mild (a switch that goes through unverified, never a
// refusal).
const ModelListTimeout = 3 * time.Second

// FormatModelList renders a model listing, marking the current model. Order is
// the endpoint's own; the caller decides whether to sort.
func FormatModelList(models []string, current string) string {
	if len(models) == 0 {
		return "no models available\n"
	}
	var b strings.Builder
	for _, m := range models {
		if m == current {
			b.WriteString("* ")
		} else {
			b.WriteString("  ")
		}
		b.WriteString(m)
		b.WriteByte('\n')
	}
	return b.String()
}

// UnservedModelError is what SwitchModel returns when the endpoint's listing
// does not contain the requested name.
//
// It keeps the headline and the listing separate instead of pre-joining them
// into one message, because the two halves want different rendering: the TUI
// word-wraps error text, which strips the listing's indent column and
// truncates long model IDs, while a fenced block survives verbatim. A front
// end with no such distinction can just use Error().
type UnservedModelError struct {
	Name    string   // the name the user asked for
	Models  []string // what the endpoint does serve
	Current string   // the session's model, marked in the listing
}

// Headline is the one-line explanation, safe to wrap.
func (e *UnservedModelError) Headline() string {
	return fmt.Sprintf("model %q is not served by this endpoint. Available:", e.Name)
}

// Listing is the column-aligned model list, which must NOT be re-wrapped.
func (e *UnservedModelError) Listing() string {
	return FormatModelList(e.Models, e.Current)
}

func (e *UnservedModelError) Error() string {
	return e.Headline() + "\n" + strings.TrimRight(e.Listing(), "\n")
}

// SwitchModel is the checked entry point behind /model <name>: it validates the
// name against what the endpoint advertises and only then calls SetModel. It
// returns the notice to show the user, or an error to show instead. Both front
// ends go through it, so the policy and its wording cannot drift between them.
//
// SetModel takes no error by design, so a typo would otherwise switch happily
// and surface a turn later as a 404 from the backend, with nothing pointing at
// the cause. Validating here turns that into an immediate message that names
// the models the endpoint does serve.
//
// A lookup that fails does NOT veto the switch. The list is a convenience, and
// a user asking for a different model may well be asking precisely because
// something is wrong with the endpoint right now; refusing would leave them
// stuck. The same goes for an endpoint that answers with an empty list. Both
// cases switch and say the name went unverified.
func (s *Session) SwitchModel(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("usage: /model <name>")
	}

	lookupCtx, cancel := context.WithTimeout(ctx, ModelListTimeout)
	models, err := s.ListModels(lookupCtx)
	cancel()

	switch {
	case err != nil:
		s.SetModel(name)
		return "model: " + name + " (unverified: " + err.Error() + ")", nil
	case len(models) == 0:
		s.SetModel(name)
		return "model: " + name + " (unverified: the endpoint advertises no models)", nil
	case !slices.Contains(models, name):
		return "", &UnservedModelError{Name: name, Models: models, Current: s.Model()}
	}

	s.SetModel(name)
	return "model: " + name, nil
}
