package types

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	openai "github.com/sashabaranov/go-openai"
)

// AgentOptions holds configuration for the cogito ExecuteTools function
type AgentOptions struct {
	Iterations     int  `yaml:"iterations"`
	MaxAttempts    int  `yaml:"max_attempts"`
	MaxRetries     int  `yaml:"max_retries"`
	ForceReasoning bool `yaml:"force_reasoning"`
}

// CompactionConfig controls conversation compaction: summarizing older turns
// into a single summary message while keeping recent turns verbatim.
type CompactionConfig struct {
	// Disabled turns OFF automatic compaction. Zero value (false) = auto ON.
	Disabled bool `yaml:"disabled"`
	// MaxContextTokens is the model context window used to compute the trigger.
	// 0 → default 128000.
	MaxContextTokens int `yaml:"max_context_tokens"`
	// Threshold is the fraction of MaxContextTokens at which auto-compaction
	// fires. 0 → default 0.8.
	Threshold float64 `yaml:"threshold"`
	// KeepRecent is the number of trailing messages kept verbatim. 0 → default 8.
	KeepRecent int `yaml:"keep_recent"`
	// ReserveTokens is held back from the context window before the threshold
	// applies, because nib's request is not the only claim on the window — the
	// response needs room in it too.
	//
	// A fixed count rather than a fraction on purpose: a reply does not get
	// longer because the window did, so a percentage over-reserves badly at
	// large windows and under-reserves at small ones. 0 → default 4096.
	//
	// It is capped at a quarter of the window in use, so that a flat count
	// larger than the window itself cannot reserve all of it and leave
	// compaction with nothing to trigger on (see chat.contextBudget). At the
	// 4096 default the cap applies only below a 16384-token window.
	ReserveTokens int `yaml:"reserve_tokens"`
}

// ToolOutputPruningConfig controls replacing stale or oversized tool results
// with a short stub in the messages sent to the model. It never changes the
// conversation nib stores.
//
// The booleans are negative-sense on purpose, like CompactionConfig.Disabled: an
// unset Go bool is false, so a positively-named `stale_reads: true` would mean
// that omitting the key DISABLES the rule — the opposite of the intended
// default.
//
// Every field must stay comparable (bool/int): config defaulting decides the
// block is absent by comparing it against the zero struct, which stops
// compiling the moment a slice or map field is added here.
type ToolOutputPruningConfig struct {
	// Disabled turns OFF both rules. Zero value (false) = pruning ON.
	Disabled bool `yaml:"disabled"`
	// DisableStaleReads turns off only the rule that stubs a read whose file a
	// later edit or write changed. Zero value (false) = that rule ON.
	DisableStaleReads bool `yaml:"disable_stale_reads"`
	// HighWaterTokens is the total tool-output size at which the oldest-first
	// sweep starts. 0 disables size pruning entirely, leaving the stale-read
	// rule in force. Unset (whole block absent) → default 24000.
	//
	// Because the block is defaulted as a whole, a lone `high_water_tokens: 0`
	// is byte-identical to an absent block and gets 24000 back. To actually
	// switch size pruning off, pair it with a non-zero sibling that marks the
	// block present — `disabled: false` and `low_water_tokens: 0` do not, since
	// they are zero values too:
	//
	//	tool_output_pruning:
	//	  high_water_tokens: 0
	//	  low_water_tokens: 8000
	HighWaterTokens int `yaml:"high_water_tokens"`
	// LowWaterTokens is the size the sweep prunes down to. Pruning deeply and
	// rarely beats pruning shallowly and constantly: each sweep buys many
	// subsequent calls at full prefix-cache reuse. Unset → default 8000.
	LowWaterTokens int `yaml:"low_water_tokens"`
	// MinResultTokens is the floor below which a result is never stubbed, since
	// there is nothing to reclaim. Unset → default 200.
	MinResultTokens int `yaml:"min_result_tokens"`
}

// AgentTypeConfig is a wiz-facing sub-agent type. It maps 1:1 to a
// cogito.AgentDefinition. Zero-valued numeric fields mean "inherit".
type AgentTypeConfig struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	SystemPrompt string   `yaml:"system_prompt"`
	Tools        []string `yaml:"tools"`
	Model        string   `yaml:"model"`
	Temperature  float32  `yaml:"temperature"`
	Iterations   int      `yaml:"iterations"`
	MaxAttempts  int      `yaml:"max_attempts"`
	MaxRetries   int      `yaml:"max_retries"`
	// Metadata overlays the global Config.Metadata for this agent type
	// (per-key: agent keys win, global-only keys are inherited).
	Metadata map[string]string `yaml:"metadata,omitempty"`
}

// Skill is a named, on-demand instruction set. Its Description is listed in the
// system prompt; the agent calls the load_skill tool to read Instructions.
type Skill struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Instructions string   `yaml:"instructions"` // resolved body (inline, or loaded from a plugin file)
	Tools        []string `yaml:"tools,omitempty"`
	Dir          string   `yaml:"-"` // absolute on-disk dir for bundled files; runtime-only, never serialized
}

// CommandConfig is a named slash command: a prompt template (text/template with
// {{.Args}} and {{.CurrentDirectory}}) optionally routed through a sub-agent.
type CommandConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Prompt      string `yaml:"prompt"`
	Agent       string `yaml:"agent,omitempty"`
}

// HookConfig is a shell command bound to a lifecycle event. Matcher (optional)
// is matched against the tool name for PreToolUse/PostToolUse. Dir is the
// plugin root (set during merge); it is the command's working directory and is
// exported as ${NIB_PLUGIN_ROOT}/${CLAUDE_PLUGIN_ROOT}.
type HookConfig struct {
	Event   string `yaml:"event"`
	Matcher string `yaml:"matcher,omitempty"`
	Command string `yaml:"command"`
	Dir     string `yaml:"-"` // plugin root; set during merge, not parsed
}

// Config holds configuration for creating a new session
type Config struct {
	Model   string `yaml:"model"`
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	// Specialist models for attachment handling. Empty ⇒ LocalAI auto-selects
	// by usecase (FLAG_TRANSCRIPT / FLAG_VISION).
	TranscribeModel string `yaml:"transcribe_model,omitempty" json:"transcribe_model,omitempty"`
	VisionModel     string `yaml:"vision_model,omitempty" json:"vision_model,omitempty"`
	VideoModel      string `yaml:"video_model,omitempty" json:"video_model,omitempty"`
	LogLevel        string `yaml:"log_level"`
	Prompt          string `yaml:"prompt"`
	// Metadata is a per-request metadata object attached verbatim to every
	// chat-completion request (the OpenAI "metadata" field). Backends such as
	// LocalAI use it for per-request flags, e.g. {"enable_thinking": "false"}
	// to disable reasoning. Applied to the main session and inherited by
	// sub-agents (see AgentTypeConfig.Metadata for per-agent overrides).
	Metadata map[string]string `yaml:"metadata,omitempty"`
	// ReasoningEffort sets the OpenAI "reasoning_effort" on every request
	// ("none"/"low"/"medium"/"high"). Unlike Metadata.enable_thinking, this binds
	// even when the model's chat template has no enable_thinking toggle (e.g.
	// LFM2.5), so it's the reliable way to disable a reasoning model's thinking
	// ("none"). Empty leaves the field unset.
	ReasoningEffort string               `yaml:"reasoning_effort,omitempty"`
	MCPServers      map[string]MCPServer `yaml:"mcp_servers"`
	AgentOptions    AgentOptions         `yaml:"agent_options"`
	Compaction      CompactionConfig     `yaml:"compaction"`
	// ToolOutputPruning shrinks what old tool results cost in the request
	// without touching the stored conversation.
	ToolOutputPruning ToolOutputPruningConfig `yaml:"tool_output_pruning"`
	Agents            []AgentTypeConfig       `yaml:"agents"`

	PromptFragments []string `yaml:"prompt_fragments"`
	Skills          []Skill  `yaml:"skills"`

	Commands []CommandConfig `yaml:"commands"`

	Hooks []HookConfig `yaml:"hooks"`

	// ApprovalMode controls tool-call gating:
	//   "" / "prompt"  ask the user, but auto-approve read-only calls
	//   "strict"       ask the user for every call (no read-only auto-approval)
	//   "allowlist"    auto-approve only the tools in AllowedTools, prompt the rest
	//   "auto"         approve every tool call
	ApprovalMode string `yaml:"approval_mode"`
	// AllowedTools are tool names pre-approved without prompting (always honored;
	// the basis of "allowlist" mode).
	AllowedTools []string `yaml:"allowed_tools"`
	// BuiltinTools, if non-empty, restricts which built-in tools and
	// self-config tools are exposed to the model (by name) — an allowlist.
	// Empty means all of them. Trims the prompt for small local models;
	// independent of AllowedTools (which gates approval). Never restricts
	// tools from user-configured MCP servers (mcp_servers:) — those are
	// always exposed, since restricting them would defeat the point of
	// configuring the server. See chat.Session's MCP tool filter.
	BuiltinTools []string `yaml:"builtin_tools,omitempty"`
	// ReadOnlyCommands extends the built-in set of bash commands treated as
	// read-only (auto-approved in the default "prompt" mode). An entry with a
	// space is a command+subcommand pair (e.g. "terraform plan"); otherwise it
	// matches that command at any arguments. User entries are merged with, not
	// replacing, the built-in set.
	ReadOnlyCommands []string `yaml:"read_only_commands"`
	// TraceDir, when non-empty, enables session tracing: each LLM call's raw
	// request/response is appended to <TraceDir>/trace.ndjson, and the session's
	// token totals are written to <TraceDir>/usage.json when it closes. Set at
	// runtime from the --trace-dir flag or NIB_TRACE_DIR env, not from the YAML
	// config.
	TraceDir string `yaml:"-"`
	// InitialHistory seeds a new session with a prior conversation so the very
	// next SendMessage continues with full memory of it (resume/rehydration).
	// Typically these are the messages a previous session returned from
	// Session.ExportHistory, persisted to disk and reloaded. They MUST NOT
	// include the system prompt: it is regenerated per-model/locale from Prompt
	// and re-applied on every turn (see Session.SendMessage), so a seeded system
	// message would duplicate it. Set at runtime, never from the YAML config.
	InitialHistory []openai.ChatCompletionMessage `yaml:"-"`
	// WorkingDir, when non-empty, is the directory host tools (bash, filesystem)
	// operate in. Runtime-only; empty means the process cwd (legacy behavior).
	WorkingDir string `yaml:"-"`
	// BaseDir overrides the config/plugins/skills root for this invocation, so
	// an embedder's state does not collide with a separately installed nib.
	// Set by config.LoadWith from LoadOptions.BaseDir; empty means nib's own
	// default resolution, so resolve it through plugin.BaseDirIn (or
	// config.WritablePathIn for the config file) rather than using it raw.
	// Runtime-only: never read from or written to YAML.
	BaseDir string `yaml:"-"`
	// ProgramName is what the user types to reach this program, e.g. "nib" or an
	// embedder's "local-ai chat". Empty means "nib".
	//
	// It rides on Config for one reason: the system prompt is rendered from
	// Config, and that prompt tells the MODEL how the user can register MCP
	// servers from the command line. Named wrong, the model repeats the wrong
	// command as advice, in its own words, whenever it seems relevant, and the
	// user has no cue that the tool is called something else here.
	//
	// Runtime-only, exactly like TraceDir: set by the embedder through
	// app.Options.ProgramName, never read from or written to YAML, so no config
	// file can rename the program out from under the binary that is running.
	// A user template can still read it as {{.Config.ProgramName}}.
	ProgramName string `yaml:"-"`
	// Computer is the opt-in desktop-control capability (cua-driver). Runtime-only.
	Computer ComputerConfig `yaml:"-"`
	// Browser is the opt-in browser-automation capability (chromedp-driven).
	Browser BrowserConfig `yaml:"browser,omitempty"`
}

// ComputerConfig configures the built-in computer_use MCP server. When Enabled,
// nib spawns Command (the cua-driver binary) as a stdio MCP child. nib never
// bundles the binary; Command is resolved by the consumer (or NIB_CUA_DRIVER_CMD).
type ComputerConfig struct {
	Enabled   bool
	Command   string
	Args      []string
	Env       map[string]string
	SessionID string
}

// BrowserConfig configures the built-in browser MCP server. When Enabled, nib
// starts a headed Chromium (reusing ChromePath, or auto-discovered) on the
// dedicated persistent profile ProfileDir and exposes the browser_* tools.
type BrowserConfig struct {
	Enabled          bool   `yaml:"enabled,omitempty"`
	ChromePath       string `yaml:"chrome_path,omitempty"`        // installed Chrome binary; "" = auto-discover
	ProfileDir       string `yaml:"profile_dir,omitempty"`        // persistent user-data-dir (login-once)
	AllowPrivateURLs bool   `yaml:"allow_private_urls,omitempty"` // default false = block localhost/RFC1918
	SessionID        string `yaml:"-"`                            // runtime only
}

func (c *Config) GetPrompt() string {
	tmpl, err := template.New("").Funcs(sprig.FuncMap()).Parse(c.Prompt)
	if err != nil {
		return ""
	}

	data := bytes.NewBuffer([]byte{})

	currentDirectory, err := os.Getwd()
	if err != nil {
		currentDirectory = ""
	}
	currentUser, err := user.Current()
	if err != nil {
		currentUser = &user.User{}
	}

	if err := tmpl.Execute(data, struct {
		Config           Config
		CurrentDirectory string
		CurrentUser      string
	}{
		Config:           *c,
		CurrentDirectory: currentDirectory,
		CurrentUser:      currentUser.Username,
	}); err != nil {
		return ""
	}

	var b strings.Builder
	b.WriteString(data.String())

	if len(c.Skills) > 0 {
		b.WriteString("\n\nAvailable skills — call the load_skill tool with the skill name to read its full instructions before acting on a matching task:\n")
		for _, s := range c.Skills {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		}
	}

	// Tool-usage guidance goes to every session, including ones whose config
	// replaced the default prompt. See toolGuidance for why it lives here
	// rather than in config.defaultPrompt, and why it is rendered against
	// BuiltinTools rather than naming tools this session may not expose.
	b.WriteString("\n\n")
	b.WriteString(toolGuidance(c.BuiltinTools))

	for _, f := range c.PromptFragments {
		if strings.TrimSpace(f) == "" {
			continue
		}
		b.WriteString("\n\n")
		b.WriteString(f)
	}

	if found := detectContextFiles(currentDirectory); len(found) > 0 {
		b.WriteString("\n\nThe following project instruction file(s) were found in the working directory: ")
		b.WriteString(strings.Join(found, ", "))
		b.WriteString(".\nRead ")
		if len(found) == 1 {
			b.WriteString("it")
		} else {
			b.WriteString("them")
		}
		b.WriteString(" before acting on this repository and follow the instructions ")
		if len(found) == 1 {
			b.WriteString("it contains")
		} else {
			b.WriteString("they contain")
		}
		b.WriteString(".")
	}

	// Everything in this paragraph is advice the model will relay in its own
	// words, so the program name goes into ALL of it, the closing sentence
	// included, rather than only into the backticked commands. Terminal output
	// can get away with a half-renamed sentence because the surrounding branding
	// tells the reader that "nib" and the command they typed are the same thing;
	// a system prompt has no such context, and a model told "the next nib
	// session" will tell an embedder's user to restart something they have never
	// heard of.
	//
	// Appended AFTER the template above is executed, so the name is inert text:
	// it cannot break the template, and template syntax inside it is carried
	// through verbatim rather than evaluated.
	prog := ProgramNameOr(c.ProgramName)
	b.WriteString("\n\nYou can register additional MCP servers from the command line: ")
	fmt.Fprintf(&b, "`%s mcp add <name> -- <command> [args...]` for a local server, or ", prog)
	fmt.Fprintf(&b, "`%s mcp add <name> --url <url> [--transport http|sse]` for a remote one; ", prog)
	fmt.Fprintf(&b, "`%s mcp list` and `%s mcp test <name>` show and verify them. ", prog, prog)
	fmt.Fprintf(&b, "Servers added this way become available on the next %s session.", prog)

	return b.String()
}

// contextFileNames lists the project instruction files nib looks for in the
// working directory. Their presence is surfaced in the system prompt so the
// agent reads them before acting on the repository.
var contextFileNames = []string{"AGENTS.md", "CLAUDE.md", "NIB.md", "GEMINI.md"}

// detectContextFiles returns the names of known project instruction files that
// exist as regular files in dir, preserving contextFileNames order.
func detectContextFiles(dir string) []string {
	if dir == "" {
		return nil
	}
	var found []string
	for _, name := range contextFileNames {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
			found = append(found, name)
		}
	}
	return found
}

type MCPServer struct {
	Command   string            `yaml:"command,omitempty"`
	Args      []string          `yaml:"args,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	URL       string            `yaml:"url,omitempty"`       // remote: presence selects an HTTP/SSE transport
	Transport string            `yaml:"transport,omitempty"` // remote transport: "http" (default) or "sse"

	BearerToken string            `yaml:"token,omitempty"`   // remote only: sent as "Authorization: Bearer <token>"
	Headers     map[string]string `yaml:"headers,omitempty"` // remote only: custom HTTP headers

	// Disabled, when true, keeps the server in config (so the UI can list and
	// re-enable it) but excludes it from EffectiveConfig, so it starts no
	// transport. Absent (omitempty) reads as enabled — no migration needed.
	Disabled bool `yaml:"disabled,omitempty"`
}
