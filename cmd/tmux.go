package cmd

import (
	"fmt"
	"os"
	"os/exec"
)

// inTmux returns true if running inside tmux
func IsInTmux() bool {
	return os.Getenv("TMUX") != "" && os.Getenv("TMUX_PANE") != ""
}

// runTmuxSplit runs wiz in a tmux split pane (like fzf-tmux -d)
func RunTmuxSplit(height string) error {
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
