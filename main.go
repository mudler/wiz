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
	"github.com/mudler/cogito/pkg/xlog"
	"github.com/mudler/wiz/config"
	"github.com/mudler/wiz/internal"
	"github.com/mudler/wiz/tui"
	"github.com/mudler/wiz/types"
)

// commandTransport creates a new transport for a command
func commandTransport(cmd string, args []string, env ...string) mcp.Transport {
	command := exec.Command(cmd, args...)
	command.Env = os.Environ()
	command.Env = append(command.Env, env...)

	transport := &mcp.CommandTransport{Command: command}
	return transport
}

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
		fmt.Printf("wiz %s\n", internal.PrintableVersion())
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

	cfg := config.Load()

	if cfg.LogLevel == "" {
		cfg.LogLevel = "error"
	}

	xlog.SetLogger(xlog.NewLogger(xlog.LogLevel(cfg.LogLevel), os.Getenv("LOG_FORMAT")))

	// Set MCP servers
	bashMCPServerTransport, bashMCPServerClient := mcp.NewInMemoryTransports()

	go func() {
		if err := runBashMCP(ctx, bashMCPServerTransport); err != nil {
			fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		}
	}()

	transports := []mcp.Transport{bashMCPServerClient}

	for _, c := range cfg.MCPServers {
		envs := []string{}
		for k, v := range c.Env {
			envs = append(envs, fmt.Sprintf("%s=%s", k, v))
		}
		transports = append(transports, commandTransport(c.Command, c.Args, envs...))
	}

	// Determine mode based on flags
	if *heightFlag != "" {
		height := parseHeight(*heightFlag)

		// Check if we should use tmux popup
		useTmux := *tmuxFlag || (inTmux() && !*noTmuxFlag)

		if useTmux && inTmux() {
			// Run in tmux split pane (like fzf-tmux -d)
			if err := runTmuxSplit(*heightFlag); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		} else {
			// TUI mode
			if err := runTUI(ctx, cfg, height, transports...); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		}
	} else {
		// CLI mode (original behavior)
		if err := runner(ctx, cfg, transports...); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}
}

// inTmux returns true if running inside tmux
func inTmux() bool {
	return os.Getenv("TMUX") != "" && os.Getenv("TMUX_PANE") != ""
}

// runTmuxSplit runs wiz in a tmux split pane (like fzf-tmux -d)
func runTmuxSplit(height string) error {
	// Get current working directory
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}

	// Get the wiz executable path
	executable, err := os.Executable()
	if err != nil {
		executable = "wiz"
	}

	// Build the command to run inside the split pane
	// Use --no-tmux to prevent infinite recursion
	wizCmd := fmt.Sprintf("%s --height %s --no-tmux", executable, height)

	// tmux split-window arguments
	// -d: don't switch focus to new pane initially (we'll switch after)
	// -v: vertical split (new pane below)
	// -l: size of the new pane
	// -c: working directory
	//
	// After split, we swap panes so the new pane is below and select it
	tmuxArgs := []string{
		"split-window",
		"-v",         // vertical split (creates pane below)
		"-l", height, // height of the new pane
		"-c", dir, // working directory
		"sh", "-c", wizCmd,
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
func runTUI(ctx context.Context, cfg types.Config, height int, transports ...mcp.Transport) error {

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

const zshInitScript = `# wiz shell integration for zsh
# Add this to your ~/.zshrc:
#   eval "$(wiz --init zsh)"

__wiz_widget() {
  local output
  # Save the current buffer
  local saved_buffer="$BUFFER"
  local saved_cursor="$CURSOR"
  
  # Summon the wizard in TUI mode
  # Uses tmux popup when in tmux, otherwise uses alt screen
  # The TUI writes to /dev/tty directly, stdout captures only the final output
  output=$(wiz --height 50%)
  local ret=$?
  
  # If wiz output a command, insert it
  if [[ -n "$output" ]]; then
    BUFFER="${saved_buffer:0:$saved_cursor}${output}${saved_buffer:$saved_cursor}"
    CURSOR=$((saved_cursor + ${#output}))
  fi
  
  zle reset-prompt
  return $ret
}

zle -N __wiz_widget
bindkey '^ ' __wiz_widget  # Ctrl+Space
`

const bashInitScript = `# wiz shell integration for bash
# Add this to your ~/.bashrc:
#   eval "$(wiz --init bash)"

__wiz_widget() {
  local output
  local saved_line="$READLINE_LINE"
  local saved_point="$READLINE_POINT"
  
  # Summon the wizard in TUI mode
  # Uses tmux popup when in tmux, otherwise uses alt screen
  # The TUI writes to /dev/tty directly, stdout captures only the final output
  output=$(wiz --height 50%)
  
  # If wiz output a command, insert it
  if [[ -n "$output" ]]; then
    READLINE_LINE="${saved_line:0:$saved_point}${output}${saved_line:$saved_point}"
    READLINE_POINT=$((saved_point + ${#output}))
  fi
}

# Bind Ctrl+Space
bind -x '"\C- ": __wiz_widget'
`

const fishInitScript = `# wiz shell integration for fish
# Add this to your ~/.config/fish/config.fish:
#   wiz --init fish | source

function __wiz_widget
  # Summon the wizard in TUI mode
  # Uses tmux popup when in tmux, otherwise uses alt screen
  # The TUI writes to /dev/tty directly, stdout captures only the final output
  set -l output (wiz --height 50%)
  
  if test -n "$output"
    commandline -i "$output"
  end
  
  commandline -f repaint
end

bind \c\  __wiz_widget  # Ctrl+Space
`
