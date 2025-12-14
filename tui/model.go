package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
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
	cfg          chat.Config
	sessionReady bool

	// UI state
	width     int
	height    int
	loading   bool
	status    string
	reasoning string
	err       error
	output    string // Command to output to shell on exit
	quitting  bool

	// Tool approval state
	pendingTool      *chat.ToolCallRequest
	awaitingApproval bool
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
func NewModel(ctx context.Context, cfg chat.Config, height int, transports ...mcp.Transport) Model {
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
	s.Spinner = spinner.Dot

	return Model{
		viewport:   vp,
		textarea:   ta,
		spinner:    s,
		messages:   []ChatMessage{},
		ctx:        ctx,
		cancel:     cancel,
		transports: transports,
		cfg:        cfg,
		height:     height,
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
				// Status updates are handled via program.Send in the async goroutine
			},
			OnReasoning: func(reasoning string) {
				// Reasoning updates are handled via program.Send in the async goroutine
			},
			OnToolCall: func(req chat.ToolCallRequest) chat.ToolCallResponse {
				// Tool calls will be handled synchronously for now
				// In a more advanced implementation, this would be async
				return chat.ToolCallResponse{Approved: true}
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

	case reasoningMsg:
		m.reasoning = string(msg)
		m.updateViewport()

	case toolCallMsg:
		m.pendingTool = (*chat.ToolCallRequest)(&msg)
		m.awaitingApproval = true
		m.updateViewport()

	case spinner.TickMsg:
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
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

// handleToolApproval handles tool approval input
func (m Model) handleToolApproval(input string) (tea.Model, tea.Cmd) {
	m.awaitingApproval = false
	m.pendingTool = nil
	m.textarea.Reset()
	m.updateViewport()
	return m, nil
}

// updateDimensions updates component dimensions based on window size
func (m *Model) updateDimensions() {
	headerHeight := 2
	footerHeight := 5 // textarea + border
	statusHeight := 1

	vpHeight := m.height - headerHeight - footerHeight - statusHeight
	if vpHeight < 5 {
		vpHeight = 5
	}

	m.viewport.Width = m.width
	m.viewport.Height = vpHeight
	m.textarea.SetWidth(m.width - 2)
}

// updateViewport updates the viewport content with chat messages
func (m *Model) updateViewport() {
	var sb strings.Builder

	for _, msg := range m.messages {
		switch msg.Role {
		case "user":
			sb.WriteString(userStyle.Render("You: "))
			sb.WriteString(msg.Content)
			sb.WriteString("\n\n")
		case "assistant":
			sb.WriteString(assistantStyle.Render("Assistant: "))
			sb.WriteString(msg.Content)
			sb.WriteString("\n\n")
		case "error":
			sb.WriteString(errorStyle.Render("Error: "))
			sb.WriteString(msg.Content)
			sb.WriteString("\n\n")
		}
	}

	if m.loading {
		sb.WriteString(statusStyle.Render(m.spinner.View() + " " + m.status))
		sb.WriteString("\n")
		if m.reasoning != "" {
			sb.WriteString(reasoningStyle.Render("Reasoning: " + m.reasoning))
			sb.WriteString("\n")
		}
	}

	if m.awaitingApproval && m.pendingTool != nil {
		sb.WriteString(toolStyle.Render(fmt.Sprintf(
			"Tool Request: %s\nArguments: %s\nReasoning: %s\n\nType 'y' to approve, 'n' to deny, or provide adjustment:",
			m.pendingTool.Name,
			m.pendingTool.Arguments,
			m.pendingTool.Reasoning,
		)))
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
