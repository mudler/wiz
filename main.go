package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mudler/aish/chat"
	"github.com/mudler/aish/tui"
)

var (
	version = "dev"
)

func main() {
	// Parse command line arguments
	heightFlag := flag.String("height", "", "Height of the TUI (e.g., '40%' or '20')")
	initFlag := flag.String("init", "", "Output shell integration script (zsh, bash, or fish)")
	versionFlag := flag.Bool("version", false, "Print version and exit")
	tmuxFlag := flag.Bool("tmux", false, "Run in tmux popup (auto-detected if in tmux)")
	noTmuxFlag := flag.Bool("no-tmux", false, "Disable tmux popup even when in tmux")
	flag.Parse()

	// Handle version flag
	if *versionFlag {
		fmt.Printf("aish %s\n", version)
		os.Exit(0)
	}

	// Handle init command
	if *initFlag != "" {
		script := getInitScript(*initFlag)
		if script == "" {
			fmt.Fprintf(os.Stderr, "Unknown shell: %s. Supported: zsh, bash, fish\n", *initFlag)
			os.Exit(1)
		}
		fmt.Print(script)
		os.Exit(0)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigs
		cancel()
	}()

	// Set MCP servers
	bashMCPServerTransport, bashMCPServerClient := mcp.NewInMemoryTransports()

	go func() {
		if err := runBashMCP(ctx, bashMCPServerTransport); err != nil {
			fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		}
	}()

	// Determine mode based on flags
	if *heightFlag != "" {
		height := parseHeight(*heightFlag)

		// Check if we should use tmux popup
		useTmux := *tmuxFlag || (inTmux() && !*noTmuxFlag)

		if useTmux && inTmux() {
			// Run in tmux popup
			if err := runTmuxPopup(*heightFlag); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		} else {
			// TUI mode
			if err := runTUI(ctx, height, bashMCPServerClient); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		}
	} else {
		// CLI mode (original behavior)
		if err := runner(ctx, bashMCPServerClient); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}
}

// inTmux returns true if running inside tmux
func inTmux() bool {
	return os.Getenv("TMUX") != "" && os.Getenv("TMUX_PANE") != ""
}

// runTmuxPopup runs aish in a tmux popup window
func runTmuxPopup(height string) error {
	// Get current working directory
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}

	// Get the aish executable path
	executable, err := os.Executable()
	if err != nil {
		executable = "aish"
	}

	// Build the command to run inside the popup
	// Use --no-tmux to prevent infinite recursion
	aishCmd := fmt.Sprintf("%s --height %s --no-tmux", executable, height)

	// tmux display-popup arguments
	// -E: close popup when command exits
	// -d: working directory
	// -w: width (80% of pane)
	// -h: height
	// -xC -yS: center horizontally, attach to bottom
	tmuxArgs := []string{
		"display-popup",
		"-E",
		"-d", dir,
		"-w", "80%",
		"-h", height,
		"-xC",
		"-yS",
		"sh", "-c", aishCmd,
	}

	cmd := exec.Command("tmux", tmuxArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// parseHeight parses a height string like "40%" or "20"
func parseHeight(s string) int {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "%") {
		// Percentage of terminal height
		pct, err := strconv.Atoi(strings.TrimSuffix(s, "%"))
		if err != nil || pct <= 0 || pct > 100 {
			return 40 // default
		}
		// We'll calculate actual height in the TUI based on terminal size
		return -pct // negative means percentage
	}

	height, err := strconv.Atoi(s)
	if err != nil || height <= 0 {
		return 20 // default
	}
	return height
}

// runTUI runs the Bubble Tea TUI
func runTUI(ctx context.Context, height int, transports ...mcp.Transport) error {
	cfg := chat.Config{
		Model:   os.Getenv("MODEL"),
		APIKey:  os.Getenv("API_KEY"),
		BaseURL: os.Getenv("BASE_URL"),
	}

	model := tui.NewModel(ctx, cfg, height, transports...)

	// Open /dev/tty directly for TUI - this is crucial when stdout is being captured
	// (e.g., when run from a shell widget like `output=$(aish --height 40%)`)
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

	// Configure program options to use /dev/tty directly
	opts := []tea.ProgramOption{
		tea.WithAltScreen(),
		tea.WithInput(ttyIn),
		tea.WithOutput(ttyOut),
	}

	p := tea.NewProgram(model, opts...)

	finalModel, err := p.Run()
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

// getInitScript returns the shell integration script for the given shell
func getInitScript(shell string) string {
	switch shell {
	case "zsh":
		return zshInitScript
	case "bash":
		return bashInitScript
	case "fish":
		return fishInitScript
	default:
		return ""
	}
}

const zshInitScript = `# aish shell integration for zsh
# Add this to your ~/.zshrc:
#   eval "$(aish --init zsh)"

__aish_widget() {
  local output
  # Save the current buffer
  local saved_buffer="$BUFFER"
  local saved_cursor="$CURSOR"
  
  # Run aish in TUI mode
  # Uses tmux popup when in tmux, otherwise uses alt screen
  # The TUI writes to /dev/tty directly, stdout captures only the final output
  output=$(aish --height 50%)
  local ret=$?
  
  # If aish output a command, insert it
  if [[ -n "$output" ]]; then
    BUFFER="${saved_buffer:0:$saved_cursor}${output}${saved_buffer:$saved_cursor}"
    CURSOR=$((saved_cursor + ${#output}))
  fi
  
  zle reset-prompt
  return $ret
}

zle -N __aish_widget
bindkey '^ ' __aish_widget  # Ctrl+Space
`

const bashInitScript = `# aish shell integration for bash
# Add this to your ~/.bashrc:
#   eval "$(aish --init bash)"

__aish_widget() {
  local output
  local saved_line="$READLINE_LINE"
  local saved_point="$READLINE_POINT"
  
  # Run aish in TUI mode
  # Uses tmux popup when in tmux, otherwise uses alt screen
  # The TUI writes to /dev/tty directly, stdout captures only the final output
  output=$(aish --height 50%)
  
  # If aish output a command, insert it
  if [[ -n "$output" ]]; then
    READLINE_LINE="${saved_line:0:$saved_point}${output}${saved_line:$saved_point}"
    READLINE_POINT=$((saved_point + ${#output}))
  fi
}

# Bind Ctrl+Space
bind -x '"\C- ": __aish_widget'
`

const fishInitScript = `# aish shell integration for fish
# Add this to your ~/.config/fish/config.fish:
#   aish --init fish | source

function __aish_widget
  # Uses tmux popup when in tmux, otherwise uses alt screen
  # The TUI writes to /dev/tty directly, stdout captures only the final output
  set -l output (aish --height 50%)
  
  if test -n "$output"
    commandline -i "$output"
  end
  
  commandline -f repaint
end

bind \c\  __aish_widget  # Ctrl+Space
`
