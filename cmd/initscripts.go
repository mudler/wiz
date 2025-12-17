package cmd

// getInitScript returns the shell integration script for the given shell
func GetInitScript(shell string) string {
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
