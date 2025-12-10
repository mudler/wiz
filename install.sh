#!/usr/bin/env bash
#  ╭─────╮
#  │ ◠ ◠ │    _      ___
#  │  ▽  │   | | /| / (_)___
#  ╰──┬──╯   | |/ |/ / |_ /
#    /|\     |__/|__/_//__/
#   / | \
#          your terminal wizard
#
# wiz installer
#
# This script installs wiz and sets up shell integration.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/mudler/wiz/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/mudler/wiz/main/install.sh | zsh
#   OR
#   ./install.sh
#
# Options:
#   --no-key-bindings    Skip setting up shell key bindings

set -e

# Configuration
INSTALL_DIR="${WIZ_INSTALL_DIR:-$HOME/.local/bin}"
SHELL_DIR="${WIZ_SHELL_DIR:-$HOME/.config/wiz/shell}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
BOLD='\033[1m'
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
INSTALL_FROM_SOURCE=false
WIZ_VERSION=latest
while [[ $# -gt 0 ]]; do
    case $1 in
        --no-key-bindings)
            SETUP_KEY_BINDINGS=false
            shift
            ;;
        --from-source)
            INSTALL_FROM_SOURCE=true
            shift
            ;;
        --version)
            WIZ_VERSION=$2
            shift
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

install_from_release() {
    info "Installing wiz from release..."
    
    # Get version tag if latest
    if [ "$WIZ_VERSION" = "latest" ]; then
        info "Fetching latest release version..."
        WIZ_VERSION=$(curl -fsSL https://api.github.com/repos/mudler/wiz/releases/latest | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' || echo "")
        if [ -z "$WIZ_VERSION" ]; then
            error "Failed to fetch latest release version"
        fi
        info "Latest version: $WIZ_VERSION"
    fi
    
    # Construct filename: wiz-${VERSION}-${OS}-${ARCH}
    FILENAME="wiz-${WIZ_VERSION}-${OS}-${ARCH}"
    DOWNLOAD_URL="https://github.com/mudler/wiz/releases/download/${WIZ_VERSION}/${FILENAME}"
    
    info "Downloading from: $DOWNLOAD_URL"
    mkdir -p "$INSTALL_DIR"
    curl -fsSL "$DOWNLOAD_URL" -o "$INSTALL_DIR/wiz"
    chmod +x "$INSTALL_DIR/wiz"
    success "Installed wiz to $INSTALL_DIR/wiz"
}

# Build from source
build_from_source() {
    info "Building wiz from source..."
    
    # Check for Go
    if ! command -v go &> /dev/null; then
        error "Go is required to build from source. Please install Go first."
    fi

    if ! command -v git &> /dev/null; then
        error "Git is required to build from source. Please install Git first."
    fi
    
    # Get the script directory
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    
    # Build
    cd "$SCRIPT_DIR"
    if [ ! -e main.go ]; then
        git clone https://github.com/mudler/wiz.git
        cd wiz
    fi
    go build -o wiz .
    
    # Install
    mkdir -p "$INSTALL_DIR"
    mv wiz "$INSTALL_DIR/"
    
    success "Built and installed wiz to $INSTALL_DIR/wiz"
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
        echo "  eval \"\$(wiz --init <your-shell>)\""
        echo ""
        return
    fi
    
    # Check if already configured
    if grep -q 'wiz --init' "$RC_FILE" 2>/dev/null; then
        info "Shell integration already configured in $RC_FILE"
        return
    fi
    
    # Add shell integration
    info "Adding shell integration to $RC_FILE"
    
    echo "" >> "$RC_FILE"
    echo "# wiz shell integration" >> "$RC_FILE"
    echo "eval \"\$(wiz --init $CURRENT_SHELL)\"" >> "$RC_FILE"
    
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
                echo "# wiz binary" >> "$RC_FILE"
                echo "export PATH=\"\$PATH:$INSTALL_DIR\"" >> "$RC_FILE"
            fi
        fi
    fi
}

# Main installation
main() {
    echo ""
    echo -e "${PURPLE}        ╭─────╮${NC}"
    echo -e "${PURPLE}        │${NC} ◠ ◠ ${PURPLE}│${NC}"
    echo -e "${PURPLE}        │${NC}  ▽  ${PURPLE}│${NC}"
    echo -e "${PURPLE}        ╰──┬──╯${NC}"
    echo -e "${PURPLE}          /|\\\\${NC}"
    echo -e "${PURPLE}         / | \\\\${NC}"
    echo ""
    echo -e "          ${BLUE}${BOLD}wiz${NC}"
    echo -e "   ${YELLOW}your terminal wizard${NC}"
    echo ""
    
    detect_platform
    detect_shell
    
    # Build and install
    if [ "$INSTALL_FROM_SOURCE" = "true" ]; then
        build_from_source
    else
        install_from_release
    fi
    
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
    echo "  - Press Ctrl+Space to summon the wizard"
    echo "  - Run 'wiz' for CLI mode"
    echo "  - Run 'wiz --height 40%' for TUI mode"
    echo ""
    echo "Configuration:"
    echo "  Create a config file at ~/.config/wiz/config.yaml, ~/.wiz.yaml or /etc/wiz/config.yaml"
    echo ""
    echo "Example config:"
    echo ""
    echo "  model: gpt-4o-mini"
    echo "  api_key: your-api-key"
    echo "  base_url: https://api.openai.com/v1"
    echo ""
}

main "$@"
