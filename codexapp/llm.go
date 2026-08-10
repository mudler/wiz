// Package codexapp adapts the Codex app-server JSON-RPC protocol to cogito.LLM.
package codexapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/mudler/cogito"
	openai "github.com/sashabaranov/go-openai"
)

var ErrNoResponse = errors.New("codex app-server completed without an assistant message")

type Config struct {
	Command string
	Args    []string
	Model   string
	Cwd     string
}

type LLM struct{ config Config }

func New(config Config) *LLM {
	if config.Command == "" {
		config.Command = "codex"
	}
	if len(config.Args) == 0 {
		config.Args = []string{"app-server", "--stdio"}
	}
	return &LLM{config: config}
}

func (l *LLM) Ask(ctx context.Context, fragment cogito.Fragment) (cogito.Fragment, error) {
	reply, usage, err := l.CreateChatCompletion(ctx, openai.ChatCompletionRequest{Model: l.config.Model, Messages: fragment.GetMessages()})
	if err != nil {
		return fragment, err
	}
	if len(reply.ChatCompletionResponse.Choices) == 0 {
		return fragment, ErrNoResponse
	}
	fragment.Messages = append(fragment.Messages, reply.ChatCompletionResponse.Choices[0].Message)
	if fragment.Status != nil {
		fragment.Status.LastUsage = usage
		fragment.Status.CumulativeUsage.PromptTokens += usage.PromptTokens
		fragment.Status.CumulativeUsage.CompletionTokens += usage.CompletionTokens
		fragment.Status.CumulativeUsage.TotalTokens += usage.TotalTokens
	}
	return fragment, nil
}

func (l *LLM) CreateChatCompletion(ctx context.Context, request openai.ChatCompletionRequest) (cogito.LLMReply, cogito.LLMUsage, error) {
	completion, err := l.complete(ctx, request)
	if err != nil {
		return cogito.LLMReply{}, cogito.LLMUsage{}, err
	}
	finishReason := openai.FinishReasonStop
	if len(completion.ToolCalls) > 0 {
		finishReason = openai.FinishReasonToolCalls
	}
	response := openai.ChatCompletionResponse{Model: request.Model, Choices: []openai.ChatCompletionChoice{{
		Index: 0, Message: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant,
			Content: completion.Content, ToolCalls: completion.ToolCalls}, FinishReason: finishReason,
	}}}
	return cogito.LLMReply{ChatCompletionResponse: response}, cogito.LLMUsage{}, nil
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcClient struct {
	enc     *json.Encoder
	scanner *bufio.Scanner
	nextID  int
	mu      sync.Mutex
}

func (c *rpcClient) send(method string, params any) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	err := c.enc.Encode(map[string]any{"id": c.nextID, "method": method, "params": params})
	return c.nextID, err
}

func (c *rpcClient) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(map[string]any{"method": method, "params": params})
}

func (c *rpcClient) receive() (rpcMessage, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return rpcMessage{}, err
		}
		return rpcMessage{}, io.EOF
	}
	var msg rpcMessage
	if err := json.Unmarshal(c.scanner.Bytes(), &msg); err != nil {
		return rpcMessage{}, err
	}
	return msg, nil
}

func (c *rpcClient) response(id int, into any) error {
	for {
		msg, err := c.receive()
		if err != nil {
			return err
		}
		if string(msg.ID) != fmt.Sprint(id) {
			continue
		}
		if msg.Error != nil {
			return fmt.Errorf("app-server RPC error %d: %s", msg.Error.Code, msg.Error.Message)
		}
		if into == nil {
			return nil
		}
		return json.Unmarshal(msg.Result, into)
	}
}

type completion struct {
	Content   string
	ToolCalls []openai.ToolCall
}

func (l *LLM) complete(ctx context.Context, request openai.ChatCompletionRequest) (completion, error) {
	cmd := exec.CommandContext(ctx, l.config.Command, l.config.Args...)
	if l.config.Cwd != "" {
		cmd.Dir = l.config.Cwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return completion{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return completion{}, err
	}
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return completion{}, fmt.Errorf("start codex app-server: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	rpc := &rpcClient{enc: json.NewEncoder(stdin), scanner: bufio.NewScanner(stdout)}
	rpc.scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	initID, err := rpc.send("initialize", map[string]any{"clientInfo": map[string]string{"name": "nib", "version": "dev"}})
	if err != nil {
		return completion{}, err
	}
	if err := rpc.response(initID, nil); err != nil {
		return completion{}, withStderr(err, &stderr)
	}
	if err := rpc.notify("initialized", map[string]any{}); err != nil {
		return completion{}, err
	}

	base, input := splitMessages(request.Messages)
	threadCwd := l.config.Cwd
	if threadCwd == "" {
		threadCwd, err = os.MkdirTemp("", "nib-codexapp-")
		if err != nil {
			return completion{}, fmt.Errorf("create isolated app-server workspace: %w", err)
		}
		defer os.RemoveAll(threadCwd)
	}
	model := request.Model
	if l.config.Model != "" {
		model = l.config.Model
	}
	threadParams := map[string]any{
		"ephemeral": true, "approvalPolicy": "never", "sandbox": "read-only",
		"baseInstructions": base, "cwd": threadCwd,
	}
	if tools := dynamicTools(request.Tools); len(tools) > 0 {
		threadParams["dynamicTools"] = tools
	}
	if model != "" {
		threadParams["model"] = model
	}
	threadID, err := rpc.send("thread/start", threadParams)
	if err != nil {
		return completion{}, err
	}
	var threadResult struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := rpc.response(threadID, &threadResult); err != nil {
		return completion{}, withStderr(err, &stderr)
	}
	if threadResult.Thread.ID == "" {
		return completion{}, errors.New("codex app-server returned an empty thread id")
	}

	turnParams := map[string]any{
		"threadId":       threadResult.Thread.ID,
		"input":          []map[string]string{{"type": "text", "text": input}},
		"approvalPolicy": "never",
		"sandboxPolicy":  map[string]any{"type": "readOnly", "networkAccess": false},
	}
	if request.ResponseFormat != nil && request.ResponseFormat.Type == openai.ChatCompletionResponseFormatTypeJSONSchema && request.ResponseFormat.JSONSchema != nil && request.ResponseFormat.JSONSchema.Schema != nil {
		raw, marshalErr := request.ResponseFormat.JSONSchema.Schema.MarshalJSON()
		if marshalErr != nil {
			return completion{}, fmt.Errorf("marshal output schema: %w", marshalErr)
		}
		var schema any
		if unmarshalErr := json.Unmarshal(raw, &schema); unmarshalErr != nil {
			return completion{}, fmt.Errorf("decode output schema: %w", unmarshalErr)
		}
		turnParams["outputSchema"] = schema
	}
	turnID, err := rpc.send("turn/start", turnParams)
	if err != nil {
		return completion{}, err
	}
	var turnResult struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := rpc.response(turnID, &turnResult); err != nil {
		return completion{}, withStderr(err, &stderr)
	}

	var answer string
	for {
		msg, err := rpc.receive()
		if err != nil {
			return completion{}, withStderr(err, &stderr)
		}
		switch msg.Method {
		case "item/tool/call":
			var p struct {
				CallID    string          `json:"callId"`
				Tool      string          `json:"tool"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				return completion{}, fmt.Errorf("decode Codex dynamic tool call: %w", err)
			}
			if p.CallID == "" || p.Tool == "" {
				return completion{}, fmt.Errorf("Codex dynamic tool call omitted id or name")
			}
			arguments := string(p.Arguments)
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			return completion{Content: answer, ToolCalls: []openai.ToolCall{{
				ID: p.CallID, Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: p.Tool, Arguments: arguments},
			}}}, nil
		case "item/completed":
			var p struct {
				Item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(msg.Params, &p) == nil && p.Item.Type == "agentMessage" {
				answer = p.Item.Text
			}
		case "turn/completed":
			var p struct {
				Turn struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Error  any    `json:"error"`
				} `json:"turn"`
			}
			if json.Unmarshal(msg.Params, &p) == nil && (turnResult.Turn.ID == "" || p.Turn.ID == turnResult.Turn.ID) {
				if p.Turn.Status != "completed" {
					return completion{}, fmt.Errorf("codex app-server turn ended with status %q: %v", p.Turn.Status, p.Turn.Error)
				}
				if answer == "" {
					return completion{}, ErrNoResponse
				}
				return completion{Content: answer}, nil
			}
		}
	}
}

func dynamicTools(tools []openai.Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != openai.ToolTypeFunction || tool.Function == nil || tool.Function.Name == "" {
			continue
		}
		out = append(out, map[string]any{
			"type":        "function",
			"name":        tool.Function.Name,
			"description": tool.Function.Description,
			"inputSchema": tool.Function.Parameters,
		})
	}
	return out
}

func splitMessages(messages []openai.ChatCompletionMessage) (string, string) {
	var instructions, conversation []string
	for _, message := range messages {
		content := message.Content
		if content == "" && len(message.MultiContent) > 0 {
			for _, part := range message.MultiContent {
				if part.Type == openai.ChatMessagePartTypeText {
					content += part.Text
				}
			}
		}
		if message.Role == openai.ChatMessageRoleSystem || message.Role == openai.ChatMessageRoleDeveloper {
			instructions = append(instructions, content)
		} else {
			prefix := strings.ToUpper(message.Role)
			if len(message.ToolCalls) > 0 {
				calls := make([]string, 0, len(message.ToolCalls))
				for _, call := range message.ToolCalls {
					calls = append(calls, call.Function.Name+"("+call.Function.Arguments+")")
				}
				content = strings.TrimSpace(content + "\nRequested tools: " + strings.Join(calls, ", "))
			}
			if message.ToolCallID != "" {
				prefix += " result for " + message.ToolCallID
			}
			conversation = append(conversation, prefix+":\n"+content)
		}
	}
	return strings.Join(instructions, "\n\n"), strings.Join(conversation, "\n\n")
}

type boundedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.b.Len() < 32*1024 {
		_, _ = b.b.Write(p[:min(len(p), 32*1024-b.b.Len())])
	}
	return len(p), nil
}
func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.b.String())
}
func withStderr(err error, stderr *boundedBuffer) error {
	if s := stderr.String(); s != "" {
		return fmt.Errorf("%w (app-server stderr: %s)", err, s)
	}
	return err
}

var _ cogito.LLM = (*LLM)(nil)
