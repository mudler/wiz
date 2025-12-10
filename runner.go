package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mudler/cogito"
)

// commandTransport creates a new transport for a command
func commandTransport(cmd string, args []string, env ...string) mcp.Transport {
	command := exec.Command(cmd, args...)
	command.Env = os.Environ()
	command.Env = append(command.Env, env...)

	transport := &mcp.CommandTransport{Command: command}
	return transport
}

func runner(ctx context.Context, transports ...mcp.Transport) error {

	model := os.Getenv("MODEL")
	apiKey := os.Getenv("API_KEY")
	baseURL := os.Getenv("BASE_URL")

	defaultLLM := cogito.NewOpenAILLM(model, apiKey, baseURL)
	client := mcp.NewClient(&mcp.Implementation{Name: "aish", Version: "v1.0.0"}, nil)
	clients := []*mcp.ClientSession{}
	for _, transport := range transports {
		client, err := client.Connect(ctx, transport, nil)
		if err != nil {
			return err
		}
		clients = append(clients, client)
	}

	f := cogito.NewEmptyFragment()
	for {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("> ")
		text, _ := reader.ReadString('\n')
		fmt.Println(strings.TrimSpace(text))

		f = f.AddMessage("user", strings.TrimSpace(text))
		var err error
		f, err = cogito.ExecuteTools(
			defaultLLM, f,
			cogito.WithContext(ctx),
			cogito.WithIterations(10),
			cogito.WithMaxAttempts(3),
			cogito.WithMaxRetries(3),
			cogito.WithForceReasoning(),
			cogito.WithStatusCallback(func(s string) {
				fmt.Println("Status: " + s)
			}),
			cogito.WithReasoningCallback(func(s string) {
				fmt.Println("Reasoning: " + s)
			}),
			cogito.WithMCPs(clients...),
			cogito.WithToolCallBack(func(tool *cogito.ToolChoice, state *cogito.SessionState) cogito.ToolCallDecision {

				args, err := json.Marshal(tool.Arguments)
				if err != nil {
					return cogito.ToolCallDecision{Approved: false}
				}
				fmt.Println("The agent wants to run the tool " + tool.Name + " with the following arguments: " + string(args) + "\nReasoning: " + tool.Reasoning)
				fmt.Println("Do you want to run the tool? (y/n/adjust)")
				reader := bufio.NewReader(os.Stdin)
				text, _ := reader.ReadString('\n')
				text = strings.TrimSpace(text)
				switch text {
				case "y":
					return cogito.ToolCallDecision{Approved: true}
				case "n":
					return cogito.ToolCallDecision{Approved: false}
				default:
					return cogito.ToolCallDecision{
						Approved:   true,
						Adjustment: text,
					}
				}
			}),
		)
		if err != nil && !errors.Is(err, cogito.ErrNoToolSelected) {
			return err
		}

		f, err = defaultLLM.Ask(context.Background(), f)
		if err != nil {
			return err
		}

		fmt.Println(f.LastMessage().Content)

	}
}
