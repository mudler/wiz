package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/mudler/aish/types"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mudler/aish/chat"
)

// ChatMessage represents a message in the chat history
type ChatMessage struct {
	Role    string
	Content string
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
	cfg          types.Config
	sessionReady bool

	// UI state
	width     int
	height    int
	maxHeight int // Configured max height (0 = no limit)
	loading   bool
	status    string
	reasoning string
	err       error
	output    string // Command to output to shell on exit
	quitting  bool

	// Tool approval state
	pendingTool      *chat.ToolCallRequest
	awaitingApproval bool

	// Animation state
	statusPhase int

	// Channels for async communication with callbacks
	statusChan       chan string
	reasoningChan    chan string
	toolRequestChan  chan chat.ToolCallRequest
	toolResponseChan chan chat.ToolCallResponse
}

// responseMsg is sent when the AI responds
type responseMsg struct {
	content string
	err     error
}

// statusMsg is sent for status updates
type statusMsg string

// reasoningMsg is sent for reasoning updates
type reasoningMsg string

// toolCallMsg is sent when a tool call needs approval
type toolCallMsg chat.ToolCallRequest

// sessionReadyMsg is sent when the session is initialized
type sessionReadyMsg struct {
	session *chat.Session
	err     error
}

// NewModel creates a new TUI model
func NewModel(ctx context.Context, cfg types.Config, height int, transports ...mcp.Transport) Model {
	ctx, cancel := context.WithCancel(ctx)

	ta := textarea.New()
	ta.Placeholder = "Ask the assistant..."
	ta.Focus()
	ta.Prompt = "│ "
	ta.CharLimit = 4096
	ta.SetWidth(80)
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.KeyMap.InsertNewline.SetEnabled(false) // Enter sends message

	vp := viewport.New(80, 10)
	vp.SetContent("Welcome! Type your question and press Enter.\n\nPress Esc to exit.")

	s := spinner.New()
	s.Spinner = spinner.Points
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))

	// Calculate max height - negative means percentage, positive means lines
	maxH := height
	if maxH < 0 {
		maxH = 0 // Will be calculated on first WindowSizeMsg
	}

	return Model{
		viewport:         vp,
		textarea:         ta,
		spinner:          s,
		messages:         []ChatMessage{},
		ctx:              ctx,
		cancel:           cancel,
		maxHeight:        maxH,
		transports:       transports,
		cfg:              cfg,
		height:           height,
		statusChan:       make(chan string, 10),
		reasoningChan:    make(chan string, 10),
		toolRequestChan:  make(chan chat.ToolCallRequest),
		toolResponseChan: make(chan chat.ToolCallResponse),
	}
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
		}

		session, err := chat.NewSession(m.ctx, m.cfg, callbacks, m.transports...)
		return sessionReadyMsg{session: session, err: err}
	}
}

// Update handles messages and updates the model
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.quitting = true
			if m.session != nil {
				m.session.Close()
			}
			m.cancel()
			return m, tea.Quit

		case tea.KeyEnter:
			if m.loading || !m.sessionReady {
				return m, nil
			}

			input := strings.TrimSpace(m.textarea.Value())
			if input == "" {
				return m, nil
			}

			// Check if we're in tool approval mode
			if m.awaitingApproval {
				return m.handleToolApproval(input)
			}

			// Add user message
			m.messages = append(m.messages, ChatMessage{
				Role:    "user",
				Content: input,
			})
			m.textarea.Reset()
			m.loading = true
			m.status = "Thinking..."
			m.updateViewport()

			return m, m.sendMessage(input)
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
		// Start listening for callbacks
		cmds = append(cmds, m.listenStatus(), m.listenReasoning(), m.listenToolRequest())

	case responseMsg:
		m.loading = false
		m.status = ""
		m.reasoning = ""
		if msg.err != nil {
			m.err = msg.err
			m.messages = append(m.messages, ChatMessage{
				Role:    "error",
				Content: msg.err.Error(),
			})
		} else {
			m.messages = append(m.messages, ChatMessage{
				Role:    "assistant",
				Content: msg.content,
			})
		}
		m.updateViewport()

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
		m.loading = false // Allow user input for approval
		m.updateViewport()
		// Continue listening for more tool requests
		cmds = append(cmds, m.listenToolRequest())

	case spinner.TickMsg:
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
		// Rotate status phase for animated messages
		if m.loading {
			m.statusPhase = (m.statusPhase + 1) % 12
			m.updateViewport()
		}
	}

	// Update textarea
	if !m.loading {
		m.textarea, cmd = m.textarea.Update(msg)
		cmds = append(cmds, cmd)
	}

	// Update viewport
	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// sendMessage sends a message to the AI
func (m Model) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		response, err := m.session.SendMessage(text)
		return responseMsg{content: response, err: err}
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

// handleToolApproval handles tool approval input
func (m Model) handleToolApproval(input string) (tea.Model, tea.Cmd) {
	input = strings.ToLower(strings.TrimSpace(input))

	var response chat.ToolCallResponse
	switch input {
	case "y", "yes":
		response = chat.ToolCallResponse{Approved: true}
	case "a", "always":
		response = chat.ToolCallResponse{Approved: true, AlwaysAllow: true}
	case "n", "no":
		response = chat.ToolCallResponse{Approved: false}
	default:
		// Treat as adjustment
		response = chat.ToolCallResponse{Approved: true, Adjustment: input}
	}

	m.awaitingApproval = false
	m.pendingTool = nil
	m.textarea.Reset()
	m.loading = true
	m.status = "Executing tool..."
	m.updateViewport()

	// Send response back to the waiting callback
	return m, func() tea.Msg {
		m.toolResponseChan <- response
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
	footerHeight := 5 // textarea + border
	statusHeight := 1

	vpHeight := effectiveHeight - headerHeight - footerHeight - statusHeight
	if vpHeight < 5 {
		vpHeight = 5
	}

	m.viewport.Width = m.width
	m.viewport.Height = vpHeight
	m.textarea.SetWidth(m.width - 2)
}

// getThinkingStatus returns an animated thinking status message
func (m *Model) getThinkingStatus() string {
	phases := []string{
		"Thinking",
		"Thinking.",
		"Thinking..",
		"Thinking...",
		"Processing",
		"Processing.",
		"Processing..",
		"Processing...",
		"Analyzing",
		"Analyzing.",
		"Analyzing..",
		"Analyzing...",
	}
	return phases[m.statusPhase%len(phases)]
}

// updateViewport updates the viewport content with chat messages
func (m *Model) updateViewport() {
	var sb strings.Builder

	for _, msg := range m.messages {
		switch msg.Role {
		case "user":
			sb.WriteString(userStyle.Render("👤 You: "))
			sb.WriteString(msg.Content)
			sb.WriteString("\n\n")
		case "assistant":
			sb.WriteString(assistantStyle.Render("🤖 Assistant: "))
			sb.WriteString(msg.Content)
			sb.WriteString("\n\n")
		case "error":
			sb.WriteString(errorStyle.Render("✗ Error: "))
			sb.WriteString(msg.Content)
			sb.WriteString("\n\n")
		}
	}

	if m.loading {
		// Use animated status if no specific status is set
		displayStatus := m.status
		if displayStatus == "" || displayStatus == "Thinking..." {
			displayStatus = m.getThinkingStatus()
		}

		// Build thinking box content
		var thinkingContent strings.Builder
		thinkingContent.WriteString(thinkingStyle.Render(m.spinner.View() + " " + displayStatus))
		if m.reasoning != "" {
			thinkingContent.WriteString("\n")
			thinkingContent.WriteString(reasoningStyle.Render("💭 " + m.reasoning))
		}

		sb.WriteString(thinkingBoxStyle.Render(thinkingContent.String()))
		sb.WriteString("\n")
	}

	if m.awaitingApproval && m.pendingTool != nil {
		// Build tool request box content
		var toolContent strings.Builder
		toolContent.WriteString(toolNameStyle.Render("🔧 " + m.pendingTool.Name))
		toolContent.WriteString("\n\n")
		toolContent.WriteString(dimmedStyle.Render("Arguments: "))
		toolContent.WriteString(m.pendingTool.Arguments)
		if m.pendingTool.Reasoning != "" {
			toolContent.WriteString("\n")
			toolContent.WriteString(reasoningStyle.Render("💭 " + m.pendingTool.Reasoning))
		}
		toolContent.WriteString("\n\n")
		toolContent.WriteString(promptHintStyle.Render("[y]es  [a]lways  [n]o  "))
		toolContent.WriteString(dimmedStyle.Render("or type adjustment"))

		sb.WriteString(toolRequestBoxStyle.Render(toolContent.String()))
		sb.WriteString("\n")
	}

	m.viewport.SetContent(sb.String())
	m.viewport.GotoBottom()
}

// View renders the TUI
func (m Model) View() string {
	if m.quitting {
		return ""
	}

	var sb strings.Builder

	// Header
	sb.WriteString(headerStyle.Render("🤖 AI Shell Assistant"))
	sb.WriteString("\n")
	sb.WriteString(strings.Repeat("─", m.width))
	sb.WriteString("\n")

	// Chat viewport
	sb.WriteString(m.viewport.View())
	sb.WriteString("\n")

	// Separator
	sb.WriteString(strings.Repeat("─", m.width))
	sb.WriteString("\n")

	// Input area
	if m.sessionReady {
		sb.WriteString(m.textarea.View())
	} else {
		sb.WriteString(m.spinner.View() + " Initializing session...")
	}

	// Help text
	sb.WriteString("\n")
	sb.WriteString(helpStyle.Render("Enter: send • Esc: exit"))

	if m.err != nil {
		sb.WriteString("\n")
		sb.WriteString(errorStyle.Render(fmt.Sprintf("Error: %v", m.err)))
	}

	return sb.String()
}

// Output returns any command that should be output to the shell
func (m Model) Output() string {
	return m.output
}
