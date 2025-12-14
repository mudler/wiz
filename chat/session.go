package chat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"

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
	Approved   bool
	Adjustment string
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
	ctx       context.Context
	llm       cogito.LLM
	clients   []*mcp.ClientSession
	fragment  cogito.Fragment
	messages  []openai.ChatCompletionMessage
	callbacks Callbacks
}

// Config holds configuration for creating a new session
type Config struct {
	Model   string
	APIKey  string
	BaseURL string
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
func NewSession(ctx context.Context, cfg Config, callbacks Callbacks, transports ...mcp.Transport) (*Session, error) {
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
		ctx:       ctx,
		llm:       llm,
		clients:   clients,
		fragment:  cogito.NewEmptyFragment(),
		messages:  []openai.ChatCompletionMessage{},
		callbacks: callbacks,
	}, nil
}

// SendMessage sends a message to the assistant and processes the response
func (s *Session) SendMessage(text string) (string, error) {
	s.fragment = s.fragment.AddMessage("user", text)
	s.messages = append(s.messages, openai.ChatCompletionMessage{
		Role:    "user",
		Content: text,
	})

	var err error
	s.fragment, err = cogito.ExecuteTools(
		s.llm, s.fragment,
		cogito.WithContext(s.ctx),
		cogito.WithIterations(10),
		cogito.WithMaxAttempts(3),
		cogito.WithMaxRetries(3),
		cogito.WithForceReasoning(),
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

			return cogito.ToolCallDecision{
				Approved:   resp.Approved,
				Adjustment: resp.Adjustment,
			}
		}),
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

