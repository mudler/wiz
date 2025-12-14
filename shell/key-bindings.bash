#     ___    _      __  
#    /   |  (_)____/ /_ 
#   / /| | / / ___/ __ \
#  / ___ |/ (__  ) / / /
# /_/  |_/_/____/_/ /_/ 
#
# key-bindings.bash - Bash keybindings for aish
#
# Usage:
#   source /path/to/key-bindings.bash
#   OR
#   eval "$(aish --init bash)"
#
# Configuration:
#   AISH_HEIGHT - Height of the chat window (default: 40%)

if [[ $- =~ i ]]; then

# Key bindings
# ------------

# Determine the aish command
__aishcmd() {
  echo "aish"
}

# Default options for aish
__aish_defaults() {
  echo "--height ${AISH_HEIGHT:-40%}"
}

# Ctrl+Space - Open AI chat assistant
__aish_chat__() {
  local output
  local saved_line="$READLINE_LINE"
  local saved_point="$READLINE_POINT"
  
  # Run aish with TUI
  # The TUI writes to /dev/tty directly, stdout captures only the final output
  output=$($(__aishcmd) $(__aish_defaults))
  
  # If aish output a command, insert it at cursor position
  if [[ -n "$output" ]]; then
    READLINE_LINE="${saved_line:0:$saved_point}${output}${saved_line:$saved_point}"
    READLINE_POINT=$((saved_point + ${#output}))
  fi
}

# Required to refresh the prompt after aish
bind -m emacs-standard '"\er": redraw-current-line'

# Mode switching for vi mode
bind -m vi-command '"\C-z": emacs-editing-mode'
bind -m vi-insert '"\C-z": emacs-editing-mode'
bind -m emacs-standard '"\C-z": vi-editing-mode'

if ((BASH_VERSINFO[0] < 4)); then
  # Older bash versions: use command substitution
  bind -m emacs-standard '"\C- ": " \C-b\C-k \C-u`__aish_chat__`\e\C-e\er\C-a\C-y\C-h\C-e\e \C-y\ey\C-x\C-x\C-f"'
  bind -m vi-command '"\C- ": "\C-z\C- \C-z"'
  bind -m vi-insert '"\C- ": "\C-z\C- \C-z"'
else
  # Bash 4+: use -x for direct execution
  bind -m emacs-standard -x '"\C- ": __aish_chat__'
  bind -m vi-command -x '"\C- ": __aish_chat__'
  bind -m vi-insert -x '"\C- ": __aish_chat__'
fi

fi
