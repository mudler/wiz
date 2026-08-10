<h1 align="center">
  <br>
  <img width="460" alt="nib" src="docs/images/logo.png" />
  <br>
</h1>

<p align="center">
  <b>A tiny, zero-dependency LLM agent harness that lives in your terminal.</b><br>
  One static binary. No runtime, no daemon, no cloud. Local-LLM friendly. Summon it anywhere with <code>Ctrl+Space</code>.
</p>

<p align="center">
  <img alt="License" src="https://img.shields.io/github/license/mudler/nib">
  <img alt="Go" src="https://img.shields.io/github/go-mod/go-version/mudler/nib">
  <img alt="Release" src="https://img.shields.io/github/v/release/mudler/nib?sort=semver">
  <img alt="CI" src="https://github.com/mudler/nib/actions/workflows/test.yml/badge.svg">
</p>

<p align="center">
  <a href="#why-nib">Why nib</a> •
  <a href="#quickstart">Quickstart</a> •
  <a href="#usage">Usage</a> •
  <a href="#plugins">Plugins</a> •
  <a href="#skills">Skills</a> •
  <a href="#configuration">Configuration</a> •
  <a href="#tool-approval">Tool Approval</a>
</p>

<p align="center">
  <img alt="nib in the terminal: ask a question, approve a command, get an answer" src="docs/images/demo-tui.gif" width="800">
</p>

---

## Why nib

Most LLM coding agents are big: a Node/Python runtime, a pile of dependencies, a login, a
background service. **nib is the opposite.** It's a single ~20 MB Go binary you drop on any
machine — laptop, server, container, a box you SSH'd into — and it just runs. Point it at
any OpenAI-compatible endpoint (including a local model) and press `Ctrl+Space`.

Small doesn't mean toy. nib is a real agent harness:

- **Tool use with approval** — it runs shell commands, but every call passes an approval gate you control.
- **Sub-agents** — delegate self-contained subtasks (`explore`, `plan`, …) that run in the foreground or background.
- **MCP** — connect any [Model Context Protocol](https://modelcontextprotocol.io/) server for extra tools, local or remote, with one command (`nib mcp add`).
- **Plugins** — installable packages that add MCP servers, sub-agents, prompt fragments, skills, slash commands, and lifecycle hooks. **Claude Code plugins work too.**
- **Skills** — install skill packs (e.g. [`obra/superpowers`](https://github.com/obra/superpowers)) and let the agent load them on demand.

Think of it as the **`fzf` for LLMs**: portable, keyboard-driven, composable, and out of your way.

| | nib | typical agent CLIs |
|---|---|---|
| Install | one static binary | runtime + package tree |
| Dependencies | **zero** | many |
| Local LLMs | first-class | varies |
| Summon | `Ctrl+Space`, anywhere | launch a session |
| Extend | plugins · skills · MCP | varies |
| Footprint | ~20 MB | hundreds of MB |

## Features

- **`Ctrl+Space` anywhere** — summon nib straight from your shell prompt; inline like `fzf`, or a tmux split when you're in tmux.
- **Two modes** — a polished TUI, or a plain `--cli` mode for pipes and scripts.
- **Tool execution with approval** — the AI proposes commands; you approve, deny, edit, or trust for the session.
- **Sub-agents & background jobs** — delegate to typed sub-agents; background them (`Ctrl+B`) and watch the jobs footer (`Ctrl+J`).
- **Plugins** — `nib plugin install <git-url|local-path|zip|catalog-name>`; six contribution types; Claude-Code-plugin compatible.
- **Skills** — `nib skill install <git-url|local-path|zip|url|catalog-name>`; progressive-disclosure skill packs loaded on demand.
- **Catalog** — `nib skill browse` / `nib plugin browse` discover extensions from agentskills.io-compatible index sources; a starter catalog ships built in.
- **MCP protocol** — bring any external tool server.
- **tmux-native** — seamless splits and popups.
- **Multi-shell** — zsh, bash, and fish.
- **Zero dependencies** — one portable binary, trivial to install and upgrade.

## Quickstart

**1. Install**

```bash
curl -fsSL https://raw.githubusercontent.com/mudler/nib/master/install.sh | bash
```

<details>
<summary>Other ways to install</summary>

```bash
# zsh users
curl -fsSL https://raw.githubusercontent.com/mudler/nib/master/install.sh | zsh

# from source
git clone https://github.com/mudler/nib && cd nib && go build -o nib . && sudo mv nib /usr/local/bin/

# go install
go install github.com/mudler/nib@latest
```
</details>

**2. Configure** a model — `~/.config/nib/config.yaml`:

```yaml
model: gpt-4o-mini
api_key: your-api-key
base_url: https://api.openai.com/v1   # or your local endpoint, e.g. http://localhost:8080/v1
```

**3. Press `Ctrl+Space`** in your terminal (or just run `nib`). That's it.

## Usage

Run `nib` to open the TUI, or press `Ctrl+Space` from your shell. Use `--cli` for a plain,
pipe-friendly mode:

```bash
echo "how do I list every open port?" | nib --cli
```

Running out of input ends the session, so a piped question is answered and nib exits `0`.
`Ctrl+D` does the same thing interactively. `Ctrl+C` still exits non-zero, so a script can
tell an interrupted run from a finished one.

#### What a piped run may and may not do

A piped run still uses tools, but only the ones that cannot change anything. In the default
approval mode nib auto-approves **read-only** tools without asking, so a piped question can
read your files and inspect your repo to answer you:

- read-only tools: `read`, `grep`, `glob`, `read_image`, `read_video`, `transcribe_audio`,
  `bash_jobs`, `bash_job_output`, `agent_logs`, `check_agent`, `get_agent_result`,
  `cron_list`
- read-only shell commands, run through `bash` or `bash_background`: `ls`, `cat`, `head`, `tail`, `grep`, `rg`,
  `wc`, `pwd`, `stat`, `file`, `du`, `df`, `basename`, `dirname`, `echo`, `printf`,
  `whoami`, `uname`, `which`, `type`, `cut`, `column`, `nl`, `less`, `more`, plus specific
  subcommands of mixed tools: `git status|log|diff|show|blame|describe|rev-parse|ls-files`,
  `go list|version|doc|vet`, `docker ps|images|inspect|logs|version|info`,
  `kubectl get|describe|logs|version`, `npm ls|list|view|outdated`, `cargo tree|metadata`.
  Extend the list with `read_only_commands` in `config.yaml`

Everything else needs approval, and a piped run has nobody at the keyboard to give it, so
those calls are **denied** and the session ends. A closed stdin is not consent. That
includes every write, every other shell command, and every MCP or plugin tool.

The exit code says which happened:

| code | meaning |
|---|---|
| `0` | answered, and the input ran out (also `exit` and `Ctrl+D`) |
| `1` | a failure, including `Ctrl+C` and a broken stdin |
| `2` | an unparseable flag |
| `3` | a tool call needed approval and stdin was closed, so nib refused to act |

`3` exists so a script that discards stdout can still tell "here is your answer" from "I
refused to act". To let a piped run act, say so explicitly with `--yolo` (`NIB_YOLO=1`),
which auto-approves every tool call, or allowlist the tools you trust in `config.yaml`.

### Summon nib from your shell (`Ctrl+Space`)

The `install.sh` script wires this up for you. Inside tmux, nib opens in a split pane so it
never disturbs what you're doing:

<p align="center">
  <img alt="press Ctrl+Space to summon nib in a tmux split and ask for a command" src="docs/images/demo-tmux.gif" width="800">
</p>

To wire it up manually, add the line for your shell:

```bash
eval "$(nib --init zsh)"      # ~/.zshrc
eval "$(nib --init bash)"     # ~/.bashrc
nib --init fish | source      # ~/.config/fish/config.fish
```

### Sub-agents & background jobs

Ask nib to delegate, and it spawns a typed sub-agent (`explore`, `plan`, or any you
configure). Background a running job with `Ctrl+B` and watch the jobs footer with `Ctrl+J`:

<p align="center">
  <img alt="nib delegating to the explore sub-agent, with the jobs footer" src="docs/images/demo-agents.gif" width="800">
</p>

### `/loop` — recurring & self-paced tasks

- `/loop 5m /foo` — run `/foo` (a slash command or prompt) every 5 minutes.
- `/loop /foo` — self-paced: the model runs `/foo`, then decides when to repeat
  by scheduling its own wake-ups; it stops when the task is done.
- `/loop list` — show active loops.
- `/loop stop [id]` — stop one loop, or all loops if no id is given.

Loops are session-only by default. The model can also schedule jobs directly
with the `cron`, `cron_list`, and `cron_delete` tools; `cron(durable: true)`
persists across restarts to `.nib/loops.json`.

### `/goal` — keep going until a goal is met

- `/goal <text>` — set a goal. nib keeps working and re-checks it every time
  the model would stop, only finishing when the model decides the goal is met
  (it calls a `goal_done` tool) or you stop it.
- `/goal` — show the current goal.
- `/goal clear` — clear it. Pressing `Ctrl+C` during pursuit also clears it.

Unlike `/loop`, `/goal` is not scheduling — there are no timers. It's an
in-turn "keep going" gate: the model self-judges progress and continues until
done. You can still chat and steer while a goal is being pursued. Goals are
session-only and single (setting a new one replaces the old).

### `/model` and `/models`: switch model mid-session

- `/models` lists the models the configured endpoint serves, marking the
  current one with `*`. Bare `/model` lists too, so forgetting the name gets
  you the menu rather than an error.
- `/model <name>` switches the session to that model.

The switch **keeps the conversation**: history carries over to the new model.
It applies from the next turn (a turn already in flight finishes on the model
it started with), and the sub-agents those turns spawn follow along. Use
`/compact` first when the history needs trimming, for instance before moving to
a model with a smaller context window.

A name the endpoint does not serve is refused, and the refusal lists what it
does serve. If the lookup fails or the endpoint advertises nothing (it is
bounded at 3 seconds, since it runs on the goroutine that draws the prompt),
the switch still goes through and is marked unverified: a broken endpoint may
be the very reason you are switching. Both the TUI and `--cli` support this.

## Plugins

A **plugin** is a single installable unit — a git repo (or local dir) with a
`nib-plugin.yaml` manifest — that can contribute any combination of:

| Contribution | What it adds |
|---|---|
| `mcp_servers` | external MCP tool servers |
| `agents` | typed sub-agents the agent can spawn |
| `prompt_fragments` | extra system-prompt text (inline or from a file) |
| `skills` | skills indexed in the prompt, loaded on demand |
| `commands` | slash commands, optionally routed through a sub-agent |
| `hooks` | shell commands bound to lifecycle events (e.g. `SessionStart`, `PreToolUse`) |

```bash
nib plugin install <git-url|local-path|zip|catalog-name>   # [--ref <tag|branch>] [--yes]
nib plugin browse                          # discover installable plugins from the catalog
nib plugin search <query>
nib plugin source <list|add URL|enable|disable|remove LABEL>
nib plugin list
nib plugin enable|disable <name>
nib plugin update|remove <name>
```

The source can be a git URL, a local directory, a **`.zip` archive** (extracted with
zip-slip protection), or a **catalog name** (`nib plugin browse` to discover them). Install
prints a summary of what the plugin contributes and asks for confirmation (`--yes` to skip).
Plugins install **disabled** by default; a disabled plugin contributes nothing, and every
tool call still passes the approval gate at runtime. See **[Catalog](#catalog)** below for
where `browse`/`search` pull from.

A minimal `nib-plugin.yaml`:

```yaml
name: my-plugin
version: 1.0.0
description: adds a sub-agent and a slash command

agents:
  - name: researcher
    description: investigates a self-contained subtask
    system_prompt: You are a focused research sub-agent.
    tools: [bash]

commands:
  - name: review
    description: review the given input
    prompt: "Review the following: {{.Args}}"
    agent: researcher
```

**Claude Code compatible.** `nib plugin install` also installs an unmodified Claude Code
plugin (`.claude-plugin/` layout) or marketplace, mapping its `plugin.json`, `skills/`,
`commands/`, `agents/`, `hooks/`, and `.mcp.json` into nib's model.

See **[`examples/nib-plugin-demo`](examples/nib-plugin-demo)** for a reference plugin that
exercises all six contribution types.

## Skills

A **skill pack** is a git repo (or local dir) containing a `skills/<name>/SKILL.md`
collection — for example [`obra/superpowers`](https://github.com/obra/superpowers). nib
harvests every skill, indexes it (name + description) in the system prompt, and the agent
pulls in a skill's full instructions on demand via the `load_skill` tool — or you inject one
eagerly for the session with `/skill <name>`. A single `SKILL.md` at the pack root is treated
as a one-skill pack (the [agentskills.io](https://agentskills.io) single-file form).

```bash
nib skill install <git-url|local-path|zip|url|catalog-name>  # [--ref REF] [--yes] [--link]
nib skill browse                          # discover installable skills from the catalog
nib skill search <query>
nib skill source <list|add URL|enable|disable|remove LABEL>
nib skill list
nib skill enable|disable <name>
nib skill update|remove <name>
```

The source can be a git URL, a local directory, a **`.zip` archive** (extracted with
zip-slip protection), a **URL to a bare `SKILL.md`** (the agentskills.io well-known form,
e.g. `https://host/.well-known/skills/<name>/SKILL.md`), or a **catalog name**
(`nib skill browse` to discover them). Like plugins, skill packs install **disabled**; enable
the ones you want with `nib skill enable <name>`. Skill packs carry their bundled files, so a
skill can `Read` or run scripts from its own directory at runtime.

## Catalog

`nib skill browse` / `nib plugin browse` discover installable extensions from a set of
**catalog sources** — each an [agentskills.io](https://agentskills.io)-compatible
`index.json` (a direct URL, a bare host probed at `/.well-known/skills/index.json`, or a
GitHub repo resolved index-first with a tree-crawl fallback). A small starter catalog ships
built into the binary, so `browse` works offline. Manage sources with `nib skill source`
(shared with `nib plugin source` — one source list feeds both):

```bash
nib skill source list
nib skill source add github.com/openclaw/agent-skills
nib skill source disable <label>          # built-in sources can be disabled, not removed
nib skill source remove <label>
```

Sources are resolved at runtime (nothing is crawled at build time), results are merged and
deduped across sources, and a source that fails to fetch is skipped without failing the rest.
Installing from the catalog lands the extension **disabled**, same as every other install.

## Agent over MCP (`nib mcp`)

nib can expose its agent as an **MCP server** so an external program can drive it
headless — a voice client (owning the mic/speaker and speech-to-text/text-to-speech),
a chat frontend, an IDE extension, an automation script, anything that speaks MCP.
nib stays a pure-Go static binary; the consumer lives entirely outside it.

```bash
nib mcp                       # serve over stdio (default; the client launches nib)
nib mcp --http --addr :8090   # serve over streamable HTTP instead
```

The server exposes two tools and one notification:

- `converse(utterance)` — send a message to the agent; returns the **first** reply
  immediately (even while background work continues, `pending: true`), so turns stay
  responsive instead of blocking until a multi-step task finishes.
- `interrupt()` — cancel the current turn.
- `notifications/message` (logger `nib`, payload `kind: "reply" | "error"`) — replies
  produced *after* the synchronous `converse` (finished sub-agents / shell jobs,
  resumes) arrive here, carrying the same `turn` id.

After connecting, the client **must** call MCP `logging/setLevel` with level
`info` (or lower), or it will receive no `nib/reply` / `nib/error` notifications:
the server emits them at info/error level, and the SDK gates logging
notifications behind the level the client has set.

In this mode tool calls are auto-approved (there is no terminal to prompt at);
set `approval_mode: allowlist` + `allowed_tools` in your config to restrict it.

The server adds **no prompt of its own** — the agent uses your configured
`system_prompt` as usual. To tune behavior for a particular consumer (e.g. a voice
client that wants short, spoken replies and long work pushed to the background),
set that in your `system_prompt` (or ship it as a plugin `prompt_fragment`); the
server stays consumer-agnostic.

## Configuration

nib looks for config (in order) in `./.nib.yaml`, `$XDG_CONFIG_HOME/nib/config.yaml`,
`~/.config/nib/config.yaml`, `~/.nib.yaml`, then `/etc/nib/config.yaml`.

```yaml
# Main LLM. provider defaults to "openai", accepting any local or remote
# OpenAI-compatible endpoint. Set it to "codex" to use `codex app-server
# --stdio` and an existing Codex/ChatGPT login instead.
# provider: openai
model: gpt-4o-mini
api_key: your-api-key
base_url: https://api.openai.com/v1

# Optional and disabled by default: enable provenance tracking, LLM span
# classification/redaction, and stricter approval checks for external data.
prompt_injection_protection:
  enabled: true
  # Optional independent classifier. If omitted, it inherits all top-level
  # model settings. Individual omitted fields below also inherit from the main
  # model, while provider/model/endpoint may all be overridden.
  classifier:
    provider: openai
    model: qwen3-4b
    api_key: local
    base_url: http://localhost:8080/v1

# For either role, provider: codex defaults to `codex app-server --stdio`.
# Codex retains the ChatGPT OAuth credentials; nib communicates over stdio and
# never reads the token. On Nix, make Codex available on PATH with
# `nix shell github:numtide/nix-ai-tools#codex` (or add it to the project flake).

# Optional: custom system prompt
prompt: |
  You are a calm, helpful terminal assistant...

# Optional: per-request metadata sent verbatim on every LLM request (the OpenAI
# "metadata" object). Backends such as LocalAI use it for per-request flags —
# e.g. disable a reasoning model's thinking:
metadata:
  enable_thinking: "false"

# Optional: OpenAI-standard reasoning effort, sent on every request as
# "reasoning_effort" ("none"/"low"/"medium"/"high"). Unlike metadata.enable_thinking,
# this works even when the model's chat template has no enable_thinking toggle
# (e.g. LFM2.5) — so it's the reliable way to turn a reasoning model's thinking off:
reasoning_effort: "none"

# Optional: agent behavior
agent_options:
  iterations: 10
  max_attempts: 3
  max_retries: 3
  force_reasoning: false

# Optional: tool-approval policy (default: prompt)
#   prompt    — ask before each tool call, but auto-approve read-only calls
#               (read, grep, glob, and safe read-only shell commands like
#               `ls`, `cat`, `git status`, `go list`). Mutating calls prompt.
#   strict    — ask before EVERY tool call, including read-only ones
#   allowlist — auto-approve the tools in allowed_tools, prompt for the rest
#   auto      — approve every tool call without prompting
approval_mode: prompt
allowed_tools:
  - bash

# Optional: extend the read-only bash command set (auto-approved in prompt
# mode). An entry with a space is a command+subcommand pair; otherwise it
# matches the command at any arguments. Read-only classification is
# conservative — anything compound or unrecognized still prompts.
read_only_commands:
  - terraform plan
  - kubectl get

# Optional: extra sub-agent types (general, explore, plan are built in)
agents:
  - name: researcher
    description: investigates a self-contained subtask
    system_prompt: You are a focused research sub-agent.
    tools: [bash]
    # Per-agent metadata overlays the global metadata above (per key):
    metadata:
      enable_thinking: "true"

# Optional: external MCP servers
mcp_servers:
  filesystem:
    command: npx
    args: ["-y", "@anthropic/mcp-filesystem", "/home/user"]
    env:
      FOO: bar
```

You can also configure the essentials via environment variables:

```bash
export MODEL=gpt-4o-mini
export API_KEY=your-api-key
export BASE_URL=https://api.openai.com/v1
```

To skip every approval prompt for a run ("yolo" mode), pass `--yolo` or set
`NIB_YOLO=1` — both force `approval_mode: auto` regardless of what the config
file says:

```bash
nib --yolo          # or: NIB_YOLO=1 nib
```

While it's active, nib shows it on screen: a `yolo` badge in the TUI header and
a one-line notice in the CLI banner. If opt-in prompt-injection protection is
enabled, its fresh-approval boundary remains active after external content.

## Tool Approval

By default nib auto-approves read-only tool calls (reads, searches, and safe
read-only shell commands) and only prompts for calls that can change state; use
`approval_mode: strict` to be prompted for everything.

When nib wants to run a mutating command, you decide:

```
▏ bash wants to run
▏ $ df -h
▏
▏ [1] run it once
▏ [2] always allow `df …`  (this session)
▏ [3] yes to everything this turn
▏ [n] no · [e] edit
```

In the **TUI**, approval is a single keypress (no Enter):

- `1` — run this call once
- `2` — always allow, scoped: for a simple shell command this grants just that
  command prefix (`df …`); for anything compound — or any other tool — it grants
  the whole tool. Session-only; sub-agents share the allow list. A command that
  chains (`&&`, `;`, `|`, `$( )`, …) never rides a prefix grant. A prefix grant
  trusts everything that command can do with its arguments — grant prefixes
  you'd trust with any flags.
- `3` — allow **all** tool calls for the rest of this turn (handy after delegating
  a multi-step task)
- `n` / `Esc` — deny
- `e` — edit the call, then submit

(`y`/`a`/`A` still work as aliases for `1`/`2`/`3`.)

In the **CLI** (`--cli`) the prompt is line-based: type `y`, `a`, `all`, `n`, or a free-form
change, then Enter. Read-only calls (reads, searches, safe read-only shell) already skip the
prompt by default; set `approval_mode: strict` to be prompted for those too. To skip prompting
entirely, set `approval_mode: auto` / `allowed_tools` in your config, or run with `--yolo`
(env: `NIB_YOLO=1`) to auto-approve ordinary tool calls. When
`prompt_injection_protection.enabled: true`, web, browser, attachment, and configured MCP
results are tracked as untrusted external data. A separate, tool-free LLM pass
identifies exact prompt-injection spans for redaction before they reach the main
agent; classification failures withhold the external text. Subsequent
consequential calls require fresh approval even under a broad session/turn grant.

## MCP Servers

nib speaks the [Model Context Protocol](https://modelcontextprotocol.io/). A set of
tools is built in — `bash`, the filesystem tools (`read`, `write`, `edit`, `glob`,
`grep`), and the web tools (`web_fetch`, `web_search`); add any external server with
the `nib mcp` CLI or directly in your config.

### `nib mcp` CLI

The quickest way to register a server — local (stdio) or remote (HTTP/SSE):

```bash
# Local stdio server: everything after `--` is the command and its args.
nib mcp add filesystem --env API_KEY=secret -- npx -y @anthropic/mcp-filesystem /home/user

# Remote server over streamable HTTP (default) or SSE.
nib mcp add docs --url https://example.com/mcp
nib mcp add docs --url https://example.com/mcp --transport sse

nib mcp list             # show configured servers
nib mcp test docs        # connect, list the server's tools, exit nonzero on failure
nib mcp remove docs      # delete a server
```

`nib mcp add` writes to your user config; servers become available on the next nib
session. (The agent itself can also register servers mid-session via its
`add_mcp_server` tool.) Note `nib mcp` with no subcommand serves nib's *own* agent
over MCP — see [Agent over MCP](#agent-over-mcp-nib-mcp) above.

### Config

The CLI just edits the `mcp_servers` map; you can also write it by hand:

```yaml
mcp_servers:
  my_server:                  # local (stdio) server
    command: /path/to/mcp-server
    args: ["--some-flag"]
    env:
      API_KEY: secret
  remote_docs:                # remote server
    url: https://example.com/mcp
    transport: sse            # "http" (default) or "sse"
```

## tmux

Inside tmux, nib automatically uses a split pane for the TUI. Pass `--no-tmux` to disable.

## Embedding nib

nib's entrypoint is importable, so another Go binary can ship it as a subcommand:

```go
import (
	"context"

	"github.com/mudler/nib/app"
	"github.com/mudler/nib/types"
)

func runAgent(ctx context.Context, args []string, modelFlag string) error {
	return app.Run(ctx, app.Options{
		Args:        args,           // the arguments after your subcommand
		ProgramName: "myprog agent", // see below for where it reaches
		BaseDir:     "/path/to/state",
		Defaults: types.Config{ // seeds fields the config file leaves empty
			BaseURL: "http://127.0.0.1:8080/v1",
		},
		Overrides: types.Config{ // beats the config file: for your own flags
			Model: modelFlag,
		},
		SkipSetup:   true, // you resolve the model yourself
		SkipBareEnv: true, // ignore bare MODEL / API_KEY / BASE_URL
	})
}
```

Every option's zero value reproduces standalone nib, so an embedder opts in to
each difference. `BaseDir` is the root for `config.yaml`, `plugins/` and
`skills/`, keeping embedded state out of a separately installed nib's.

### Defaults and Overrides

There are two config channels, and which one a value belongs in comes down to a
single question: should the user's config file be able to change it?

Precedence, lowest to highest:

| rung | who sets it |
|------|-------------|
| nib's built-in defaults | nib |
| `Options.Defaults` | the embedder, as a starting point |
| `config.yaml` | the user |
| `MODEL` / `API_KEY` / `BASE_URL` | the user's shell, unless `SkipBareEnv` |
| `Options.Overrides` | the embedder, as the last word |

`Defaults` is for values the user is meant to be able to change: a sensible
endpoint for a first run, a house prompt, a set of sub-agents. It fills what the
file left empty and loses to anything the user writes.

`Overrides` is for values the embedder resolved on the user's behalf and that
the file must not silently undo. **Your own CLI flags belong here.** Routed
through `Defaults`, an `--endpoint` or `--model` flag is accepted and then
ignored the moment the file carries a `base_url` or a `model`, which is the
normal state rather than an edge case: anything that writes the file once makes
the flag dead from the next run on.

Both channels merge every field of `types.Config`, not a hand-picked subset, and
both share the same single carve-out: `BaseDir` is ignored in each, because
`Options.BaseDir` is the only knob for the root. Nested structs merge per leaf
field, maps merge per key, and slices are all or nothing.

Neither merge can tell "set to the zero value" from "not set", so each is
one-way. A seeded `true` beats a `false:` in the file, and an overridden `false`
**cannot** beat a `true:` in the file. An override only ever raises a field: it
cannot blank a model, zero an iteration count, or switch
`browser.allow_private_urls` back off. To force one of those, own the config
file under a `BaseDir` you control rather than reaching over it.

Two fields still sit above `Overrides`, by design rather than by omission. nib's
own `--trace-dir` and `--yolo`, and their `NIB_TRACE_DIR` and `NIB_YOLO` twins,
are resolved after the config load and keep winning for `TraceDir` and
`ApprovalMode`. Both are deliberate instructions to nib rather than ambient
environment, which is what separates them from the bare `MODEL` / `API_KEY` /
`BASE_URL` that `SkipBareEnv` exists to suppress.

> **`NIB_YOLO=1` escalates past a stricter override.** If you set
> `Overrides.ApprovalMode` to `strict`, `allowlist` or `prompt`, that variable
> silently downgrades it to `auto`, which approves every tool call without
> prompting. Filtering `Args` does not help, because this one arrives through
> the environment: to prevent it, control the child process environment and do
> not pass `NIB_YOLO` through. `NIB_TRACE_DIR` escalates nothing — the config
> file cannot set `TraceDir` at all — but it is not inert either. A trace is an
> explicit request, so `app.Run` checks the directory before starting anything
> and returns `ExitError{1}` when it cannot be written, rather than running on
> and quietly producing no transcript. An inherited `NIB_TRACE_DIR` pointing
> somewhere unwritable therefore fails the whole run, not just the tracing, and
> `--trace-dir` behaves the same way.

`app.Run` never calls `os.Exit`, and it takes cancellation from the context you
pass rather than installing its own signal handling. Cancelling that context
unwinds whichever mode is running, the TUI included, and comes back as
`ExitError{1}` with the context's error on `Stderr`. Nothing below `app.Run`
listens for a signal, so an embedded nib is stopped by your handler cancelling
your context, which is also what restores the terminal. Every failure comes back
as a bare `app.ExitError` carrying nothing but an exit code, the cause having
been printed to `Stderr` instead: an unparseable flag, an unknown `--init`
shell, an MCP transport failure, the setup abort, the stream refusal described
below, a management subcommand's non-zero code and a TUI error are all
indistinguishable to the caller. The code is `1` for every one of those except
two: an unparseable flag is `2`, and a CLI session that refused a tool call
because stdin could not approve it is `app.ExitCodeApprovalNoInput` (`3`), which
is separable precisely so a caller can act on it. `app.Main(os.Args) int` is the same
entrypoint shaped for a `main` function: it installs nib's SIGINT/SIGTERM
handling and takes no options, which is all nib's own `main.go` needs. That
handling covers the modes that take a context; the management subcommands below
run under the process default disposition, so `Ctrl+C` kills them outright,
exactly as it does for standalone nib.

**`ProgramName` renames what `app` itself prints, and the `--init` scripts.**
The printed messages are the `--version` line, the flag package's usage and
parse errors, the setup gate's two aborts, and the injected-stream refusal
described below.

The `--init` snippets are more than a cosmetic rename: the `Ctrl+Space` widget
they define invokes this name, so `myprog agent --init zsh` emits a widget that
runs `myprog agent`, not a `nib` your users never installed. A name of several
words stays several words where a command belongs, and the widget's function
name is derived from it by reducing it to an identifier, so `local-ai chat`
defines `__local_ai_chat_widget`. An empty `ProgramName` still emits standalone
nib's script byte for byte.

The rule for the rest is one line: **if a string names a command that someone is
expected to run, `ProgramName` renames it; if it merely names the tool, it does
not.**

Renamed:

- **The tmux split.** Inside tmux, `Ctrl+Space` lands in a split pane, and that
  pane has to re-enter *this* program. That means the host binary **plus its
  subcommand**, not the executable path alone, so without this an embedded nib
  spawns your default command instead of the agent.
- **The management subcommands' instructions**: the `usage: ...` lines of
  `plugin`, `skill` and `mcp`, and the hints that follow an install, such as
  `Enable later: local-ai chat plugin enable foo` and `verify now with:
  local-ai chat mcp test bar`. A non-interactive `plugin install` without
  `--yes` reaches that enable hint every time.
- **The skill installer's suggestions**, which surface through those same
  subcommands: "use `local-ai chat skill update x`" on a duplicate pack, and
  "did you mean `local-ai chat plugin install`?" on a source with no
  `SKILL.md`. The catalog install path included.
- **The system prompt.** nib tells the model how a user can register MCP servers
  from the command line, and the model relays that as advice in its own words.
  Named wrong, your assistant confidently tells your users to run a binary they
  do not have, and unlike a usage line there is no cue anywhere that the tool
  goes by another name here.

Not renamed, deliberately, so these still say `nib`:

- The CLI and TUI branding, and the setup wizard's header. They name the tool;
  they are not commands.
- Prose in terminal output that names the tool rather than instructing:
  specifically the "available on the next nib session" clause of the `mcp add`
  confirmation, whose `verify now with:` half **is** renamed. The surrounding
  output makes the referent obvious. The equivalent sentence in the system
  prompt, which has no such context, *is* renamed.
- The `nib: ...` diagnostics that config loading and plugin/skill discovery
  write straight to `os.Stderr` on every run.
- Internal identifiers that are never shown and never typed: the MCP server's
  implementation name and its `nib/reply` notification method, the
  `nib-plugin.yaml` manifest filename, the `~/.config/nib` state root, and the
  tmux temp-file prefix and wait-for channel.

**The management subcommands do not honor injected streams.** Every `plugin`
and `skill` subcommand, and every `nib mcp` subcommand that manages configured
servers rather than serving the agent, reads `os.Stdin` and writes `os.Stdout`
and `os.Stderr` directly. So an embedded `nib plugin install` with no terminal
behind stdin hits EOF on the "Enable this plugin?" prompt, reads that as "no",
and exits **0** with the plugin installed but left **disabled**, having said so
on a stream the embedder never set. Pass `--yes` to install and enable in one
step.

### Injecting streams

`Options.Stdin`, `Stdout` and `Stderr` default to the process streams when nil,
but only CLI mode honors them fully. The TUI, which is the **default** mode,
renders on `/dev/tty` deliberately: stdout is often a pipe while the terminal is
still there, and serving that case is the whole reason it opens `/dev/tty` (it
is what makes the `Ctrl+Space` shell widget work). Only the shell-capture line
the TUI prints on exit goes to `Stdout`.

So, for an embedder:

- Injecting a `Stdin` or a `Stdout` that is **not** a terminal (a buffer, a
  pipe, a file) requires `--cli`. Without it the run is refused, with an error
  naming `--cli` and the offending stream, rather than rendering into a writer
  the TUI cannot drive and leaving it empty. Only those two are gated: a
  non-terminal `Stderr` is always accepted. What it receives is the errors
  `app.Run` itself prints, which is not all of nib's error output: whatever the
  TUI reports from inside a running session goes to `/dev/tty` along with the
  rest of the interface.
- Injecting real terminals, including the process streams while attached to
  one, leaves every mode working.
- To keep nib's shell-capture idiom, where a user runs `out=$(myprog agent)`
  and the TUI prints the selected command for the shell to pick up, leave
  `Stdout` **nil**, not `os.Stdout`. Nil means "not injected": nib falls back
  to the process stream and the capture line lands on stdout as it should.
  Setting `os.Stdout` explicitly injects whatever stdout happens to be, and
  under `$(...)` that is a pipe, so the TUI refuses a stream it would have used
  anyway.

## License

MIT
