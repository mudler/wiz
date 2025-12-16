<p align="center">
  <pre>
    ╭─────╮
    │ ◠ ◠ │
    │  ▽  │
    ╰──┬──╯
      /|\
     / | \

      wiz
  </pre>
</p>

<p align="center">
  <strong>Summon AI intelligence in your terminal with Ctrl+Space</strong>

  Wiz aims to be the `fzf` for llms in the terminal.
</p>

<p align="center">
  <a href="#features">Features</a> •
  <a href="#installation">Installation</a> •
  <a href="#usage">Usage</a> •
  <a href="#configuration">Configuration</a> •
  <a href="#tool-approval">Tool Approval</a>
</p>

---

## Features

🧙 **Magic at your fingertips** — Press `Ctrl+Space` anywhere to summon the wizard

⚡ **Dual modes** — Beautiful TUI or simple CLI, your choice

🔧 **Tool execution** — AI runs shell commands with your approval

✅ **Allow list** — Type `a` to trust a tool for the entire session

🔌 **MCP Protocol** — Connect external AI tool servers

📟 **Tmux support** — Seamless splits and popups

🐚 **Multi-shell** — zsh, bash, and fish supported

## Installation

### Quick Install

```bash
curl -fsSL https://raw.githubusercontent.com/mudler/wiz/main/install.sh | bash
```

Or, if you use zsh:


```bash
curl -fsSL https://raw.githubusercontent.com/mudler/wiz/main/install.sh | zsh
```


### From Source

```bash
git clone https://github.com/mudler/wiz
cd wiz
go build -o wiz .
sudo mv wiz /usr/local/bin/
```

### Go Install

```bash
go install github.com/mudler/wiz@latest
```

## Usage

### Summon the Wizard (TUI Mode)

```bash
wiz --height 40%
```

### CLI Mode

```bash
wiz
```

### Shell Integration

Add to your shell config to enable `Ctrl+Space`:

**zsh** (~/.zshrc):
```bash
eval "$(wiz --init zsh)"
```

**bash** (~/.bashrc):
```bash
eval "$(wiz --init bash)"
```

**fish** (~/.config/fish/config.fish):
```fish
wiz --init fish | source
```

Then press `Ctrl+Space` anywhere to summon the wizard!

## Configuration

Create a config file at `~/.config/wiz/config.yaml` or `~/.wiz.yaml`:

```yaml
# Required: Your LLM configuration
model: gpt-4o-mini
api_key: your-api-key
base_url: https://api.openai.com/v1

# Optional: Custom system prompt
prompt: |
  You are a helpful terminal wizard...

# Optional: Agent behavior
agent_options:
  iterations: 10
  max_attempts: 3
  max_retries: 3
  force_reasoning: false

# Optional: Additional MCP servers
mcp_servers:
  filesystem:
    command: npx
    args:
      - "-y"
      - "@anthropic/mcp-filesystem"
      - "/home/user"
    env:
      foo: bar
```

### Environment Variables

You can also configure via environment variables:

```bash
export MODEL=gpt-4o-mini
export API_KEY=your-api-key
export BASE_URL=https://api.openai.com/v1
```

## Tool Approval

When the wizard wants to run a command, you'll see a prompt:

```
┌──────────────────────────────────────┐
│ 🔧 bash                              │
│                                      │
│ Arguments: {"script": "ls -la"}      │
│ 💭 Listing directory contents...     │
│                                      │
│ [y]es  [a]lways  [n]o  or adjust     │
└──────────────────────────────────────┘
```

**Options:**
- `y` or `yes` — Approve this execution
- `a` or `always` — Approve and add to session allow list (won't ask again)
- `n` or `no` — Deny execution
- *anything else* — Treated as an adjustment to the command

## MCP Servers

Wiz uses the [Model Context Protocol](https://modelcontextprotocol.io/) for tool execution.

### Built-in Tools

- **bash** — Execute shell scripts

### Adding External MCP Servers

Add to your config:

```yaml
mcp_servers:
  my_server:
    command: /path/to/mcp-server
    args:
      - --some-flag
    env:
      API_KEY: secret
```

## Tmux Integration

When running inside tmux, wiz automatically uses a split pane for the TUI. Use `--no-tmux` to disable this behavior.

## License

MIT
