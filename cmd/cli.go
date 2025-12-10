package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mudler/wiz/chat"
	"github.com/mudler/wiz/types"
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorCyan   = "\033[36m"
	colorYellow = "\033[33m"
	colorGreen  = "\033[32m"
	colorGray   = "\033[90m"
	colorBold   = "\033[1m"
	colorRed    = "\033[31m"
	colorPurple = "\033[35m"
)

// Spinner frames for animated display
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinner manages an animated spinner for CLI output
type spinner struct {
	mu       sync.Mutex
	active   bool
	message  string
	stopChan chan struct{}
	doneChan chan struct{}
}

func newSpinner() *spinner {
	return &spinner{
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
	}
}

func (s *spinner) start(message string) {
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return
	}
	s.active = true
	s.message = message
	s.stopChan = make(chan struct{})
	s.doneChan = make(chan struct{})
	s.mu.Unlock()

	go func() {
		frame := 0
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		defer close(s.doneChan)

		for {
			select {
			case <-s.stopChan:
				// Clear the spinner line
				fmt.Print("\r\033[K")
				return
			case <-ticker.C:
				s.mu.Lock()
				msg := s.message
				s.mu.Unlock()
				fmt.Printf("\r%s%s %s%s", colorCyan, spinnerFrames[frame], msg, colorReset)
				frame = (frame + 1) % len(spinnerFrames)
			}
		}
	}()
}

func (s *spinner) update(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message = message
}

func (s *spinner) stop() {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return
	}
	s.active = false
	s.mu.Unlock()

	close(s.stopChan)
	<-s.doneChan
}

// readStringCancellable reads a line from the reader, but can be cancelled via context
func readStringCancellable(ctx context.Context, reader *bufio.Reader) (string, error) {
	type result struct {
		text string
		err  error
	}
	resultChan := make(chan result, 1)

	go func() {
		text, err := reader.ReadString('\n')
		resultChan <- result{text: text, err: err}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-resultChan:
		return res.text, res.err
	}
}

func RunCLI(ctx context.Context, cfg types.Config, transports ...mcp.Transport) error {
	reader := bufio.NewReader(os.Stdin)
	spin := newSpinner()

	callbacks := chat.Callbacks{
		OnStatus: func(status string) {
			spin.update(status)
		},
		OnReasoning: func(reasoning string) {
			spin.stop()
			fmt.Printf("%s💭 %s%s\n", colorGray, reasoning, colorReset)
			spin.start("Conjuring...")
		},
		OnToolCall: func(req chat.ToolCallRequest) chat.ToolCallResponse {
			spin.stop()
			fmt.Println()
			fmt.Println(strings.Repeat("─", 50))
			fmt.Printf("%s%s🔧 Tool Request: %s%s\n", colorBold, colorYellow, req.Name, colorReset)
			fmt.Printf("%sArguments:%s %s\n", colorGray, colorReset, req.Arguments)
			if req.Reasoning != "" {
				fmt.Printf("%s💭 %s%s\n", colorGray, req.Reasoning, colorReset)
			}
			fmt.Println(strings.Repeat("─", 50))
			fmt.Printf("\n%s[y]es  [a]lways  [n]o  or type adjustment:%s ", colorCyan, colorReset)

			text, _ := readStringCancellable(ctx, reader)
			text = strings.TrimSpace(text)
			fmt.Println()

			var response chat.ToolCallResponse
			switch strings.ToLower(text) {
			case "y", "yes":
				response = chat.ToolCallResponse{Approved: true}
				spin.start("Executing tool...")
			case "a", "always":
				response = chat.ToolCallResponse{Approved: true, AlwaysAllow: true}
				fmt.Printf("%s✓ Tool '%s' added to allow list for this session%s\n", colorGreen, req.Name, colorReset)
				spin.start("Executing tool...")
			case "n", "no":
				response = chat.ToolCallResponse{Approved: false}
				fmt.Printf("%s✗ Tool execution denied%s\n", colorRed, colorReset)
			default:
				response = chat.ToolCallResponse{Approved: true, Adjustment: text}
				spin.start("Executing adjusted tool...")
			}
			return response
		},
		OnResponse: func(response string) {
			spin.stop()
			fmt.Println()
			fmt.Println(strings.Repeat("─", 50))
			fmt.Printf("%s%s🧙 Wiz:%s\n", colorBold, colorPurple, colorReset)
			fmt.Println(response)
			fmt.Println(strings.Repeat("─", 50))
		},
		OnError: func(err error) {
			spin.stop()
			fmt.Fprintf(os.Stderr, "%s✗ Error: %v%s\n", colorRed, err, colorReset)
		},
	}

	session, err := chat.NewSession(ctx, cfg, callbacks, transports...)
	if err != nil {
		return err
	}
	defer session.Close()

	fmt.Printf("%s%s✨ [◠ ◠] wiz%s\n", colorBold, colorPurple, colorReset)
	fmt.Println(strings.Repeat("─", 50))
	fmt.Printf("%sYour terminal wizard awaits. Type your command and press Enter.%s\n", colorGray, colorReset)
	fmt.Printf("%sCtrl+C to exit.%s\n\n", colorGray, colorReset)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			fmt.Printf("%s>%s ", colorCyan, colorReset)

			text, err := readStringCancellable(ctx, reader)
			if err != nil {
				return err
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}

			switch text {
			case "clear":
				session.ClearHistory()
				continue
			case "exit":
				return nil
			case "help":
				fmt.Println("Available commands:")
				fmt.Println("  exit - Exit the wizard")
				fmt.Println("  help - Show this help message")
				fmt.Println("  clear - Clear the conversation")
				continue
			}

			fmt.Println()
			spin.start("Casting spell...")
			_, err = session.SendMessage(text)
			spin.stop()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s✗ Error: %v%s\n", colorRed, err, colorReset)
			}
			fmt.Println()
		}
	}
}
