#     ___    _      __  
#    /   |  (_)____/ /_ 
#   / /| | / / ___/ __ \
#  / ___ |/ (__  ) / / /
# /_/  |_/_/____/_/ /_/ 
#
# key-bindings.fish - Fish keybindings for aish
#
# Usage:
#   source /path/to/key-bindings.fish
#   OR
#   aish --init fish | source
#
# Configuration:
#   AISH_HEIGHT - Height of the chat window (default: 40%)

# Check if running interactively
if not status is-interactive
    exit
end

# Key bindings
# ------------

# Default height if not set
if not set -q AISH_HEIGHT
    set -g AISH_HEIGHT "40%"
end

# Determine the aish command
function __aish_cmd
    echo "aish"
end

# Ctrl+Space - Open AI chat assistant
function __aish_chat_widget
    # Save the current command line
    set -l saved_cmd (commandline)
    set -l saved_cursor (commandline -C)
    
    # Run aish with TUI
    # The TUI writes to /dev/tty directly, stdout captures only the final output
    set -l output (eval (__aish_cmd) --height $AISH_HEIGHT)
    
    # If aish output a command, insert it at cursor position
    if test -n "$output"
        # Build new command line with output inserted at cursor
        set -l before (string sub -l $saved_cursor -- "$saved_cmd")
        set -l after (string sub -s (math $saved_cursor + 1) -- "$saved_cmd")
        commandline -r "$before$output$after"
        commandline -C (math $saved_cursor + (string length -- "$output"))
    end
    
    commandline -f repaint
end

# Bind Ctrl+Space
# Note: \c  represents Ctrl+Space in fish
bind \c\  __aish_chat_widget
bind -M insert \c\  __aish_chat_widget
