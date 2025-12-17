package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mudler/wiz/tui"
	"github.com/mudler/wiz/types"
)

// runTUI runs the Bubble Tea TUI
func RunTUI(ctx context.Context, cfg types.Config, height int, transports ...mcp.Transport) error {

	model := tui.NewModel(ctx, cfg, height, transports...)

	// Open /dev/tty directly for TUI - this is crucial when stdout is being captured
	// (e.g., when run from a shell widget like `output=$(wiz --height 40%)`)
	ttyIn, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open /dev/tty for reading: %w", err)
	}
	defer ttyIn.Close()

	ttyOut, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open /dev/tty for writing: %w", err)
	}
	defer ttyOut.Close()

	// Calculate actual height for inline mode
	// Like fzf, we render inline at the bottom of the terminal
	termHeight := getTerminalHeight()
	actualHeight := height
	if height < 0 {
		// Negative means percentage
		actualHeight = (termHeight * (-height)) / 100
	}
	if actualHeight > termHeight {
		actualHeight = termHeight
	}
	if actualHeight < 5 {
		actualHeight = 5
	}

	// Make space for the TUI by printing newlines (like fzf does)
	// This pushes the existing content up
	for i := 0; i < actualHeight; i++ {
		fmt.Fprint(ttyOut, "\n")
	}
	// Move cursor up to the start of our space
	fmt.Fprintf(ttyOut, "\x1b[%dA", actualHeight)
	// Move to beginning of line
	fmt.Fprint(ttyOut, "\x1b[G")

	// Configure program options to use /dev/tty directly
	// Don't use alt screen - render inline like fzf
	opts := []tea.ProgramOption{
		tea.WithInput(ttyIn),
		tea.WithOutput(ttyOut),
	}

	p := tea.NewProgram(model, opts...)

	finalModel, err := p.Run()

	// Clear the space we used (move to start and clear to end of screen)
	fmt.Fprint(ttyOut, "\x1b[G") // Move to beginning of line
	fmt.Fprint(ttyOut, "\x1b[J") // Clear from cursor to end of screen

	if err != nil {
		return err
	}

	// Output any command to shell if needed (this goes to real stdout for shell capture)
	if m, ok := finalModel.(tui.Model); ok {
		if output := m.Output(); output != "" {
			fmt.Print(output)
		}
	}

	return nil
}

// getTerminalHeight returns the terminal height
func getTerminalHeight() int {
	// Try to get terminal size
	cmd := exec.Command("tput", "lines")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err == nil {
		if h, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && h > 0 {
			return h
		}
	}

	// Fallback: try stty
	cmd = exec.Command("stty", "size")
	cmd.Stdin, _ = os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	out, err = cmd.Output()
	if err == nil {
		parts := strings.Fields(string(out))
		if len(parts) >= 1 {
			if h, err := strconv.Atoi(parts[0]); err == nil && h > 0 {
				return h
			}
		}
	}

	// Default
	return 24
}
