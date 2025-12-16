package chat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"

	"github.com/mudler/aish/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mudler/cogito"
	openai "github.com/sashabaranov/go-openai"
)

// Message represents a chat message
type Message struct {
	Role    string
	Content string
}

// ToolCallRequest contains information about a tool the agent wants to run
type ToolCallRequest struct {
	Name      string
	Arguments string
	Reasoning string
}

// ToolCallResponse represents the user's decision on a tool call
type ToolCallResponse struct {
	Approved    bool
	Adjustment  string
	AlwaysAllow bool // Add tool to session allow list
}

// Callbacks defines the interface for UI interactions
type Callbacks struct {
	// OnStatus is called when there's a status update
	OnStatus func(status string)
	// OnReasoning is called when the agent is reasoning
	OnReasoning func(reasoning string)
	// OnToolCall is called when the agent wants to run a tool
	// Returns the user's decision
	OnToolCall func(req ToolCallRequest) ToolCallResponse
	// OnResponse is called when the agent responds
	OnResponse func(response string)
	// OnError is called when an error occurs
	OnError func(err error)
}

// Session represents a chat session with the AI assistant
type Session struct {
	ctx           context.Context
	llm           cogito.LLM
	clients       []*mcp.ClientSession
	fragment      cogito.Fragment
	messages      []openai.ChatCompletionMessage
	callbacks     Callbacks
	systemPrompt  string
	cogitoOptions types.AgentOptions
	allowedTools  map[string]bool // Tools that don't need approval this session
}

// CommandTransport creates a new transport for a command
func CommandTransport(cmd string, args []string, env ...string) mcp.Transport {
	command := exec.Command(cmd, args...)
	command.Env = os.Environ()
	command.Env = append(command.Env, env...)

	transport := &mcp.CommandTransport{Command: command}
	return transport
}

// NewSession creates a new chat session
func NewSession(ctx context.Context, cfg types.Config, callbacks Callbacks, transports ...mcp.Transport) (*Session, error) {
	llm := cogito.NewOpenAILLM(cfg.Model, cfg.APIKey, cfg.BaseURL)

	client := mcp.NewClient(&mcp.Implementation{Name: "aish", Version: "v1.0.0"}, nil)
	clients := []*mcp.ClientSession{}

	for _, transport := range transports {
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			return nil, err
		}
		clients = append(clients, session)
	}

	return &Session{
		ctx:           ctx,
		llm:           llm,
		clients:       clients,
		fragment:      cogito.NewEmptyFragment(),
		messages:      []openai.ChatCompletionMessage{},
		callbacks:     callbacks,
		systemPrompt:  cfg.GetPrompt(),
		cogitoOptions: cfg.AgentOptions,
		allowedTools:  make(map[string]bool),
	}, nil
}

// SendMessage sends a message to the assistant and processes the response
func (s *Session) SendMessage(text string) (string, error) {
	if s.systemPrompt != "" {
		s.fragment = s.fragment.AddMessage("system", s.systemPrompt)
	}
	s.fragment = s.fragment.AddMessage("user", text)
	s.messages = append(s.messages, openai.ChatCompletionMessage{
		Role:    "user",
		Content: text,
	})

	// Build cogito options from config
	cogitoOpts := []cogito.Option{
		cogito.WithContext(s.ctx),
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
		cogito.WithMCPs(s.clients...),
		cogito.WithToolCallBack(func(tool *cogito.ToolChoice, state *cogito.SessionState) cogito.ToolCallDecision {
			// Check if tool is in the allow list
			if s.allowedTools[tool.Name] {
				return cogito.ToolCallDecision{Approved: true}
			}

			if s.callbacks.OnToolCall == nil {
				return cogito.ToolCallDecision{Approved: true}
			}

			args, err := json.Marshal(tool.Arguments)
			if err != nil {
				return cogito.ToolCallDecision{Approved: false}
			}

			resp := s.callbacks.OnToolCall(ToolCallRequest{
				Name:      tool.Name,
				Arguments: string(args),
				Reasoning: tool.Reasoning,
			})

			// Add to allow list if requested
			if resp.AlwaysAllow && resp.Approved {
				s.allowedTools[tool.Name] = true
			}

			return cogito.ToolCallDecision{
				Approved:   resp.Approved,
				Adjustment: resp.Adjustment,
			}
		}),
	}

	// Add ForceReasoning only if enabled in config
	if s.cogitoOptions.ForceReasoning {
		cogitoOpts = append(cogitoOpts, cogito.WithForceReasoning())
	}

	var err error
	s.fragment, err = cogito.ExecuteTools(
		s.llm, s.fragment,
		cogitoOpts...,
	)

	if err != nil && !errors.Is(err, cogito.ErrNoToolSelected) {
		if s.callbacks.OnError != nil {
			s.callbacks.OnError(err)
		}
		return "", err
	}

	s.fragment, err = s.llm.Ask(context.Background(), s.fragment)
	if err != nil {
		if s.callbacks.OnError != nil {
			s.callbacks.OnError(err)
		}
		return "", err
	}

	response := s.fragment.LastMessage().Content
	s.messages = append(s.messages, openai.ChatCompletionMessage{
		Role:    "assistant",
		Content: response,
	})

	if s.callbacks.OnResponse != nil {
		s.callbacks.OnResponse(response)
	}

	return response, nil
}

// GetMessages returns all messages in the conversation
func (s *Session) GetMessages() []Message {
	messages := []Message{}
	for _, msg := range s.messages {
		messages = append(messages, Message{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}
	return messages
}

// Close closes the session and cleans up resources
func (s *Session) Close() error {
	for _, client := range s.clients {
		if err := client.Close(); err != nil {
			return err
		}
	}
	return nil
}
