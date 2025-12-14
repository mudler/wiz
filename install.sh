#!/usr/bin/env bash
#     ___    _      __  
#    /   |  (_)____/ /_ 
#   / /| | / / ___/ __ \
#  / ___ |/ (__  ) / / /
# /_/  |_/_/____/_/ /_/ 
#
# aish installer
#
# This script installs aish and sets up shell integration.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/mudler/aish/main/install.sh | bash
#   OR
#   ./install.sh
#
# Options:
#   --no-key-bindings    Skip setting up shell key bindings

set -e

# Configuration
INSTALL_DIR="${AISH_INSTALL_DIR:-$HOME/.local/bin}"
SHELL_DIR="${AISH_SHELL_DIR:-$HOME/.config/aish/shell}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Print functions
info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
}

# Parse arguments
SETUP_KEY_BINDINGS=true
while [[ $# -gt 0 ]]; do
    case $1 in
        --no-key-bindings)
            SETUP_KEY_BINDINGS=false
            shift
            ;;
        *)
            warn "Unknown option: $1"
            shift
            ;;
    esac
done

# Detect OS and architecture
detect_platform() {
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)
    
    case $ARCH in
        x86_64)
            ARCH="amd64"
            ;;
        aarch64|arm64)
            ARCH="arm64"
            ;;
        armv7l)
            ARCH="arm"
            ;;
        *)
            error "Unsupported architecture: $ARCH"
            ;;
    esac
    
    case $OS in
        linux|darwin)
            ;;
        *)
            error "Unsupported OS: $OS"
            ;;
    esac
    
    PLATFORM="${OS}_${ARCH}"
    info "Detected platform: $PLATFORM"
}

# Detect current shell
detect_shell() {
    if [[ -n "$ZSH_VERSION" ]]; then
        CURRENT_SHELL="zsh"
        RC_FILE="$HOME/.zshrc"
    elif [[ -n "$BASH_VERSION" ]]; then
        CURRENT_SHELL="bash"
        RC_FILE="$HOME/.bashrc"
    elif [[ -n "$FISH_VERSION" ]]; then
        CURRENT_SHELL="fish"
        RC_FILE="$HOME/.config/fish/config.fish"
    else
        # Try to detect from $SHELL
        case "$SHELL" in
            */zsh)
                CURRENT_SHELL="zsh"
                RC_FILE="$HOME/.zshrc"
                ;;
            */bash)
                CURRENT_SHELL="bash"
                RC_FILE="$HOME/.bashrc"
                ;;
            */fish)
                CURRENT_SHELL="fish"
                RC_FILE="$HOME/.config/fish/config.fish"
                ;;
            *)
                CURRENT_SHELL="unknown"
                RC_FILE=""
                ;;
        esac
    fi
    
    info "Detected shell: $CURRENT_SHELL"
}

# Build from source
build_from_source() {
    info "Building aish from source..."
    
    # Check for Go
    if ! command -v go &> /dev/null; then
        error "Go is required to build from source. Please install Go first."
    fi
    
    # Get the script directory
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    
    # Build
    cd "$SCRIPT_DIR"
    go build -o aish .
    
    # Install
    mkdir -p "$INSTALL_DIR"
    mv aish "$INSTALL_DIR/"
    
    success "Built and installed aish to $INSTALL_DIR/aish"
}

# Install shell integration
install_shell_integration() {
    if [[ "$SETUP_KEY_BINDINGS" != "true" ]]; then
        info "Skipping shell key bindings setup"
        return
    fi
    
    if [[ -z "$RC_FILE" ]]; then
        warn "Could not detect shell configuration file. Please manually add:"
        echo ""
        echo "  eval \"\$(aish --init <your-shell>)\""
        echo ""
        return
    fi
    
    # Check if already configured
    if grep -q 'aish --init' "$RC_FILE" 2>/dev/null; then
        info "Shell integration already configured in $RC_FILE"
        return
    fi
    
    # Add shell integration
    info "Adding shell integration to $RC_FILE"
    
    echo "" >> "$RC_FILE"
    echo "# aish shell integration" >> "$RC_FILE"
    echo "eval \"\$(aish --init $CURRENT_SHELL)\"" >> "$RC_FILE"
    
    success "Added shell integration to $RC_FILE"
    echo ""
    info "To activate immediately, run:"
    echo ""
    echo "  source $RC_FILE"
    echo ""
}

# Install shell scripts
install_shell_scripts() {
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    
    if [[ -d "$SCRIPT_DIR/shell" ]]; then
        mkdir -p "$SHELL_DIR"
        cp -r "$SCRIPT_DIR/shell/"* "$SHELL_DIR/"
        success "Installed shell scripts to $SHELL_DIR"
    fi
}

# Update PATH
update_path() {
    if [[ ":$PATH:" != *":$INSTALL_DIR:"* ]]; then
        info "Adding $INSTALL_DIR to PATH in $RC_FILE"
        
        if [[ -n "$RC_FILE" ]]; then
            if ! grep -q "export PATH=.*$INSTALL_DIR" "$RC_FILE" 2>/dev/null; then
                echo "" >> "$RC_FILE"
                echo "# aish binary" >> "$RC_FILE"
                echo "export PATH=\"\$PATH:$INSTALL_DIR\"" >> "$RC_FILE"
            fi
        fi
    fi
}

# Main installation
main() {
    echo ""
    echo "  ___    _      __  "
    echo " /   |  (_)____/ /_ "
    echo "/ /| | / / ___/ __ \\"
    echo "/ ___ |/ (__  ) / / /"
    echo "/_/  |_/_/____/_/ /_/ "
    echo ""
    echo "AI Shell Assistant Installer"
    echo ""
    
    detect_platform
    detect_shell
    
    # Build and install
    build_from_source
    
    # Update PATH
    update_path
    
    # Install shell scripts
    install_shell_scripts
    
    # Setup shell integration
    install_shell_integration
    
    echo ""
    success "Installation complete!"
    echo ""
    echo "Usage:"
    echo "  - Press Ctrl+Space to open the AI assistant"
    echo "  - Run 'aish' for CLI mode"
    echo "  - Run 'aish --height 40%' for TUI mode"
    echo ""
    echo "Configuration:"
    echo "  Set these environment variables:"
    echo "    export MODEL=<model-name>"
    echo "    export API_KEY=<your-api-key>"
    echo "    export BASE_URL=<api-base-url>"
    echo ""
}

main "$@"

