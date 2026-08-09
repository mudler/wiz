package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mudler/nib/theme"
	"github.com/mudler/nib/types"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mudler/nib/attachments"
	"github.com/mudler/nib/attachstage"
	"github.com/mudler/nib/chat"
	"github.com/mudler/nib/loop"
	wizmcp "github.com/mudler/nib/mcp"
	"github.com/mudler/nib/slash"
)

// ChatMessage represents a message in the chat history
type ChatMessage struct {
	Role      string
	Content   string
	Name      string // tool name, for Role == "tool"
	Arguments string // marshaled call args, for Role == "tool"
	AgentID   string // issuing sub-agent, for Role == "tool" (empty = root agent)
}

// Model represents the TUI state
type Model struct {
	// UI components
	viewport viewport.Model
	textarea textarea.Model
	spinner  spinner.Model

	// Chat state
	messages     []ChatMessage
	session      *chat.Session
	ctx          context.Context
	cancel       context.CancelFunc
	transports   []mcp.Transport
	shellJobs    *wizmcp.ShellJobs
	cfg          types.Config
	sessionReady bool

	// UI state
	width     int
	height    int
	maxHeight int // Configured max height (0 = no limit)
	loading   bool
	// interruptArmed is set after a first Ctrl+C interrupts an in-flight turn,
	// so a second Ctrl+C exits instead of just re-interrupting. Reset when a new
	// turn starts and when the turn ends.
	interruptArmed bool
	// parked is true while the live run is parked (the assistant replied but the
	// run is still alive waiting on the injection channel — background work
	// pending, or simply ready for a follow-up). While parked the composer is
	// usable and Enter injects into the SAME run instead of starting a new turn.
	parked bool
	// lastParkedReply is the assistant text most recently surfaced via a park
	// event, so the terminal responseMsg can avoid re-appending an identical
	// final reply.
	lastParkedReply string
	// wakeupGen invalidates pending reminder/self-paced wake-up ticks: a fired
	// tea.Tick is honored only if its captured gen still matches. Bumped by
	// /loop stop to cancel a self-paced loop. Poll wake-ups ride pollGen instead.
	wakeupGen int
	// pollGen invalidates pending *poll* wake-up ticks (those that only watch
	// in-flight background work). Bumped on every park→resume so an orphan poll
	// armed while parked cannot re-dispatch the task once the work it watched
	// completes on its own (cogito resumes us with the result). Reminders are
	// unaffected — they ride wakeupGen — so a real reminder scheduled during
	// background work still fires. See the parkMsg resume branch / wakeupFireMsg.
	pollGen int
	// selfPaced counts active self-paced loops (for the footer). 0 or 1 in
	// practice. Incremented on /loop <prompt>; reset to 0 by /loop stop. Note: it
	// is NOT auto-cleared when a self-paced loop ends naturally (the TUI has no
	// signal that the model chose not to re-arm), so the footer may show a
	// self-paced loop until /loop stop.
	selfPaced int
	// loops holds active fixed-interval (cron) jobs. Self-paced loops keep no
	// state here — they ride the wake-up timer (see wakeupGen).
	loops     *loop.Registry
	loopsPath string // .nib/loops.json for durable jobs
	status    string
	reasoning string
	err       error
	output    string // Command to output to shell on exit
	quitting  bool

	// Tool approval state
	pendingTool      *chat.ToolCallRequest
	awaitingApproval bool
	// approvalEditing distinguishes the two approval sub-modes: false is the
	// default key-driven choice mode (single y/a/n/e/A/Esc keypresses, input
	// hidden); true is edit mode where the textarea is shown for a free-form
	// change.
	approvalEditing bool

	// ask_user state
	pendingAsk      *chat.AskRequest
	awaitingAsk     bool
	askRequestChan  chan chat.AskRequest
	askResponseChan chan string
	wakeupChan      chan chat.WakeupRequest
	parkChan        chan parkEvent // park/resume signals from the live run
	compactChan     chan [2]int    // {before, after} token counts from auto-compaction
	pruneChan       chan [2]int    // {results, freedTokens} from tool-output pruning

	// contextTokens is the current conversation size shown in the footer badge.
	// Updated after each turn and after compaction; 0 hides the badge.
	contextTokens int

	// sessionUsage is what the session has spent so far, shown in the footer
	// beside the context badge. Refreshed wherever contextTokens is.
	sessionUsage chat.SessionUsage

	// Animation state
	statusPhase int

	// Sub-agent jobs state
	jobs           []agentJob
	agentEventChan chan chat.AgentEvent

	// Ctrl+O log viewer state.
	showLogs    bool           // viewer open
	logSel      int            // selected index in the unified jobs list (list mode)
	logOpenID   string         // when non-empty: drilled into this job's full log
	logOpenKind string         // "agent" | "shell" for the open job
	logVP       viewport.Model // scrollable full-log view

	// Unified `/` completion state
	completion compState

	// Pending message queue: text typed while a run is in flight. Entries are
	// editable until they fire (FIFO) into the live run at step boundaries.
	// queueSel is the entry highlighted for ^e/^x when the composer is empty.
	queue    []string
	queueSel int

	// pending holds files staged via /attach, awaiting the next message. They
	// combine with inline @path files (via attachstage.BuildSend) on send and
	// clear only after a successful send (mirrors the CLI REPL).
	pending []attachstage.StagedFile

	// redispatch holds follow-ups that were released into a run (and echoed)
	// but never consumed by it — the run ended first. They re-dispatch as
	// fresh turns ahead of the queue, without a second echo, and are not
	// shown in the editable-queue UI (they already read as sent).
	redispatch []string

	// Markdown renderers cached per wrap width (glamour renderers are
	// width-bound and expensive to build). At most a couple of distinct
	// widths exist in practice (one per message-prefix width).
	mdRenderers map[int]*glamour.TermRenderer

	// Channels for async communication with callbacks
	statusChan       chan string
	reasoningChan    chan string
	toolRequestChan  chan chat.ToolCallRequest
	toolResponseChan chan chat.ToolCallResponse
	toolResultChan   chan chat.ToolResult
}

// responseMsg is sent when the AI responds
type responseMsg struct {
	content string
	err     error
	blocked []attachments.Blocked
}

// compactResultMsg is the outcome of a manual /compact run.
type compactResultMsg struct {
	before, after int
	err           error
}

// compactNoticeMsg is an auto-compaction notice pushed from the session goroutine.
type compactNoticeMsg [2]int

// pruneNoticeMsg is a tool-output pruning notice pushed from the session goroutine.
type pruneNoticeMsg [2]int

// parkEvent carries a park/resume signal from the live run. parked=true means
// the run parked (assistant replied, run still alive); parked=false means an
// injected message resumed it.
type parkEvent struct {
	parked bool
	reply  string
}

// parkMsg delivers a parkEvent to the update loop.
type parkMsg parkEvent

// statusMsg is sent for status updates
type statusMsg string

// reasoningMsg is sent for reasoning updates
type reasoningMsg string

// toolCallMsg is sent when a tool call needs approval
type toolCallMsg chat.ToolCallRequest

// askMsg is sent when the agent asks the user a question.
type askMsg chat.AskRequest

// agentEventMsg is sent for sub-agent lifecycle updates.
type agentEventMsg chat.AgentEvent

// toolResultPreviewLines bounds how many lines of a tool result we show inline.
const toolResultPreviewLines = 12

// toolResultMsg carries a finished tool's output to the UI.
type toolResultMsg chat.ToolResult

// sessionReadyMsg is sent when the session is initialized
type sessionReadyMsg struct {
	session *chat.Session
	err     error
}

// NewModel creates a new TUI model
func NewModel(ctx context.Context, cfg types.Config, height int, shellJobs *wizmcp.ShellJobs, transports ...mcp.Transport) Model {
	ctx, cancel := context.WithCancel(ctx)

	ta := textarea.New()
	ta.Placeholder = "ask anything…"
	ta.Focus()
	ta.Prompt = theme.PromptGlyph + " "
	ta.FocusedStyle.Prompt = theme.Prompt
	ta.CharLimit = 4096
	ta.SetWidth(80)
	// Single-line input: Enter sends (newline insertion is disabled), so a
	// taller textarea would just repeat the `›` prompt on every empty row.
	ta.SetHeight(1)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetEnabled(false) // Enter sends message

	vp := viewport.New(80, 10)
	vp.SetContent("")

	s := spinner.New()
	s.Spinner = spinner.Points
	s.Style = theme.Help

	// Calculate max height - negative means percentage, positive means lines
	maxH := height
	if maxH < 0 {
		maxH = 0 // Will be calculated on first WindowSizeMsg
	}

	m := Model{
		viewport:         vp,
		logVP:            viewport.New(80, 10),
		textarea:         ta,
		spinner:          s,
		messages:         []ChatMessage{},
		ctx:              ctx,
		cancel:           cancel,
		maxHeight:        maxH,
		transports:       transports,
		shellJobs:        shellJobs,
		cfg:              cfg,
		height:           height,
		agentEventChan:   make(chan chat.AgentEvent, 16),
		statusChan:       make(chan string, 10),
		reasoningChan:    make(chan string, 10),
		toolRequestChan:  make(chan chat.ToolCallRequest),
		toolResponseChan: make(chan chat.ToolCallResponse),
		toolResultChan:   make(chan chat.ToolResult, 64),
		askRequestChan:   make(chan chat.AskRequest),
		askResponseChan:  make(chan string),
		wakeupChan:       make(chan chat.WakeupRequest, 8),
		parkChan:         make(chan parkEvent, 16),
		compactChan:      make(chan [2]int, 4),
		pruneChan:        make(chan [2]int, 4),
		mdRenderers:      make(map[int]*glamour.TermRenderer),
		loops:            loop.NewRegistry(),
		loopsPath:        filepath.Join(".nib", "loops.json"),
	}
	m.completion.setRegistries(cfg.Commands, cfg.Skills, cfg.Agents)
	return m
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.spinner.Tick,
		m.initSession(),
	)
}

// initSession creates the chat session
func (m Model) initSession() tea.Cmd {
	return func() tea.Msg {
		callbacks := chat.Callbacks{
			OnStatus: func(status string) {
				select {
				case m.statusChan <- status:
				default:
				}
			},
			OnReasoning: func(reasoning string) {
				select {
				case m.reasoningChan <- reasoning:
				default:
				}
			},
			OnToolCall: func(req chat.ToolCallRequest) chat.ToolCallResponse {
				// Send tool request and wait for user response
				m.toolRequestChan <- req
				return <-m.toolResponseChan
			},
			OnAskUser: func(req chat.AskRequest) string {
				m.askRequestChan <- req
				return <-m.askResponseChan
			},
			OnScheduleWakeup: func(req chat.WakeupRequest) string {
				// Non-blocking: hand the request to the UI loop and confirm now. The
				// UI arms a timer; when it fires it injects the note into the live
				// run (see wakeupFireMsg).
				select {
				case m.wakeupChan <- req:
					if req.Reason != "" {
						return fmt.Sprintf("Scheduled a wake-up in %ds (%s). You'll be re-invoked then.", req.DelaySeconds, req.Reason)
					}
					return fmt.Sprintf("Scheduled a wake-up in %ds: %q. You'll be re-invoked then.", req.DelaySeconds, req.Prompt)
				default:
					return "Could not schedule wake-up (too many pending)."
				}
			},
			OnCronCreate: func(req chat.CronRequest) string {
				j, err := m.loops.Add(req.Expr, req.Prompt, req.Recurring, req.Durable)
				if err != nil {
					return "cron rejected: " + err.Error()
				}
				if req.Durable {
					_ = m.loops.Save(m.loopsPath)
				}
				return fmt.Sprintf("Scheduled %s (%s) → %q", j.ID, j.Expr, j.Prompt)
			},
			OnCronList: func() string {
				jobs := m.loops.List()
				if len(jobs) == 0 {
					return "No active cron loops."
				}
				var b strings.Builder
				for _, j := range jobs {
					fmt.Fprintf(&b, "%s · %s · %q\n", j.ID, j.Expr, j.Prompt)
				}
				return strings.TrimRight(b.String(), "\n")
			},
			OnCronDelete: func(id string) string {
				if m.loops.Delete(id) {
					_ = m.loops.Save(m.loopsPath)
					return "Cancelled " + id
				}
				return "No such loop: " + id
			},
			OnParked: func(reply string) {
				select {
				case m.parkChan <- parkEvent{parked: true, reply: reply}:
				default:
				}
			},
			OnResumed: func() {
				select {
				case m.parkChan <- parkEvent{parked: false}:
				default:
				}
			},
			OnAgentEvent: func(ev chat.AgentEvent) {
				select {
				case m.agentEventChan <- ev:
				default:
				}
			},
			OnCompactDone: func(before, after int) {
				select {
				case m.compactChan <- [2]int{before, after}:
				default:
				}
			},
			OnPruneDone: func(results, freed int) {
				select {
				case m.pruneChan <- [2]int{results, freed}:
				default:
				}
			},
			OnToolResult: func(res chat.ToolResult) {
				select {
				case m.toolResultChan <- res:
				default:
				}
			},
		}

		session, err := chat.NewSession(m.ctx, m.cfg, callbacks, m.transports...)
		if session != nil {
			// Wire the shell-job registry so backgrounded shell jobs keep the run
			// parked and inject a completion notice when they finish.
			session.SetShellJobs(m.shellJobs)
		}
		return sessionReadyMsg{session: session, err: err}
	}
}

// Update handles messages and updates the model
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Ctrl+O log viewer: intercepts navigation/scroll keys while open. Ctrl+C
		// falls through so it can still interrupt/quit.
		if m.showLogs && msg.Type != tea.KeyCtrlC {
			jobs := m.unifiedJobs()
			if m.logOpenID == "" {
				// LIST mode
				switch {
				case msg.Type == tea.KeyEsc, msg.Type == tea.KeyCtrlO:
					m.showLogs = false
					return m, nil
				case msg.Type == tea.KeyUp:
					if m.logSel > 0 {
						m.logSel--
					}
					return m, nil
				case msg.Type == tea.KeyDown:
					if m.logSel < len(jobs)-1 {
						m.logSel++
					}
					return m, nil
				case msg.Type == tea.KeyEnter:
					if m.logSel >= 0 && m.logSel < len(jobs) {
						m.logOpenID = jobs[m.logSel].ID
						m.logOpenKind = jobs[m.logSel].Kind
						m.syncLogViewport()
					}
					return m, nil
				case msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && (msg.Runes[0] == 'k' || msg.Runes[0] == 'K'):
					if m.logSel >= 0 && m.logSel < len(jobs) {
						m.killSelected(m.logSel + 1)
					}
					return m, nil
				}
				return m, nil // swallow other keys in list mode
			}
			// LOG mode (drilled into one job): Esc -> back to list; Ctrl+O -> close.
			switch msg.Type {
			case tea.KeyEsc:
				m.logOpenID = ""
				m.logOpenKind = ""
				return m, nil
			case tea.KeyCtrlO:
				m.showLogs = false
				m.logOpenID = ""
				m.logOpenKind = ""
				return m, nil
			}
			// Anything else scrolls the log viewport (up/down/pgup/pgdn/home/end).
			var vpCmd tea.Cmd
			m.logVP, vpCmd = m.logVP.Update(msg)
			return m, vpCmd
		}
		// Tool approval is a distinct key-driven mode: in choice mode the chat
		// input is hidden and a numbered menu takes single keypresses (1/2/3,
		// with y/a/A as silent legacy aliases, n/Esc deny, e edits); edit mode
		// (entered with `e`) shows the textarea for a free-form change.
		if m.awaitingApproval {
			if !m.approvalEditing {
				switch {
				case msg.Type == tea.KeyEsc:
					return m.resolveApproval(chat.ToolCallResponse{Approved: false})
				case msg.Type == tea.KeyRunes && len(msg.Runes) == 1:
					switch msg.Runes[0] {
					case '1', 'y', 'Y':
						return m.resolveApproval(chat.ToolCallResponse{Approved: true})
					case '2', 'a':
						var prefix string
						if m.pendingTool != nil {
							_, prefix = chat.GrantScope(m.pendingTool.Name, m.pendingTool.Arguments)
						}
						return m.resolveApproval(chat.ToolCallResponse{Approved: true, AlwaysAllow: true, AlwaysPrefix: prefix})
					case '3', 'A':
						return m.resolveApproval(chat.ToolCallResponse{Approved: true, AllowAllTurn: true})
					case 'n', 'N':
						return m.resolveApproval(chat.ToolCallResponse{Approved: false})
					case 'e', 'E':
						m.approvalEditing = true
						m.textarea.Reset()
						m.updateViewport()
						return m, nil
					}
				}
				// Swallow every other key in choice mode, but let Ctrl+C fall
				// through to the normal interrupt/quit handling below.
				if msg.Type != tea.KeyCtrlC {
					return m, nil
				}
			} else if msg.Type == tea.KeyEsc {
				// Edit mode: Esc cancels back to choice mode without denying.
				m.approvalEditing = false
				m.textarea.Reset()
				m.updateViewport()
				return m, nil
			}
		}
		switch msg.Type {
		case tea.KeyCtrlC:
			// First Ctrl+C on an in-flight turn interrupts the request but keeps
			// the session open; a second Ctrl+C (or Ctrl+C while idle) exits.
			if m.isWorking() && !m.interruptArmed {
				if m.session != nil {
					m.session.Interrupt()
				}
				m.interruptArmed = true
				m.status = "Interrupting… (Ctrl+C again to exit)"
				return m, nil
			}
			return m.quit()

		case tea.KeyEsc:
			return m.quit()

		case tea.KeyCtrlY:
			// Yank nib's last suggested command to the shell and exit, so the
			// Ctrl+Space widget inserts it at the prompt. No-op if there's
			// nothing to yank or a turn is still in flight.
			if !m.sessionReady || m.loading {
				return m, nil
			}
			cmd := lastSuggestedCommand(m.messages)
			if cmd == "" {
				m.status = "no command to use yet"
				return m, nil
			}
			m.output = cmd
			return m.quit()

		case tea.KeyCtrlB:
			// Background the running foreground work: a sub-agent first,
			// otherwise a running foreground shell command.
			if m.sessionReady && m.session != nil {
				if id := m.firstRunningJobID(); id != "" {
					// Detach the sub-agent so it keeps running in the background; its
					// completion is auto-injected into the live run by cogito.
					_ = m.session.AgentManager().Detach(id)
					return m, nil
				}
			}
			if id, ok := m.shellJobs.DetachForeground(); ok {
				m.status = "Backgrounded shell job " + id
				m.updateViewport()
			}
			return m, nil

		case tea.KeyCtrlE:
			// Pull the selected queued entry back into the composer to edit; it
			// re-queues (appended) on the next Enter. Only when the composer is
			// empty, so it never clobbers in-progress typing — otherwise fall
			// through so ctrl+e keeps its textarea meaning (move to line end).
			if strings.TrimSpace(m.textarea.Value()) == "" && len(m.queue) > 0 {
				entry := m.queueDeleteSel()
				if entry != "" {
					m.textarea.SetValue(entry)
					m.textarea.Focus()
					m.completion.sync(entry)
					m.updateViewport()
				}
				return m, nil
			}

		case tea.KeyCtrlX:
			// Delete the selected queued entry. Only when the composer is empty,
			// so it never interferes with in-progress typing — otherwise fall
			// through to the textarea.
			if strings.TrimSpace(m.textarea.Value()) == "" && len(m.queue) > 0 {
				m.queueDeleteSel()
				m.updateViewport()
				return m, nil
			}

		case tea.KeyCtrlO:
			// Toggle the navigable log viewer.
			if !m.sessionReady {
				return m, nil
			}
			m.showLogs = !m.showLogs
			m.logSel = 0
			m.logOpenID = ""
			m.logOpenKind = ""
			return m, nil

		case tea.KeyTab:
			if m.completion.active {
				if ins, ok := m.completion.accept(); ok {
					m.textarea.SetValue(ins)
					m.completion.sync(ins)
				}
				return m, nil
			}

		case tea.KeyUp:
			if m.completion.active {
				m.completion.up()
				return m, nil
			}
			if strings.TrimSpace(m.textarea.Value()) == "" && len(m.queue) > 0 {
				m.queueMoveSel(-1)
				return m, nil
			}

		case tea.KeyDown:
			if m.completion.active {
				m.completion.down()
				return m, nil
			}
			if strings.TrimSpace(m.textarea.Value()) == "" && len(m.queue) > 0 {
				m.queueMoveSel(1)
				return m, nil
			}

		case tea.KeyEnter:
			// Accept an open completion instead of submitting.
			if m.completion.active {
				if ins, ok := m.completion.accept(); ok {
					m.textarea.SetValue(ins)
					m.completion.sync(ins)
				}
				return m, nil
			}

			if !m.sessionReady {
				return m, nil
			}

			input := strings.TrimSpace(m.textarea.Value())
			if input == "" {
				return m, nil
			}

			// Check if we're answering an ask_user question
			if m.awaitingAsk && m.pendingAsk != nil {
				answer := parseAskAnswer(m.textarea.Value(), *m.pendingAsk)
				m.messages = append(m.messages, ChatMessage{Role: "user", Content: answer})
				m.textarea.Reset()
				m.awaitingAsk = false
				m.pendingAsk = nil
				m.loading = true
				m.status = "Thinking…"
				m.updateViewport()
				m.askResponseChan <- answer
				return m, nil
			}

			// Check if we're in tool approval mode
			if m.awaitingApproval {
				return m.handleToolApproval(input)
			}

			// While a run is in flight (working or parked), the input does not start
			// a new turn — it queues into the live run. Queued entries are editable
			// until they fire (FIFO) at the next step boundary. A parked run is idle
			// and waiting, so release the front entry immediately to resume it.
			if m.loading || m.parked {
				m.queue = append(m.queue, input)
				m.textarea.Reset()
				m.completion.sync("")
				m.interruptArmed = false
				if m.parked {
					m.releaseQueueFront()
				}
				m.updateViewport()
				return m, nil
			}

			// Echo + resolve + dispatch (command/skill/message).
			m.textarea.Reset()
			m.completion.sync("")
			cmd := m.dispatchInput(input)
			m.updateViewport()
			return m, cmd
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateDimensions()

	case sessionReadyMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.session = msg.session
		m.sessionReady = true
		// Reload durable cron loops persisted from a previous session.
		if n, err := m.loops.Load(m.loopsPath); err == nil && n > 0 {
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: fmt.Sprintf("Reloaded %d durable loop(s).", n)})
		}
		// Start listening for callbacks
		cmds = append(cmds, m.listenStatus(), m.listenReasoning(), m.listenToolRequest(), m.listenToolResult(), m.listenAskRequest(), m.listenAgentEvents(), m.shellTick(), m.loopTick(), m.listenWakeup(), m.listenPark(), m.listenCompact(), m.listenPrune())

	case responseMsg:
		// The run returned: it is no longer parked (all background work drained).
		m.loading = false
		m.parked = false
		m.interruptArmed = false
		m.status = ""
		m.reasoning = ""
		// Surface any attachments that couldn't be sent (blocked by model caps
		// or resolution), mirroring the CLI's per-file error lines.
		for _, b := range msg.blocked {
			m.messages = append(m.messages, ChatMessage{Role: "error", Content: filepath.Base(b.Path) + " — " + b.Reason})
		}
		// Clear staged attachments only on a successful send (retain on error),
		// matching the CLI REPL.
		if msg.err == nil {
			m.pending = nil
		}
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) {
				m.messages = append(m.messages, ChatMessage{Role: "agent", Content: "interrupted."})
			} else {
				m.err = msg.err
				m.messages = append(m.messages, ChatMessage{Role: "error", Content: msg.err.Error()})
			}
		} else if content := strings.TrimSpace(msg.content); content != "" && content != m.lastParkedReply {
			// Skip the final reply when it duplicates the text already surfaced at
			// the park gate (a run that parked and returned with the same answer).
			m.messages = append(m.messages, ChatMessage{Role: "assistant", Content: msg.content})
		}
		m.lastParkedReply = ""
		if m.session != nil {
			m.contextTokens = m.session.ContextTokens()
			m.sessionUsage = m.session.Usage()
			// Follow-ups released into the ended run that it never consumed:
			// the model never saw them, so re-dispatch them ahead of the queue.
			m.redispatch = append(m.redispatch, m.session.TakeUndelivered()...)
		}
		m.updateViewport()
		// The run ended with messages still queued: dispatch them as fresh turns
		// (resolving slash commands/skills) until one starts a turn or the queue
		// drains; the remainder stay queued and flush on subsequent run ends.
		if cmd := m.flushQueueAsTurn(); cmd != nil {
			m.updateViewport()
			return m, cmd
		}
		// A tail of non-turn entries (skill loads / resolve errors) may have
		// appended notices without starting a turn; re-render so they show now.
		m.updateViewport()

	case parkMsg:
		if msg.parked {
			// The run parked: the assistant has replied but stays alive (background
			// work pending, or ready for a follow-up). Surface the reply as a
			// durable transcript line and unlock the composer so the user can keep
			// chatting — their input injects into this same run.
			reply := strings.TrimSpace(msg.reply)
			if reply != "" && reply != m.lastParkedReply {
				m.messages = append(m.messages, ChatMessage{Role: "assistant", Content: reply})
				m.lastParkedReply = reply
			}
			m.parked = true
			m.loading = false
			m.interruptArmed = false
			m.reasoning = ""
			if m.isWorking() {
				m.status = "Working in the background — type to add a follow-up"
			} else {
				m.status = ""
			}
			if m.session != nil {
				m.contextTokens = m.session.ContextTokens()
				m.sessionUsage = m.session.Usage()
			}
			m.textarea.Focus()
			// Parked == the agent is idle waiting; release the next queued
			// follow-up now (flips back to loading via releaseQueueFront).
			if m.releaseQueueFront() {
				m.status = "Thinking…"
			}
		} else {
			// An injected message resumed the run: re-lock the composer and show
			// the working indicator again.
			m.parked = false
			m.loading = true
			m.interruptArmed = false
			m.status = "Thinking…"
			// Invalidate any orphan poll wake-up. A poll wake-up only exists to
			// nudge the run if it stays stuck on background work; once that work
			// completes it injects its own result and resumes us here, so a
			// still-pending poll tick would otherwise fire after the turn ends and
			// re-dispatch the finished task as a fresh turn. Each live poll cycle
			// re-arms with the bumped gen, so only the now-moot orphan dies.
			// Reminders ride wakeupGen and are left intact.
			m.pollGen++
		}
		m.updateViewport()
		cmds = append(cmds, m.listenPark())

	case compactResultMsg:
		m.loading = false
		m.status = ""
		if msg.err != nil {
			m.messages = append(m.messages, ChatMessage{Role: "error", Content: "compaction failed: " + msg.err.Error()})
		} else if msg.before == msg.after {
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: "Nothing to compact yet."})
		} else {
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: compactNotice(msg.before, msg.after)})
			m.contextTokens = msg.after
			// The context shrank; the spend did not. Re-read the session's own
			// counter rather than deriving anything from msg.
			if m.session != nil {
				m.sessionUsage = m.session.Usage()
			}
		}
		m.updateViewport()
		if cmd := m.flushQueueAsTurn(); cmd != nil {
			m.updateViewport()
			return m, cmd
		}
		m.updateViewport()
		return m, nil

	case compactNoticeMsg:
		m.messages = append(m.messages, ChatMessage{Role: "agent", Content: compactNotice(msg[0], msg[1])})
		m.contextTokens = msg[1]
		// Auto-compaction only shrinks the context: the summarising call itself
		// costs tokens, so take the session's total rather than msg's numbers.
		if m.session != nil {
			m.sessionUsage = m.session.Usage()
		}
		m.updateViewport()
		return m, m.listenCompact()

	case pruneNoticeMsg:
		m.messages = append(m.messages, ChatMessage{Role: "agent", Content: prunedNotice(msg[0], msg[1])})
		m.updateViewport()
		return m, m.listenPrune()

	case shellTickMsg:
		// Periodic refresh so the shell-jobs footer reflects jobs that finish (or
		// are started by the model) while the user is idle. Completion handling no
		// longer lives here: finished background work injects into the live run
		// (shell jobs via SetShellJobs, sub-agents via cogito's auto-injection).
		m.updateViewport()
		if m.showLogs && m.logOpenID != "" {
			m.syncLogViewport()
		}
		if !m.quitting {
			cmds = append(cmds, m.shellTick())
		}

	case loopTickMsg:
		for _, j := range m.loops.Due() {
			if c := m.dispatchLoop(j.Prompt); c != nil {
				cmds = append(cmds, c)
			}
		}
		if !m.quitting {
			cmds = append(cmds, m.loopTick())
		}

	case wakeupScheduledMsg:
		// Arm a timer for the requested delay, then keep listening for more.
		// Poll wake-ups capture pollGen (dropped on park→resume); reminders and
		// self-paced steps capture wakeupGen (dropped only by /loop stop).
		req := chat.WakeupRequest(msg)
		d := time.Duration(req.DelaySeconds) * time.Second
		prompt := req.Prompt
		poll := req.Poll
		gen := m.wakeupGen
		if poll {
			gen = m.pollGen
		}
		cmds = append(cmds,
			tea.Tick(d, func(time.Time) tea.Msg { return wakeupFireMsg{prompt: prompt, gen: gen, poll: poll} }),
			m.listenWakeup(),
		)

	case wakeupFireMsg:
		// A stale tick — a cancelled self-paced loop, or a poll whose background
		// work already completed (park→resume bumped pollGen): ignore.
		curGen := m.wakeupGen
		if msg.poll {
			curGen = m.pollGen
		}
		if msg.gen != curGen {
			break
		}
		// The delay elapsed: re-run the carried prompt as the next turn. Resolve
		// the payload so a /command re-runs as that command (self-paced loop).
		prompt := strings.TrimSpace(msg.prompt)
		if prompt == "" {
			prompt = "continue"
		}
		action := slash.Resolve(prompt, m.cfg.Commands, m.cfg.Skills, m.cfg.Agents)
		text := action.Text
		if action.Kind != slash.KindSend || text == "" {
			// Non-KindSend payloads (skill/compact/error) intentionally degrade to literal text — loops carry slash-commands or prompts, not those.
			text = prompt // fall back to the raw text for non-send payloads
		}
		if m.session != nil && m.parked && m.session.Inject(text) {
			m.messages = append(m.messages, ChatMessage{Role: "user", Content: prompt})
			m.parked = false
			m.loading = true
			m.interruptArmed = false
			m.status = "Thinking…"
			m.updateViewport()
		} else if m.sessionReady && m.session != nil && !m.loading && !m.awaitingApproval && !m.awaitingAsk {
			m.messages = append(m.messages, ChatMessage{Role: "user", Content: prompt})
			m.loading = true
			m.interruptArmed = false
			m.status = "Thinking…"
			m.updateViewport()
			cmds = append(cmds, m.sendMessage(text))
		}
		// If a wake-up fires mid-run (loading and not parked) it is dropped: the model drives self-pacing at turn end, so this is rare; cron loops queue instead (see releaseQueueFront).

	case statusMsg:
		m.status = string(msg)
		m.updateViewport()
		// Continue listening for more status updates
		cmds = append(cmds, m.listenStatus())

	case reasoningMsg:
		m.reasoning = string(msg)
		m.updateViewport()
		// Continue listening for more reasoning updates
		cmds = append(cmds, m.listenReasoning())

	case toolCallMsg:
		m.pendingTool = (*chat.ToolCallRequest)(&msg)
		m.awaitingApproval = true
		m.approvalEditing = false // every approval starts in key-driven choice mode
		m.loading = false         // Allow user input for approval
		m.textarea.Focus()        // Ensure textarea is focused for input
		m.updateViewport()
		// Continue listening for more tool requests
		cmds = append(cmds, m.listenToolRequest())

	case askMsg:
		req := chat.AskRequest(msg)
		m.pendingAsk = &req
		m.awaitingAsk = true
		m.loading = false
		m.textarea.Focus()
		m.updateViewport()
		cmds = append(cmds, m.listenAskRequest())

	case agentEventMsg:
		// Update value-receiver copy via pointer helper, then write back.
		ev := chat.AgentEvent(msg)
		am := m
		(&am).applyAgentEvent(ev)
		m = am
		// On completion, always show the stats marker line (e.g.
		// "sub-agent explore finished · 3 tools · …"); when the agent produced a
		// final result, also surface it inline as one labeled block. Per-tool
		// activity stays in the Ctrl+O log viewer.
		if line := agentTranscriptLine(ev); line != "" {
			m.messages = append(m.messages, ChatMessage{Role: "agent", AgentID: ev.ID, Content: line})
		}
		if ev.Status == chat.AgentStatusCompleted && strings.TrimSpace(ev.Result) != "" {
			typ := ev.Type
			if typ == "" {
				typ = "agent"
			}
			m.messages = append(m.messages, ChatMessage{
				Role:    "agent_result",
				Name:    typ,
				AgentID: ev.ID,
				Content: chat.PreviewResult(ev.Result, toolResultPreviewLines),
			})
		}
		m.updateViewport()
		if m.showLogs && m.logOpenID != "" {
			m.syncLogViewport()
		}
		// Continue listening for more agent events
		cmds = append(cmds, m.listenAgentEvents())

	case toolResultMsg:
		res := chat.ToolResult(msg)
		if res.AgentID == "" {
			// Root agent: stream the result inline with its (previewed) body.
			if preview := chat.PreviewResult(res.Result, toolResultPreviewLines); preview != "" {
				m.messages = append(m.messages, ChatMessage{Role: "tool", Name: res.Name, Arguments: res.Arguments, Content: preview})
				m.updateViewport()
			}
		} else {
			// Sub-agent: append a compact, body-less line to its inline thread.
			// The output body lives in the Ctrl+O log viewer.
			label := chat.FormatToolCall(res.Name, res.Arguments)
			if nl := strings.IndexByte(label, '\n'); nl >= 0 {
				label = label[:nl]
			}
			if label == "" {
				label = res.Name
			}
			m.messages = append(m.messages, ChatMessage{Role: "agent_tool", Name: res.Name, Arguments: res.Arguments, AgentID: res.AgentID, Content: label})
			m.updateViewport()
			if m.showLogs && m.logOpenID != "" {
				m.syncLogViewport()
			}
		}
		// A step just completed: release the next queued follow-up into the run.
		m.releaseQueueFront()
		// Continue listening for more tool results
		cmds = append(cmds, m.listenToolResult())

	case spinner.TickMsg:
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
		// Rotate status phase for animated messages
		if m.loading {
			m.statusPhase = (m.statusPhase + 1) % 12
			m.updateViewport()
		}
	}

	// Update textarea. The composer is always editable — even while a run is in
	// flight — so the user can type follow-ups that queue into the live run.
	m.textarea, cmd = m.textarea.Update(msg)
	cmds = append(cmds, cmd)
	m.completion.sync(m.textarea.Value())

	// Update viewport
	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// fencedListing wraps a model listing in a code fence so the transcript leaves
// its columns alone. An "agent" line is rendered as markdown, where a plain
// listing loses its two-space indent and "* current" becomes an ordinary
// bullet: the marker column, which is the only thing the listing has to say,
// is exactly what gets eaten.
func fencedListing(listing string) string {
	return "```\n" + listing + "```"
}

// dispatchInput echoes the user's literal input to the transcript, resolves it
// as a slash command / skill / message, and starts the appropriate action.
// Returns the command to run (nil for actions that don't start a turn, e.g. a
// skill load or a resolve error). Shared by the Enter handler and the queue
// flush so typed-while-idle and queued-while-busy input behave identically.
func (m *Model) dispatchInput(input string) tea.Cmd {
	m.messages = append(m.messages, ChatMessage{Role: "user", Content: input})
	return m.dispatchResolved(input)
}

// dispatchResolved resolves and starts input WITHOUT echoing it to the
// transcript — for re-dispatched undelivered follow-ups, whose transcript
// line was already written when they were released into the previous run.
func (m *Model) dispatchResolved(input string) tea.Cmd {
	action := slash.Resolve(input, m.cfg.Commands, m.cfg.Skills, m.cfg.Agents)
	switch action.Kind {
	case slash.KindError:
		m.messages = append(m.messages, ChatMessage{Role: "error", Content: action.Err})
		return nil
	case slash.KindLoadSkill:
		notice, err := m.session.LoadSkill(action.Skill)
		if err != nil {
			m.messages = append(m.messages, ChatMessage{Role: "error", Content: err.Error()})
		} else {
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: notice})
		}
		return nil
	case slash.KindCompact:
		m.loading = true
		m.interruptArmed = false
		m.status = "Compacting conversation…"
		return m.compactCmd()
	case slash.KindModelList:
		// Bounded: dispatchResolved runs on the Update goroutine, so an endpoint
		// that accepts the connection and never answers would freeze the TUI.
		lookupCtx, cancel := context.WithTimeout(m.ctx, chat.ModelListTimeout)
		defer cancel()
		models, err := m.session.ListModels(lookupCtx)
		if err != nil {
			m.messages = append(m.messages, ChatMessage{Role: "error", Content: err.Error()})
		} else {
			listing := fencedListing(chat.FormatModelList(models, m.session.Model()))
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: listing})
		}
		return nil
	case slash.KindModelSet:
		notice, err := m.session.SwitchModel(m.ctx, action.Model)
		var unserved *chat.UnservedModelError
		switch {
		case errors.As(err, &unserved):
			// Two transcript lines, not one. The refusal itself is prose and
			// wraps happily, but the listing must not: an "error" line goes
			// through wrapText, which re-flows on word boundaries, drops the
			// leading indent and clips a long model ID, so at a narrow width
			// the marker column stops meaning anything.
			m.messages = append(m.messages,
				ChatMessage{Role: "error", Content: unserved.Headline()},
				ChatMessage{Role: "agent", Content: fencedListing(unserved.Listing())})
		case err != nil:
			m.messages = append(m.messages, ChatMessage{Role: "error", Content: err.Error()})
		default:
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: notice})
		}
		return nil
	case slash.KindLoopStart:
		return m.startLoop(action)
	case slash.KindLoopStop:
		m.messages = append(m.messages, ChatMessage{Role: "agent", Content: m.stopLoop(action.LoopID)})
		return nil
	case slash.KindLoopList:
		m.messages = append(m.messages, ChatMessage{Role: "agent", Content: m.listLoops()})
		return nil
	case slash.KindGoalSet:
		m.session.SetGoal(action.Text)
		m.messages = append(m.messages, ChatMessage{Role: "agent", Content: theme.Goal + " Goal set: " + action.Text + "\nI'll pursue it on your next message, re-checking until it's met. Press Ctrl+C or /goal clear to stop."})
		return nil
	case slash.KindGoalShow:
		if g := m.session.Goal(); g != "" {
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: theme.Goal + " Current goal: " + g})
		} else {
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: "No goal set. Use /goal <text> to set one."})
		}
		return nil
	case slash.KindGoalClear:
		if m.session.Goal() != "" {
			m.session.ClearGoal()
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: "Goal cleared."})
		} else {
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: "No goal to clear."})
		}
		return nil
	case slash.KindAttach:
		switch action.AttachOp {
		case slash.AttachStage:
			m.pending = append(m.pending, attachstage.StagedFile{Path: action.AttachPath, Transcribe: action.Transcribe})
			mode := "default"
			if action.Transcribe {
				mode = "transcribe"
			}
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: "attached: " + filepath.Base(action.AttachPath) + " (" + mode + ") — sends with your next message"})
		case slash.AttachList:
			if len(m.pending) == 0 {
				m.messages = append(m.messages, ChatMessage{Role: "agent", Content: "nothing staged"})
			} else {
				var b strings.Builder
				for i, s := range m.pending {
					if i > 0 {
						b.WriteString("\n")
					}
					b.WriteString(filepath.Base(s.Path))
				}
				m.messages = append(m.messages, ChatMessage{Role: "agent", Content: b.String()})
			}
		case slash.AttachClear:
			n := len(m.pending)
			m.pending = nil
			m.messages = append(m.messages, ChatMessage{Role: "agent", Content: fmt.Sprintf("cleared %d staged attachment(s)", n)})
		}
		return nil
	default: // slash.KindSend
		files, overrides := attachstage.BuildSend(m.pending, action)
		m.loading = true
		m.interruptArmed = false
		m.status = ""
		if len(files) == 0 {
			return m.sendMessage(action.Text)
		}
		return m.sendWithAttachmentsCmd(action.Text, files, overrides)
	}
}

// sendMessage sends a message to the AI
func (m Model) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		response, err := m.session.SendMessage(text)
		return responseMsg{content: response, err: err}
	}
}

// sendWithAttachmentsCmd sends a message with staged + inline @path attachments.
// Blocked entries and clear-on-success are handled in the responseMsg handler.
func (m Model) sendWithAttachmentsCmd(text string, files []string, overrides map[string]attachments.Override) tea.Cmd {
	return func() tea.Msg {
		reply, blocked, err := m.session.SendWithAttachments(m.ctx, text, files, overrides)
		return responseMsg{content: reply, err: err, blocked: blocked}
	}
}

// shellTickMsg drives a periodic refresh of the shell-jobs footer.
type shellTickMsg struct{}

// shellTick schedules the next shell-jobs footer refresh.
func (m Model) shellTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return shellTickMsg{} })
}

// loopTickMsg drives the cron scheduler poll.
type loopTickMsg struct{}

// loopTick schedules the next scheduler poll.
func (m Model) loopTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return loopTickMsg{} })
}

// wakeupScheduledMsg is emitted when the agent schedules an in-session wake-up.
type wakeupScheduledMsg chat.WakeupRequest

// wakeupFireMsg is emitted when a scheduled wake-up's delay elapses. poll marks
// a tick that watches background work; it is validated against pollGen rather
// than wakeupGen so only poll ticks are dropped on park→resume.
type wakeupFireMsg struct {
	prompt string
	gen    int
	poll   bool
}

// listenWakeup waits for the agent to schedule a wake-up.
func (m Model) listenWakeup() tea.Cmd {
	return func() tea.Msg {
		select {
		case req := <-m.wakeupChan:
			return wakeupScheduledMsg(req)
		case <-m.ctx.Done():
			return nil
		}
	}
}

// listenPark waits for a park/resume signal from the live run.
func (m Model) listenPark() tea.Cmd {
	return func() tea.Msg {
		select {
		case ev := <-m.parkChan:
			return parkMsg(ev)
		case <-m.ctx.Done():
			return nil
		}
	}
}

// listenCompact waits for an auto-compaction notice from the session.
func (m Model) listenCompact() tea.Cmd {
	return func() tea.Msg {
		select {
		case v := <-m.compactChan:
			return compactNoticeMsg(v)
		case <-m.ctx.Done():
			return nil
		}
	}
}

// listenPrune waits for a tool-output pruning notice from the session.
func (m Model) listenPrune() tea.Cmd {
	return func() tea.Msg {
		select {
		case v := <-m.pruneChan:
			return pruneNoticeMsg(v)
		case <-m.ctx.Done():
			return nil
		}
	}
}

// compactCmd runs a manual /compact (an LLM call) off the event loop.
func (m Model) compactCmd() tea.Cmd {
	return func() tea.Msg {
		before, after, err := m.session.CompactHistory()
		return compactResultMsg{before: before, after: after, err: err}
	}
}

// listenStatus listens for status updates from the session
func (m Model) listenStatus() tea.Cmd {
	return func() tea.Msg {
		select {
		case status := <-m.statusChan:
			return statusMsg(status)
		case <-m.ctx.Done():
			return nil
		}
	}
}

// listenReasoning listens for reasoning updates from the session
func (m Model) listenReasoning() tea.Cmd {
	return func() tea.Msg {
		select {
		case reasoning := <-m.reasoningChan:
			return reasoningMsg(reasoning)
		case <-m.ctx.Done():
			return nil
		}
	}
}

// listenAgentEvents listens for sub-agent lifecycle events from the session
func (m Model) listenAgentEvents() tea.Cmd {
	return func() tea.Msg {
		select {
		case ev := <-m.agentEventChan:
			return agentEventMsg(ev)
		case <-m.ctx.Done():
			return nil
		}
	}
}

// listenToolRequest listens for tool call requests from the session
func (m Model) listenToolRequest() tea.Cmd {
	return func() tea.Msg {
		select {
		case req := <-m.toolRequestChan:
			return toolCallMsg(req)
		case <-m.ctx.Done():
			return nil
		}
	}
}

// listenToolResult listens for finished tool results from the session.
func (m Model) listenToolResult() tea.Cmd {
	return func() tea.Msg {
		select {
		case res := <-m.toolResultChan:
			return toolResultMsg(res)
		case <-m.ctx.Done():
			return nil
		}
	}
}

// listenAskRequest listens for ask_user requests from the session.
func (m Model) listenAskRequest() tea.Cmd {
	return func() tea.Msg {
		select {
		case req := <-m.askRequestChan:
			return askMsg(req)
		case <-m.ctx.Done():
			return nil
		}
	}
}

// handleToolApproval handles a free-form adjustment typed in edit mode. An empty
// input is treated as a plain approval. (Choice-mode keypresses are handled by
// the interception block in Update and never reach here.)
func (m Model) handleToolApproval(input string) (tea.Model, tea.Cmd) {
	input = strings.TrimSpace(input)
	if input == "" {
		return m.resolveApproval(chat.ToolCallResponse{Approved: true})
	}
	return m.resolveApproval(chat.ToolCallResponse{Approved: true, Adjustment: input})
}

// resolveApproval finalizes a tool-approval decision: tear down approval state,
// resume the spinner, and hand the response back to the waiting callback.
func (m Model) resolveApproval(resp chat.ToolCallResponse) (tea.Model, tea.Cmd) {
	m.awaitingApproval = false
	m.approvalEditing = false
	m.pendingTool = nil
	m.textarea.Reset()
	m.loading = true
	m.status = "Executing tool..."
	m.updateViewport()
	return m, func() tea.Msg {
		m.toolResponseChan <- resp
		return nil
	}
}

// updateDimensions updates component dimensions based on window size
func (m *Model) updateDimensions() {
	// Constrain height to maxHeight if set
	effectiveHeight := m.height
	if m.maxHeight > 0 && effectiveHeight > m.maxHeight {
		effectiveHeight = m.maxHeight
	}

	headerHeight := 2
	footerHeight := 3 // single-line input + help line + spacing
	statusHeight := 1

	vpHeight := effectiveHeight - headerHeight - footerHeight - statusHeight
	if vpHeight < 5 {
		vpHeight = 5
	}

	m.viewport.Width = m.width
	m.viewport.Height = vpHeight
	m.logVP.Width = m.width
	m.logVP.Height = vpHeight
	m.textarea.SetWidth(m.width - 2)
}

// truncateLine caps a single line at w runes, ending with an ellipsis. A
// non-positive budget returns the bare ellipsis rather than an unclamped line.
func truncateLine(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

// wrapText wraps text to fit within the specified width, preserving existing newlines
func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}

	var result strings.Builder
	lines := strings.Split(text, "\n")

	for _, line := range lines {
		if line == "" {
			result.WriteString("\n")
			continue
		}

		// Calculate the visual width (accounting for ANSI codes)
		visualWidth := lipgloss.Width(line)
		if visualWidth <= width {
			result.WriteString(line)
			result.WriteString("\n")
			continue
		}

		// Need to wrap this line
		words := strings.Fields(line)
		if len(words) == 0 {
			result.WriteString("\n")
			continue
		}

		currentLine := strings.Builder{}
		currentWidth := 0

		for i, word := range words {
			wordWidth := lipgloss.Width(word)

			// If a single word is longer than width, truncate it on a rune
			// boundary (byte slicing here would split a multibyte rune).
			if wordWidth > width && currentWidth == 0 {
				result.WriteString(truncateRunes(word, width))
				result.WriteString("\n")
				continue
			}

			if currentWidth > 0 {
				// Check if adding this word would exceed width
				if currentWidth+1+wordWidth > width {
					// Write current line and start new one
					result.WriteString(currentLine.String())
					result.WriteString("\n")
					currentLine.Reset()
					currentWidth = 0
				} else {
					// Add space before word
					currentLine.WriteString(" ")
					currentWidth += 1
				}
			}

			currentLine.WriteString(word)
			currentWidth += wordWidth

			// If this is the last word, write the line
			if i == len(words)-1 {
				result.WriteString(currentLine.String())
				result.WriteString("\n")
			}
		}
	}

	return result.String()
}

// truncateRunes shortens word to at most width display columns, breaking on a
// rune boundary and appending an ellipsis when there is room for it.
func truncateRunes(word string, width int) string {
	runes := []rune(word)
	if width <= 0 {
		return ""
	}
	if len(runes) <= width {
		return word
	}
	if width <= 1 {
		return string(runes[:width])
	}
	return string(runes[:width-1]) + "…"
}

// updateViewport updates the viewport content with chat messages
// markdownFor returns a glamour renderer for the given wrap width, building and
// caching one per distinct width. Returns nil on construction error (callers
// fall back to plain wrapText).
func (m *Model) markdownFor(width int) *glamour.TermRenderer {
	if width < 1 {
		width = 1
	}
	if m.mdRenderers == nil {
		m.mdRenderers = make(map[int]*glamour.TermRenderer)
	}
	if r, ok := m.mdRenderers[width]; ok {
		return r
	}
	r, err := nibMarkdownRenderer(width)
	if err != nil {
		return nil
	}
	m.mdRenderers[width] = r
	return r
}

// renderAgentThreadRun renders one contiguous run of a sub-agent's thread
// messages (agent_tool labels and/or agent_result blocks, all same AgentID) as
// an indented block. It prints a short continuation header (↳ <type>) when
// reprint is true (i.e. the previous rendered line did not belong to this
// agent), caps the tool lines, and renders each result with a → marker. The
// trailing blank separator is omitted when hugNext is true (the next message
// still belongs to this agent's run), keeping the whole thread visually tight.
func (m *Model) renderAgentThreadRun(sb *strings.Builder, run []ChatMessage, contentWidth int, reprint, hugNext bool) {
	if len(run) == 0 {
		return
	}
	if reprint {
		typ := "agent"
		if j, ok := m.jobByID(run[0].AgentID); ok && j.Type != "" {
			typ = j.Type
		}
		sb.WriteString(theme.Subtle.Render(theme.SubAgent + " " + typ))
		sb.WriteString("\n")
	}
	// Tool labels are capped; results render in full after them.
	var toolLines []string
	var results []ChatMessage
	for _, msg := range run {
		if msg.Role == "agent_result" {
			results = append(results, msg)
			continue
		}
		toolLines = append(toolLines, msg.Content)
	}
	for _, line := range capThreadLines(toolLines, agentThreadInlineCap) {
		sb.WriteString("   " + theme.Help.Render(clipLine(line, contentWidth-3)))
		sb.WriteString("\n")
	}
	for _, r := range results {
		wrapped := wrapText(r.Content, contentWidth-5)
		for i, line := range strings.Split(strings.TrimRight(wrapped, "\n"), "\n") {
			if i == 0 {
				sb.WriteString("   " + theme.Subtle.Render(theme.Arrow+" "+line))
			} else {
				sb.WriteString("     " + theme.Subtle.Render(line))
			}
			sb.WriteString("\n")
		}
	}
	if !hugNext {
		sb.WriteString("\n")
	}
}

// sameAgentMsg reports whether the message at idx is part of agentID's run —
// a lifecycle line, tool line, or result tagged with that id. Used to decide
// whether a thread item should hug the next one (omit the blank separator).
func (m *Model) sameAgentMsg(idx int, agentID string) bool {
	if agentID == "" || idx < 0 || idx >= len(m.messages) {
		return false
	}
	x := m.messages[idx]
	if x.AgentID != agentID {
		return false
	}
	return x.Role == "agent" || x.Role == "agent_tool" || x.Role == "agent_result"
}

func (m *Model) updateViewport() {
	var sb strings.Builder

	// Calculate available width for content (use viewport width, not terminal width)
	contentWidth := m.viewport.Width
	if contentWidth <= 0 {
		contentWidth = m.width
	}
	if contentWidth <= 0 {
		contentWidth = 80 // fallback
	}

	lastAgent := "" // id of the agent whose line was rendered last, "" for non-agent
	for i := 0; i < len(m.messages); i++ {
		msg := m.messages[i]

		// Group a contiguous run of one sub-agent's thread messages.
		if msg.Role == "agent_tool" || msg.Role == "agent_result" {
			j := i
			for j < len(m.messages) &&
				(m.messages[j].Role == "agent_tool" || m.messages[j].Role == "agent_result") &&
				m.messages[j].AgentID == msg.AgentID {
				j++
			}
			m.renderAgentThreadRun(&sb, m.messages[i:j], contentWidth, lastAgent != msg.AgentID, m.sameAgentMsg(j, msg.AgentID))
			lastAgent = msg.AgentID
			i = j - 1
			continue
		}

		// Track agent context so a following thread run knows whether to reprint.
		if msg.Role == "agent" && msg.AgentID != "" {
			lastAgent = msg.AgentID
		} else {
			lastAgent = ""
		}

		switch msg.Role {
		case "user":
			prefix := userStyle.Render("you") + " " + theme.SepStyle.Render(theme.Sep) + " "
			prefixWidth := lipgloss.Width(prefix)
			wrappedContent := wrapText(msg.Content, contentWidth-prefixWidth)
			// Add prefix to first line, indent continuation lines
			lines := strings.Split(strings.TrimRight(wrappedContent, "\n"), "\n")
			for i, line := range lines {
				if i == 0 {
					// First line: prefix + content
					sb.WriteString(prefix)
					sb.WriteString(line)
				} else {
					// Continuation lines: indent with spaces only (no prefix)
					sb.WriteString(strings.Repeat(" ", prefixWidth))
					sb.WriteString(line)
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		case "assistant":
			prefix := assistantStyle.Render(theme.BrandName) + " " + theme.SepStyle.Render(theme.Sep) + " "
			prefixWidth := lipgloss.Width(prefix)
			wrappedContent := renderMarkdownWith(m.markdownFor(contentWidth-prefixWidth), msg.Content, contentWidth-prefixWidth)
			// Add prefix to first line, indent continuation lines
			lines := strings.Split(strings.TrimRight(wrappedContent, "\n"), "\n")
			for i, line := range lines {
				if i == 0 {
					// First line: prefix + content
					sb.WriteString(prefix)
					sb.WriteString(line)
				} else {
					// Continuation lines: indent with spaces only (no prefix)
					sb.WriteString(strings.Repeat(" ", prefixWidth))
					sb.WriteString(line)
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		case "agent":
			prefix := theme.Subtle.Render(theme.SubAgent) + " "
			prefixWidth := lipgloss.Width(prefix)
			wrappedContent := renderMarkdownWith(m.markdownFor(contentWidth-prefixWidth), msg.Content, contentWidth-prefixWidth)
			lines := strings.Split(strings.TrimRight(wrappedContent, "\n"), "\n")
			for li, line := range lines {
				if li == 0 {
					sb.WriteString(prefix)
					sb.WriteString(agentStyle.Render(line))
				} else {
					sb.WriteString(strings.Repeat(" ", prefixWidth))
					sb.WriteString(agentStyle.Render(line))
				}
				sb.WriteString("\n")
			}
			// Tighten: a sub-agent lifecycle header hugs its own thread run that
			// follows (tool lines / result) — omit the blank separator.
			if !m.sameAgentMsg(i+1, msg.AgentID) {
				sb.WriteString("\n")
			}
		case "tool":
			// Calm, dim block: a header naming the tool, then the pretty/truncated
			// output indented and dimmed beneath it.
			label := msg.Name
			if msg.Arguments != "" {
				// First line of the friendly summary makes the clearest header.
				summary := chat.FormatToolCall(msg.Name, msg.Arguments)
				if nl := strings.IndexByte(summary, '\n'); nl >= 0 {
					summary = summary[:nl]
				}
				if summary != "" {
					label = summary
				}
			}
			if msg.AgentID != "" {
				label = theme.SubAgent + " " + shortID(msg.AgentID) + " · " + label
			}
			sb.WriteString(theme.Subtle.Render(theme.Sep + " " + label))
			sb.WriteString("\n")
			// Content is already previewed (truncated + pretty) at append time.
			wrapped := wrapText(msg.Content, contentWidth-2)
			for _, line := range strings.Split(strings.TrimRight(wrapped, "\n"), "\n") {
				sb.WriteString("  " + theme.Help.Render(line))
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		case "error":
			prefix := errorStyle.Render(theme.Cross) + " "
			prefixWidth := lipgloss.Width(prefix)
			wrappedContent := wrapText(msg.Content, contentWidth-prefixWidth)
			// Add prefix to first line, indent continuation lines
			lines := strings.Split(strings.TrimRight(wrappedContent, "\n"), "\n")
			for i, line := range lines {
				if i == 0 {
					// First line: prefix + content
					sb.WriteString(prefix)
					sb.WriteString(line)
				} else {
					// Continuation lines: indent with spaces only (no prefix)
					sb.WriteString(strings.Repeat(" ", prefixWidth))
					sb.WriteString(line)
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		}
	}

	if m.loading {
		displayStatus := m.status
		if displayStatus == "" || displayStatus == "Thinking..." {
			displayStatus = theme.Status(theme.VerbThinking, m.statusPhase)
		}
		sb.WriteString(theme.SepStyle.Render(theme.Sep) + " " + theme.Reasoning.Render(displayStatus))
		sb.WriteString("\n")
		if m.reasoning != "" {
			sb.WriteString(theme.ReasoningHeader() + "\n")
			wrapped := wrapText(m.reasoning, contentWidth-4)
			for _, line := range strings.Split(strings.TrimRight(wrapped, "\n"), "\n") {
				sb.WriteString("  " + theme.Reasoning.Render(line) + "\n")
			}
		}
	}

	if m.awaitingApproval && m.pendingTool != nil {
		gutter := theme.Gutter.Render(theme.ApprovalGutter) + " "
		sb.WriteString(gutter + theme.ApproveKey.Render(toolApprovalLabel(*m.pendingTool)))
		sb.WriteString("\n")
		if rows, ok := chat.ToolArgRows(m.pendingTool.Name, m.pendingTool.Arguments); ok {
			// Structured args render as an aligned card: dim keys padded to a
			// column; values truncated to the row (multi-line hints included).
			maxKey := 0
			for _, r := range rows {
				if len(r.Key) > maxKey {
					maxKey = len(r.Key)
				}
			}
			for _, r := range rows {
				key := r.Key + strings.Repeat(" ", maxKey-len(r.Key))
				val := truncateLine(r.ValueDisplay(), contentWidth-8-maxKey)
				sb.WriteString(gutter + "  " + theme.Meta.Render(key) + "  " + theme.Help.Render(val) + "\n")
			}
		} else {
			args := wrapText(chat.FormatToolCall(m.pendingTool.Name, m.pendingTool.Arguments), contentWidth-4)
			for _, line := range strings.Split(strings.TrimRight(args, "\n"), "\n") {
				sb.WriteString(gutter + theme.Help.Render(line) + "\n")
			}
		}
		if m.pendingTool.Reasoning != "" {
			rz := wrapText(m.pendingTool.Reasoning, contentWidth-4)
			for _, line := range strings.Split(strings.TrimRight(rz, "\n"), "\n") {
				sb.WriteString(gutter + theme.Reasoning.Render(line) + "\n")
			}
		}
		if m.approvalEditing {
			sb.WriteString(gutter + theme.ApproveKey.Render(theme.ApproveEditHint))
			sb.WriteString("\n")
		} else {
			scope, _ := chat.GrantScope(m.pendingTool.Name, m.pendingTool.Arguments)
			sb.WriteString(gutter + "\n")
			sb.WriteString(gutter + theme.ApproveKey.Render(theme.ApproveOnce) + "\n")
			sb.WriteString(gutter + theme.ApproveKey.Render(theme.ApproveAlwaysPrefix+scope+theme.ApproveAlwaysSuffix) + "\n")
			sb.WriteString(gutter + theme.ApproveKey.Render(theme.ApproveTurn) + "\n")
			sb.WriteString(gutter + theme.Help.Render(theme.ApproveDenyEdit) + "\n")
		}
	}

	if m.awaitingAsk && m.pendingAsk != nil {
		sb.WriteString(renderAsk(*m.pendingAsk, m.width))
		sb.WriteString("\n")
	}

	// Preserve the user's scroll position: only follow to the bottom when they
	// were already there. Otherwise a re-render (spinner tick, status update,
	// streamed token) would yank them back down while they're reading history.
	wasAtBottom := m.viewport.AtBottom()
	offset := m.viewport.YOffset
	m.viewport.SetContent(sb.String())
	if wasAtBottom {
		m.viewport.GotoBottom()
	} else {
		m.viewport.SetYOffset(offset)
	}
}

// View renders the TUI.
func (m Model) View() string {
	if m.quitting {
		return ""
	}

	var sb strings.Builder

	// Header: brand (plus a yolo badge when the approval gate is off) left,
	// cwd right, one dim hairline beneath.
	left := theme.Brand.Render(theme.BrandName)
	if m.cfg.ApprovalMode == "auto" {
		left += "  " + theme.Yolo.Render(theme.YoloBadge)
	}
	cwd := theme.Meta.Render(shortenPath(currentDir()))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(cwd)
	if gap < 1 {
		gap = 1
	}
	sb.WriteString(left + strings.Repeat(" ", gap) + cwd)
	sb.WriteString("\n")
	sb.WriteString(theme.Rule.Render(strings.Repeat("─", max(1, m.width))))
	sb.WriteString("\n")

	// Body: log viewer, first-run empty state, otherwise the conversation viewport.
	if m.showLogs {
		sb.WriteString(m.renderLogsViewer())
	} else if len(m.messages) == 0 && !m.loading && !m.awaitingApproval && !m.awaitingAsk {
		sb.WriteString(renderEmptyState(m.width))
	} else {
		sb.WriteString(m.viewport.View())
	}
	sb.WriteString("\n")

	// `/` completion popup, above the input.
	if comp := renderCompletion(m.completion, strings.TrimSpace(m.textarea.Value()), m.width); comp != "" {
		sb.WriteString(comp)
		sb.WriteString("\n")
	}

	// Pending message queue, above the input. Selection only matters when the
	// composer is empty (that's when up/down navigate it).
	if q := renderQueue(m.queue, m.queueSel, m.width); q != "" {
		sb.WriteString(q)
		sb.WriteString("\n")
	}

	// Input. In key-driven approval choice mode the textarea is hidden (the
	// approval block in the viewport carries the choice row); edit mode and
	// normal chat show the textarea.
	switch {
	case !m.sessionReady:
		sb.WriteString(theme.Help.Render(theme.Starting))
	case m.showLogs:
		// no input: the log viewer owns the body and the keystrokes
	case m.awaitingApproval && !m.approvalEditing:
		// no input: choice row lives in the viewport approval block
	default:
		sb.WriteString(m.textarea.View())
	}
	sb.WriteString("\n")
	help := theme.Help.Render(m.helpLine())
	if badge := m.footerBadges(lipgloss.Width(help)); badge != "" {
		gap := m.width - lipgloss.Width(help) - lipgloss.Width(badge)
		if gap < 1 {
			gap = 1
		}
		sb.WriteString(help + strings.Repeat(" ", gap) + badge)
	} else {
		sb.WriteString(help)
	}

	if m.err != nil {
		sb.WriteString("\n" + theme.Error.Render(theme.Cross+" "+m.err.Error()))
	}

	// Jobs footers (renderers restyle internally; nil-safe when empty). Hidden
	// while the log viewer owns the body.
	if !m.showLogs {
		if f := renderJobsFooter(m.jobs, m.width); f != "" {
			sb.WriteString("\n" + f)
		}
		if f := renderShellJobsFooter(m.shellJobs.List(), m.width); f != "" {
			sb.WriteString("\n" + f)
		}
		if f := renderLoopsFooter(m.loops, m.selfPaced, m.width); f != "" {
			sb.WriteString("\n" + f)
		}
		if m.session != nil {
			if f := renderGoalFooter(m.session.Goal(), m.width); f != "" {
				sb.WriteString("\n" + f)
			}
		}
	}

	return sb.String()
}

// helpLine returns the context-appropriate help string.
func (m Model) helpLine() string {
	switch {
	case m.showLogs && m.logOpenID != "":
		return theme.ScrollKeys + " scroll · esc back · ctrl+o close"
	case m.showLogs:
		return theme.ScrollKeys + " select · enter open · k kill · esc close"
	case m.awaitingApproval && m.approvalEditing:
		return theme.HelpApprovalEdit
	case m.awaitingApproval:
		return theme.HelpApproval
	case m.parked:
		return "enter add a follow-up · ctrl+c interrupt · ctrl+o logs"
	case strings.TrimSpace(m.textarea.Value()) == "" && len(m.queue) > 0:
		return "↑↓ pick · ^e edit · ^x delete"
	default:
		return theme.HelpDefault
	}
}

func currentDir() string {
	d, err := os.Getwd()
	if err != nil {
		return ""
	}
	return d
}

// shortenPath replaces the home-dir prefix with ~ for a compact header.
func shortenPath(p string) string {
	if p == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(filepath.Separator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}

// Output returns any command that should be output to the shell
func (m Model) Output() string {
	return m.output
}

// SessionUsage reports what the session spent, for the exit summary RunTUI
// prints once the program has stopped rendering.
func (m Model) SessionUsage() chat.SessionUsage { return m.sessionUsage }

// isWorking reports whether a turn or sub-agent is currently running, or the
// run is parked (alive, waiting on the injection channel). A parked run is still
// in flight, so the first Ctrl+C should interrupt it rather than quit.
func (m Model) isWorking() bool {
	if m.loading || m.parked {
		return true
	}
	if m.session != nil && m.session.AgentManager() != nil {
		return m.session.AgentManager().HasRunning()
	}
	return false
}

// prunedNotice formats a one-line tool-output pruning summary for the
// transcript. It repeats cmd.pruneNotice's wording because tui cannot import
// cmd (cmd builds the TUI) — including its silence about staleness, since the
// high-water sweep prunes on size alone and calling those results stale would
// be untrue. The saving uses HumanTokensOrZero: a stale read is stubbed however
// small it was, so a pass can free nothing measurable and HumanTokens renders
// 0 as "".
//
// It carries the same "~" and "(estimated)" markers as compactNotice below,
// because the figure is the same byte/4 estimate: an unmarked number reads as
// measured, and a user cannot tell the difference. The count is exact and stays
// unmarked.
func prunedNotice(results, freed int) string {
	noun := "results"
	if results == 1 {
		noun = "result"
	}
	return fmt.Sprintf("pruned %d tool %s — freed ~%s tokens (estimated)", results, noun, chat.HumanTokensOrZero(freed))
}

// compactNotice formats a one-line compaction summary for the transcript. It
// repeats cmd.compactNotice's wording — tui cannot import cmd, because cmd
// builds the TUI — including the "~" and "(estimated)" markers: the figures are
// byte/4 estimates, not the backend's reported usage, and an unmarked number
// reads as measured. Both figures use HumanTokensOrZero for the same reason
// pruneNotice above does: HumanTokens renders 0 as "", which would print
// "~ → ~ tokens" — a hole where the number belongs.
func compactNotice(before, after int) string {
	return fmt.Sprintf("Compacted conversation — ~%s → ~%s tokens (estimated)",
		chat.HumanTokensOrZero(before), chat.HumanTokensOrZero(after))
}

// ctxBadgeWarn highlights the context badge once usage nears the auto-compaction
// threshold (clay — the palette's warmest attention color).
var ctxBadgeWarn = lipgloss.NewStyle().Foreground(theme.Accent)

// contextBudget is the number the badge measures against: the window the
// session is really using, less the reserve held back for the response. It is
// what chat.shouldAutoCompact takes its threshold of, so a badge drawn against
// it reaches 100% exactly where compaction fires.
//
// Both halves used to be wrong. The raw cfg.Compaction.MaxContextTokens ignores
// the reserve (a few percent at 128k) and, worse, ignores a window learned from
// a backend overflow error: a session configured for 400k against a model that
// really serves 262k drew a calm badge while compaction ran. The session is the
// authority whenever there is one; the config is the fallback for a model with
// no session yet.
func (m Model) contextBudget() int {
	window := m.cfg.Compaction.MaxContextTokens
	if m.session != nil {
		window = m.session.ContextWindow()
	}
	return chat.ContextBudget(m.cfg.Compaction, window)
}

// contextBadge renders the right-aligned context-size indicator for the bottom
// bar, e.g. "ctx 8k (6%)". It highlights once usage reaches the auto-compaction
// threshold. Returns "" when there's nothing to show yet.
func (m Model) contextBadge() string {
	used := m.contextTokens
	if used <= 0 {
		return ""
	}
	budget := m.contextBudget()
	if budget <= 0 {
		// No window at all (auto-compaction off): show the bare size.
		return theme.Meta.Render("ctx " + chat.HumanTokens(used))
	}
	pct := used * 100 / budget
	label := fmt.Sprintf("ctx %s (%d%%)", chat.HumanTokens(used), pct)
	threshold := m.cfg.Compaction.Threshold
	if threshold <= 0 || threshold > 1 {
		threshold = 0.8
	}
	if float64(used) >= float64(budget)*threshold {
		return ctxBadgeWarn.Render(label)
	}
	return theme.Meta.Render(label)
}

// usageBadge renders the session's cumulative token spend for the bottom bar,
// e.g. "session 312k in / 18.4k out". Returns "" before anything is spent, the
// same way contextBadge does.
//
// Plain words rather than arrows: it matches contextBadge's phrasing and the
// calm editorial voice TestNoEmojiInRenderHelpers guards.
//
// chat.HumanTokensOrZero, not chat.HumanTokens: the badge names both directions,
// so a zero one has to render "0" rather than the empty string HumanTokens
// returns, or the badge shows "session 1.2k in /  out". The exit summary prints
// the same fixed shape and shares the same formatter.
func (m Model) usageBadge() string {
	in, out := m.sessionUsage.PromptTokens, m.sessionUsage.CompletionTokens
	if in <= 0 && out <= 0 {
		return ""
	}
	return theme.Meta.Render(fmt.Sprintf("session %s in / %s out",
		chat.HumanTokensOrZero(in), chat.HumanTokensOrZero(out)))
}

// footerBadges renders the right-aligned bottom-bar badges for a help line of
// helpWidth columns.
//
// When both do not fit, the usage badge is dropped WHOLE — never truncated,
// never abbreviated. The context badge earns the space because it predicts
// auto-compaction and is therefore actionable, while the session total is
// informational and still reaches the user through the exit summary and
// usage.json. Narrow terminals are the common case here, not an edge case:
// nib is routinely run through ttyd in a browser.
func (m Model) footerBadges(helpWidth int) string {
	ctx, usage := m.contextBadge(), m.usageBadge()
	if usage == "" {
		return ctx
	}
	if ctx == "" {
		if helpWidth+lipgloss.Width(usage)+1 > m.width {
			return ""
		}
		return usage
	}
	both := usage + "  " + ctx
	if helpWidth+lipgloss.Width(both)+1 > m.width {
		return ctx
	}
	return both
}

// quit tears down the session and exits.
//
// The usage refresh comes BEFORE Close, and is not decoration. Close writes
// usage.json from the session's live counter, while RunTUI prints the exit
// summary from this cached field, so skipping the refresh lets the two disagree
// about the same run: a second Ctrl+C during the first turn left usage.json
// holding the real spend and the terminal printing nothing at all, because
// FormatSessionSummary renders "" for zero. Reading first makes both surfaces
// quote the same snapshot.
func (m Model) quit() (tea.Model, tea.Cmd) {
	m.quitting = true
	if m.session != nil {
		m.sessionUsage = m.session.Usage()
		m.session.Close()
	}
	m.cancel()
	return m, tea.Quit
}
