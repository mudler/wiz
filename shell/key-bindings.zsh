#     ___    _      __  
#    /   |  (_)____/ /_ 
#   / /| | / / ___/ __ \
#  / ___ |/ (__  ) / / /
# /_/  |_/_/____/_/ /_/ 
#
# key-bindings.zsh - Zsh keybindings for aish
#
# Usage:
#   source /path/to/key-bindings.zsh
#   OR
#   eval "$(aish --init zsh)"
#
# Configuration:
#   AISH_HEIGHT - Height of the chat window (default: 40%)
#   AISH_CTRL_SPACE_COMMAND - Custom command to run (default: empty)

# Key bindings
# ------------

if 'zmodload' 'zsh/parameter' 2>'/dev/null' && (( ${+options} )); then
  __aish_key_bindings_options="options=(${(j: :)${(kv)options[@]}})"
else
  () {
    __aish_key_bindings_options="setopt"
    'local' '__aish_opt'
    for __aish_opt in "${(@)${(@f)$(set -o)}%% *}"; do
      if [[ -o "$__aish_opt" ]]; then
        __aish_key_bindings_options+=" -o $__aish_opt"
      else
        __aish_key_bindings_options+=" +o $__aish_opt"
      fi
    done
  }
fi

'builtin' 'emulate' 'zsh' && 'builtin' 'setopt' 'no_aliases'

{
if [[ -o interactive ]]; then

# Determine the aish command
__aishcmd() {
  echo "aish"
}

# Default options for aish
__aish_defaults() {
  echo "--height ${AISH_HEIGHT:-50%}"
}

# Ctrl+Space - Open AI chat assistant
aish-chat-widget() {
  local output
  setopt localoptions pipefail no_aliases 2> /dev/null
  
  # Save current buffer state
  local saved_buffer="$BUFFER"
  local saved_cursor="$CURSOR"
  
  # Run aish with TUI
  # The TUI writes to /dev/tty directly, stdout captures only the final output
  output=$($(__aishcmd) $(__aish_defaults))
  local ret=$?
  
  # If aish output a command, insert it at cursor position
  if [[ -n "$output" ]]; then
    BUFFER="${saved_buffer:0:$saved_cursor}${output}${saved_buffer:$saved_cursor}"
    CURSOR=$((saved_cursor + ${#output}))
  fi
  
  zle reset-prompt
  return $ret
}

# Register the widget and bind to Ctrl+Space
zle -N aish-chat-widget
bindkey -M emacs '^ ' aish-chat-widget
bindkey -M vicmd '^ ' aish-chat-widget
bindkey -M viins '^ ' aish-chat-widget

fi
} always {
  eval $__aish_key_bindings_options
  'unset' '__aish_key_bindings_options'
}
