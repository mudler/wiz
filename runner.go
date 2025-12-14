package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mudler/aish/chat"
	"github.com/mudler/aish/config"
)

func runner(ctx context.Context, transports ...mcp.Transport) error {
	reader := bufio.NewReader(os.Stdin)

	callbacks := chat.Callbacks{
		OnStatus: func(status string) {
			fmt.Println("Status: " + status)
		},
		OnReasoning: func(reasoning string) {
			fmt.Println("Reasoning: " + reasoning)
		},
		OnToolCall: func(req chat.ToolCallRequest) chat.ToolCallResponse {
			fmt.Printf("The agent wants to run the tool %s with the following arguments: %s\nReasoning: %s\n",
				req.Name, req.Arguments, req.Reasoning)
			fmt.Println("Do you want to run the tool? (y/n/adjust)")

			text, _ := reader.ReadString('\n')
			text = strings.TrimSpace(text)

			switch text {
			case "y":
				return chat.ToolCallResponse{Approved: true}
			case "n":
				return chat.ToolCallResponse{Approved: false}
			default:
				return chat.ToolCallResponse{
					Approved:   true,
					Adjustment: text,
				}
			}
		},
		OnResponse: func(response string) {
			fmt.Println(response)
		},
		OnError: func(err error) {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		},
	}

	cfg := config.Load()

	session, err := chat.NewSession(ctx, cfg, callbacks, transports...)
	if err != nil {
		return err
	}
	defer session.Close()

	for {
		fmt.Print("> ")
		text, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}

		_, err = session.SendMessage(text)
		if err != nil {
			return err
		}
	}
}
