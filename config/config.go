package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"dario.cat/mergo"
	"github.com/mudler/nib/internal"
	"github.com/mudler/nib/plugin"
	"github.com/mudler/nib/skill"
	"github.com/mudler/nib/types"

	"gopkg.in/yaml.v3"
)

const defaultPrompt = `
You are a Operative System terminal assistant that helps the user into automatizing common tasks, and can also do perform coding tasks.
You will use the tools at your disposal to fullfill the user request, and, for instance run bash scripts to execute and automate things.

For self-contained subtasks (exploring a codebase, researching, drafting a plan) you can delegate to a sub-agent by calling the spawn_agent tool with an appropriate agent_type. Use background=true to keep working while it runs.
{{- if .Config.Agents }}
Available sub-agent types:
{{- range .Config.Agents }}
- {{ .Name }}: {{ .Description }}
{{- end }}
{{- end }}

Current directory: {{.CurrentDirectory}}
Current user: {{.CurrentUser}}
`

// configPaths returns the list of config file paths to try, in order of priority
func configPaths() []string {
	var paths []string

	// current directory, .nib.yaml (legacy .wiz.yaml as fallback)
	cwd, err := os.Getwd()
	if err == nil {
		paths = append(paths, filepath.Join(cwd, ".nib.yaml"))
		paths = append(paths, filepath.Join(cwd, ".wiz.yaml"))
	}

	// First priority: XDG config directory (legacy wiz dir as fallback)
	if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
		paths = append(paths, filepath.Join(xdgConfig, "nib", "config.yaml"))
		paths = append(paths, filepath.Join(xdgConfig, "wiz", "config.yaml"))
	}

	// Second priority: ~/.config/nib/config.yaml (legacy wiz dir as fallback)
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "nib", "config.yaml"))
		paths = append(paths, filepath.Join(home, ".config", "wiz", "config.yaml"))
		// Third priority: ~/.nib.yaml (legacy ~/.wiz.yaml as fallback)
		paths = append(paths, filepath.Join(home, ".nib.yaml"))
		paths = append(paths, filepath.Join(home, ".wiz.yaml"))
	}

	paths = append(paths, filepath.Join("/etc", "nib", "config.yaml"))
	paths = append(paths, filepath.Join("/etc", "wiz", "config.yaml"))

	return paths
}

// configPathsIn returns the config paths to try. A non-empty root collapses
// the search to that single directory, so an embedder gets exactly the file it
// asked for and never picks up a stray ~/.config/nib or /etc/nib.
func configPathsIn(root string) []string {
	if root != "" {
		return []string{filepath.Join(root, "config.yaml")}
	}
	return configPaths()
}

// WritablePath returns the config file self-configuration should write to: the
// first existing config path (so additions are visible to Load), else the
// preferred default ~/.config/nib/config.yaml.
func WritablePath() string {
	for _, p := range configPaths() {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "nib", "config.yaml")
	}
	return ".nib.yaml"
}

// WritablePathIn returns the config file to write to for the given root.
func WritablePathIn(root string) string {
	if root != "" {
		return filepath.Join(root, "config.yaml")
	}
	return WritablePath()
}

// BaseDirOf resolves the plugins/skills root that a loaded config points at.
//
// types.Config.BaseDir holds the RAW OVERRIDE, empty for standalone nib, so it
// is not a directory and must never be joined onto a path: filepath.Join("",
// "plugins") silently yields a relative path under the process working
// directory. Use this helper wherever a resolved directory is wanted. Handing
// the raw field to something that documents its parameter as an override
// (manage.NewIn, setup.Save) is the other correct use and needs no wrapping.
//
// This cannot be a method on types.Config: plugin already imports types, so
// types importing plugin would be an import cycle.
func BaseDirOf(cfg types.Config) string { return plugin.BaseDirIn(cfg.BaseDir) }

// loadFromFileIn attempts to load config from the first existing config file
// under root (or, for an empty root, the default search path).
func loadFromFileIn(root string) types.Config {
	var cfg types.Config

	for _, path := range configPathsIn(root) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		if err := yaml.Unmarshal(data, &cfg); err != nil {
			continue
		}

		// Found and parsed a config file
		break
	}

	return cfg
}

// LoadOptions configures LoadWith. The zero value is what standalone nib uses.
type LoadOptions struct {
	// BaseDir overrides the config/plugins/skills root. Empty means the
	// default XDG resolution.
	BaseDir string
	// Defaults seed fields the config file leaves empty. They sit beneath the
	// file so a user edit always wins. Every field of types.Config is seedable,
	// not a hand-picked subset, with ONE carve-out: Defaults.BaseDir is ignored.
	// The root has exactly one knob, LoadOptions.BaseDir above, and a seeded
	// BaseDir is overwritten by it (including when it is empty, which is what
	// selects standalone nib's XDG resolution). See applySeeds for exactly what
	// "empty" means for maps, slices and booleans.
	Defaults types.Config
	// Overrides are Defaults' mirror image: they sit ABOVE the config file and
	// above the environment block, so whatever an embedder sets here is the
	// final word for that field. This is the channel for a value the embedder
	// resolved on the user's behalf and that a config file must not silently
	// undo, a CLI flag above all: `--endpoint` seeded through Defaults is
	// accepted and then ignored the moment the file carries a base_url, which
	// is the default state once anything has written one.
	//
	// The same reflective merge as Defaults, so every field of types.Config is
	// overridable, with the SAME single carve-out: Overrides.BaseDir is ignored,
	// because LoadOptions.BaseDir remains the one knob for the root.
	//
	// The zero-value merge cuts the other way here, and it is the one thing an
	// embedder has to plan around. A zero value is indistinguishable from
	// "unset", so an override only ever raises a field: an override of "" cannot
	// blank a model the file sets, an override of 0 cannot zero an iteration
	// count, and an override of FALSE CANNOT BEAT A `true:` IN THE FILE. There
	// is no exception, Browser.AllowPrivateURLs included, so an embedder that
	// must force a bool off cannot do it through this channel; it has to own the
	// file, or point BaseDir at a root it controls. Presence tracking would need
	// a decoder that records which keys were written, which is not what a
	// types.Config-typed field can express.
	Overrides types.Config
	// SkipBareEnv suppresses the bare MODEL / API_KEY / BASE_URL environment
	// variables. Embedders that expose their own prefixed variables set this so
	// a MODEL meant for some other tool cannot retarget the agent.
	SkipBareEnv bool
}

// Load loads the configuration from YAML file and environment variables.
// Environment variables take precedence over YAML config.
func Load() types.Config { return LoadWith(LoadOptions{}) }

// LoadWith is Load with injectable roots and behavior switches.
func LoadWith(o LoadOptions) types.Config {
	// Load from YAML file first
	cfg := loadFromFileIn(o.BaseDir)

	// Seed the gaps the file left. This runs before the env block and before
	// withDefaults, which is what makes the precedence read, lowest to highest:
	// nib's built-in defaults, the embedder's seeds, the config file, the
	// environment, the embedder's overrides.
	applySeeds(&cfg, o.Defaults)

	// Override with environment variables if set. An embedder that publishes its
	// own prefixed variables suppresses these, so a bare MODEL exported for some
	// unrelated tool cannot silently retarget the agent.
	if !o.SkipBareEnv {
		if model := os.Getenv("MODEL"); model != "" {
			cfg.Model = model
		}
		if apiKey := os.Getenv("API_KEY"); apiKey != "" {
			cfg.APIKey = apiKey
		}
		if baseURL := os.Getenv("BASE_URL"); baseURL != "" {
			cfg.BaseURL = baseURL
		}
	}

	// Last word: the embedder's overrides go on top of the file and of the block
	// above, which is what makes a host's CLI flag a flag rather than a wish.
	// Anything left unset here falls through to whatever the rungs below
	// resolved, so a zero Overrides is exactly today's load.
	applyOverrides(&cfg, o.Overrides)

	cfg = withDefaults(cfg)

	// Carry the override (not the resolved root) so consumers keep resolving it
	// through plugin.BaseDirIn / config.WritablePathIn: with no override those
	// two reproduce standalone nib exactly, including WritablePath's
	// first-existing-file search, which <base>/config.yaml would not.
	//
	// Unconditional, so it also enforces the one carve-out in Defaults: the root
	// has a single knob and a seeded BaseDir loses to it, empty included. Two
	// ways to set one thing would be worse than one, and the file this very load
	// came from was already chosen by o.BaseDir, so honoring a seed here would
	// leave cfg pointing at a root it was not read from.
	cfg.BaseDir = o.BaseDir
	root := BaseDirOf(cfg)

	// Merge enabled skill-pack skills before plugins, so precedence is
	// built-in defaults < plugins < skill-packs < user. plugin.Apply skips any
	// skill name already present (user + packs), so packs win over plugins.
	if err := skill.Apply(&cfg, root); err != nil {
		fmt.Fprintf(os.Stderr, "nib: skill load: %v\n", err)
	}

	// Merge enabled plugin contributions (mcp servers + agents) before the
	// agent default-merge, so precedence is built-in defaults < plugins < user.
	if err := plugin.Apply(&cfg, root, internal.Version); err != nil {
		fmt.Fprintf(os.Stderr, "nib: plugin load: %v\n", err)
	}

	// Merge user-provided agent types with the built-in defaults.
	cfg.Agents = MergeAgentTypes(cfg.Agents)

	return cfg
}

// applySeeds fills every zero-valued field of cfg from the embedder's seeds.
//
// It is reflective rather than field-by-field on purpose: LoadOptions.Defaults
// is typed types.Config, so the type promises a merge, and a hand-written
// whitelist would silently stop honoring each field added to types.Config
// afterwards. Seeding LogLevel is the sharp end of that: the seed has to land
// here or app's `if cfg.LogLevel == ""` forces "error" over it, with no
// compile error and no warning to say the seed was dropped.
//
// What "empty" means, since a zero-value merge cannot ask the YAML decoder
// which keys the file actually contained:
//
//   - Scalars and structs: recursive, per leaf field. A file that sets
//     agent_options.iterations but not max_attempts keeps its iterations and
//     takes the seeded max_attempts.
//   - Maps (Metadata, MCPServers, MCPServer.Env, ...): merged per key. A key
//     the file sets wins whole; a key only the seeds have is added. The
//     TOP-LEVEL map is freshly allocated, so cfg.Metadata and cfg.MCPServers
//     never alias the caller's. That is one level deep only: a value stored
//     under a key keeps whatever containers the caller built, so
//     cfg.MCPServers[k].Env, .Args and cfg.Agents[i].Metadata DO alias the
//     seeds. Nothing in nib mutates those in place, which is why this has been
//     safe, but an embedder that reuses a seed struct across two loads is
//     sharing them and should not write through either.
//   - Slices (Agents, Skills, Hooks, Commands, AllowedTools, ...): all or
//     nothing. A file that lists any entry keeps exactly its own list; the
//     seeded list is used only when the file has none. Element-wise merging
//     would be meaningless for order-dependent lists of named things.
//   - Booleans: false is indistinguishable from unset, so for EVERY bool field
//     a seeded true beats an explicit `false:` in the file. There is no
//     exception: Compaction.Disabled, AgentOptions.ForceReasoning,
//     Computer.Enabled, Browser.Enabled and Browser.AllowPrivateURLs all behave
//     this way, as does any bool added later. AllowPrivateURLs is the
//     consequential one, since it gates the browser tools' access to localhost
//     and RFC1918 addresses: seeding it true cannot be walked back by a config
//     file, only by not seeding it. An embedder should seed a bool only when it
//     genuinely means "on regardless of what the user wrote".
//
// Slice-valued seeds are cloned before the merge. mergo adopts an empty
// destination slice by reference, and skill.Apply/plugin.Apply then append to
// cfg.Hooks, cfg.Skills and cfg.Commands, which would write into the caller's
// backing array whenever it has spare capacity.
func applySeeds(cfg *types.Config, defaults types.Config) {
	if err := mergo.Merge(cfg, cloneSliceFields(defaults)); err != nil {
		// Only reachable for a nil or non-struct argument, neither of which the
		// single call site can produce. Report rather than drop it silently.
		fmt.Fprintf(os.Stderr, "nib: config defaults: %v\n", err)
	}
}

// applyOverrides is applySeeds in reverse: every non-zero field of the
// embedder's overrides replaces what the file and the environment resolved.
//
// It reuses the same reflective merge for the same reason, so the two channels
// cover exactly the same fields and neither can quietly fall behind a field
// added to types.Config later. TestOverrideEveryFieldOfConfig is the guard that
// says so, and it is the mirror of TestDefaultsSeedEveryFieldOfConfig.
//
// The container rules carry over unchanged in shape, only reversed in who wins:
//
//   - Scalars and structs: recursive, per leaf field. An override of
//     agent_options.iterations leaves the file's max_attempts alone.
//   - Maps: merged per key. An overridden key wins, a key only the file has
//     survives. The TOP-LEVEL map is nib's own either way, never the caller's,
//     with the same one-level-deep caveat applySeeds documents: the containers
//     inside a map VALUE (MCPServers[k].Env, .Args, Agents[i].Metadata) still
//     alias the overrides. Inherited from the seed path rather than introduced
//     here, and safe only because nothing in nib writes to them in place.
//   - Slices: all or nothing. A non-empty override replaces the file's list
//     whole; an empty one leaves it. Cloned first, because mergo adopts the
//     SOURCE slice by reference under WithOverride and skill.Apply /
//     plugin.Apply then append to cfg.Hooks, cfg.Skills and cfg.Commands, which
//     would write into the embedder's backing array.
//   - Booleans and every other zero value: skipped. This is the asymmetry an
//     embedder has to plan around. Under Defaults a seeded true beats a file's
//     `false:`; here an overridden false CANNOT beat a file's `true:`, because
//     nothing in a types.Config-typed field distinguishes "set to the zero
//     value" from "not set". So an override can only ever raise a field: it
//     cannot blank a model, zero an iteration count, or switch
//     Browser.AllowPrivateURLs back off. An embedder that must force one of
//     those has to own the config file rather than reach over it.
//
// The BaseDir carve-out needs no code here: LoadWith assigns cfg.BaseDir from
// LoadOptions.BaseDir after this runs, unconditionally, which is what keeps the
// root to a single knob for both channels.
func applyOverrides(cfg *types.Config, overrides types.Config) {
	if err := mergo.Merge(cfg, cloneSliceFields(overrides), mergo.WithOverride); err != nil {
		// Only reachable for a nil or non-struct argument, neither of which the
		// single call site can produce. Report rather than drop it silently.
		fmt.Fprintf(os.Stderr, "nib: config overrides: %v\n", err)
	}
}

// cloneSliceFields returns c with each of its top-level slice fields replaced
// by a copy, so nothing downstream can reach the caller's backing arrays. It
// walks the struct reflectively so a slice field added to types.Config later is
// covered without an edit here.
func cloneSliceFields(c types.Config) types.Config {
	v := reflect.ValueOf(&c).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		if f.Kind() != reflect.Slice || f.IsNil() || !f.CanSet() {
			continue
		}
		clone := reflect.MakeSlice(f.Type(), f.Len(), f.Len())
		reflect.Copy(clone, f)
		f.Set(clone)
	}
	return c
}

// withDefaults fills in zero-valued config fields with their defaults. It is
// pure (no file/env access) so it can be unit-tested directly.
func withDefaults(cfg types.Config) types.Config {
	if cfg.Prompt == "" {
		cfg.Prompt = defaultPrompt
	}
	if cfg.AgentOptions.Iterations == 0 {
		cfg.AgentOptions.Iterations = 10
	}
	if cfg.AgentOptions.MaxAttempts == 0 {
		cfg.AgentOptions.MaxAttempts = 3
	}
	if cfg.AgentOptions.MaxRetries == 0 {
		cfg.AgentOptions.MaxRetries = 3
	}
	// ForceReasoning defaults to false (zero value), which is intentional;
	// users must explicitly enable it in config.

	// Compaction defaults (auto-compaction is ON unless Disabled).
	if cfg.Compaction.MaxContextTokens == 0 {
		cfg.Compaction.MaxContextTokens = 128000
	}
	if cfg.Compaction.Threshold == 0 {
		cfg.Compaction.Threshold = 0.8
	}
	if cfg.Compaction.KeepRecent == 0 {
		cfg.Compaction.KeepRecent = 8
	}
	// Must match chat.ContextBudget's defaultReserveTokens, which applies the
	// same fallback for embedders that call chat.NewSession without ever
	// passing through here.
	if cfg.Compaction.ReserveTokens == 0 {
		cfg.Compaction.ReserveTokens = 4096
	}

	// Tool-output pruning is defaulted as a block, not field by field: a zero
	// HighWaterTokens is meaningful on its own (it disables size pruning while
	// leaving the stale-read rule on), so it can only be treated as "unset" when
	// the whole block is absent. Field-by-field defaulting would make that
	// documented setting unexpressible.
	if cfg.ToolOutputPruning == (types.ToolOutputPruningConfig{}) {
		cfg.ToolOutputPruning = types.ToolOutputPruningConfig{
			HighWaterTokens: 24000,
			LowWaterTokens:  8000,
			MinResultTokens: 200,
		}
	}
	return cfg
}
