package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mudler/nib/types"
)

func TestWithDefaultsCompaction(t *testing.T) {
	cfg := withDefaults(types.Config{})
	if cfg.Compaction.MaxContextTokens != 128000 {
		t.Fatalf("MaxContextTokens default = %d, want 128000", cfg.Compaction.MaxContextTokens)
	}
	if cfg.Compaction.Threshold != 0.8 {
		t.Fatalf("Threshold default = %v, want 0.8", cfg.Compaction.Threshold)
	}
	if cfg.Compaction.KeepRecent != 8 {
		t.Fatalf("KeepRecent default = %d, want 8", cfg.Compaction.KeepRecent)
	}
	if cfg.Compaction.Disabled {
		t.Fatal("auto-compaction should be enabled (Disabled=false) by default")
	}
}

func TestWithDefaultsKeepsUserValues(t *testing.T) {
	// Every field is set to a non-zero value on purpose: withDefaults reads
	// "unset" as zero, so a field left out here would be defaulted and the
	// comparison below would fail for a reason that is not an override bug.
	in := types.Config{Compaction: types.CompactionConfig{
		MaxContextTokens: 200000, Threshold: 0.5, KeepRecent: 2, Disabled: true,
		ReserveTokens: 512,
	}}
	got := withDefaults(in)
	if got.Compaction != in.Compaction {
		t.Fatalf("withDefaults overrode user values: %+v", got.Compaction)
	}
}

// clearBareEnv neutralizes the MODEL/API_KEY/BASE_URL overrides LoadWith reads
// from the process environment. Without it these tests assert against whatever
// the developer happens to export: `MODEL=oops go test ./config/` fails them.
func clearBareEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MODEL", "")
	t.Setenv("API_KEY", "")
	t.Setenv("BASE_URL", "")
}

func TestLoadWithBaseDirReadsInjectedRoot(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("model: injected-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := LoadWith(LoadOptions{BaseDir: dir})
	if cfg.Model != "injected-model" {
		t.Fatalf("Model = %q, want injected-model", cfg.Model)
	}
	if cfg.BaseDir != dir {
		t.Fatalf("BaseDir = %q, want %q", cfg.BaseDir, dir)
	}
}

func TestLoadWithEmptyBaseDirUsesDefaultPaths(t *testing.T) {
	clearBareEnv(t)
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "nib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "nib", "config.yaml"), []byte("model: xdg-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := LoadWith(LoadOptions{})
	if cfg.Model != "xdg-model" {
		t.Fatalf("Model = %q, want xdg-model", cfg.Model)
	}
}

// The standalone binary must keep writing self-configuration to the first
// existing config file (WritablePath), not to <base>/config.yaml. Load
// therefore leaves BaseDir empty when no root was injected, so every consumer
// that resolves it through WritablePathIn/BaseDirIn gets today's behavior.
func TestLoadWithEmptyBaseDirKeepsWritablePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := os.WriteFile(filepath.Join(home, ".nib.yaml"), []byte("model: home-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := LoadWith(LoadOptions{})
	if got, want := WritablePathIn(cfg.BaseDir), filepath.Join(home, ".nib.yaml"); got != want {
		t.Fatalf("WritablePathIn(cfg.BaseDir) = %q, want %q", got, want)
	}
}

// BaseDirOf must return the injected root itself, not a path derived from it,
// and must not consult the XDG/home resolution at all when an override is set.
func TestBaseDirOfReturnsInjectedRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	dir := t.TempDir()
	if got := BaseDirOf(types.Config{BaseDir: dir}); got != dir {
		t.Fatalf("BaseDirOf = %q, want %q", got, dir)
	}
}

// With no override the field is empty, and BaseDirOf must reproduce standalone
// nib's default resolution rather than returning the empty string, which would
// make every filepath.Join on it relative to the working directory.
func TestBaseDirOfEmptyResolvesDefaultRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	want := filepath.Join(home, "nib")
	if got := BaseDirOf(types.Config{}); got != want {
		t.Fatalf("BaseDirOf = %q, want %q", got, want)
	}
}

// SkipBareEnv is what lets an embedder publish its own prefixed variables: a
// bare MODEL exported for some unrelated tool must not retarget the agent.
func TestSkipBareEnvIgnoresBareVars(t *testing.T) {
	t.Setenv("MODEL", "from-env")
	t.Setenv("API_KEY", "key-from-env")
	t.Setenv("BASE_URL", "http://env.invalid/v1")
	dir := t.TempDir()

	cfg := LoadWith(LoadOptions{BaseDir: dir, SkipBareEnv: true})
	if cfg.Model == "from-env" {
		t.Fatal("SkipBareEnv did not suppress MODEL")
	}
	if cfg.APIKey == "key-from-env" {
		t.Fatal("SkipBareEnv did not suppress API_KEY")
	}
	if cfg.BaseURL == "http://env.invalid/v1" {
		t.Fatal("SkipBareEnv did not suppress BASE_URL")
	}
}

// The zero value has to reproduce standalone nib exactly, so with the switch
// off all three bare variables still apply.
func TestBareEnvStillAppliesByDefault(t *testing.T) {
	t.Setenv("MODEL", "from-env")
	t.Setenv("API_KEY", "key-from-env")
	t.Setenv("BASE_URL", "http://env.invalid/v1")
	dir := t.TempDir()

	cfg := LoadWith(LoadOptions{BaseDir: dir})
	if cfg.Model != "from-env" {
		t.Fatalf("Model = %q, want from-env (standalone nib must be unaffected)", cfg.Model)
	}
	if cfg.APIKey != "key-from-env" {
		t.Fatalf("APIKey = %q, want key-from-env", cfg.APIKey)
	}
	if cfg.BaseURL != "http://env.invalid/v1" {
		t.Fatalf("BaseURL = %q, want the env value", cfg.BaseURL)
	}
}

// Seeds fill the gaps the config file leaves; they never override it.
func TestDefaultsSitBeneathTheConfigFile(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("model: from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := types.Config{Model: "from-defaults", BaseURL: "http://seed.invalid/v1"}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	if cfg.Model != "from-file" {
		t.Fatalf("Model = %q, want from-file (the file must win over Defaults)", cfg.Model)
	}
	if cfg.BaseURL != "http://seed.invalid/v1" {
		t.Fatalf("BaseURL = %q, want the seeded value to fill the gap", cfg.BaseURL)
	}
}

// The whole ladder in one place: built-in defaults < Defaults < config file <
// environment. Each field below is set at exactly one rung above the seeds, so
// a collapsed precedence shows up as a concrete wrong value rather than as a
// missing assertion.
func TestPrecedenceDefaultsThenFileThenEnv(t *testing.T) {
	t.Setenv("MODEL", "from-env")
	t.Setenv("API_KEY", "")
	t.Setenv("BASE_URL", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("model: from-file\napi_key: key-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := types.Config{
		Model:        "model-from-defaults",
		APIKey:       "key-from-defaults",
		BaseURL:      "http://seed.invalid/v1",
		ApprovalMode: "auto",
		Prompt:       "seeded prompt",
		LogLevel:     "debug",
		AgentOptions: types.AgentOptions{Iterations: 42},
		Compaction:   types.CompactionConfig{MaxContextTokens: 4096},
	}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults})

	// env beats file beats seeds
	if cfg.Model != "from-env" {
		t.Fatalf("Model = %q, want from-env", cfg.Model)
	}
	// file beats seeds, and the env var is unset so it does not intervene
	if cfg.APIKey != "key-from-file" {
		t.Fatalf("APIKey = %q, want key-from-file", cfg.APIKey)
	}
	// nothing above the seeds sets these, so the seeds stand
	if cfg.BaseURL != "http://seed.invalid/v1" {
		t.Fatalf("BaseURL = %q, want the seeded value", cfg.BaseURL)
	}
	if cfg.ApprovalMode != "auto" {
		t.Fatalf("ApprovalMode = %q, want auto (seeded)", cfg.ApprovalMode)
	}
	// The seeds land before withDefaults, so a seeded Prompt outranks nib's
	// built-in one. That ordering is the "built-in defaults < Defaults" rung.
	if cfg.Prompt != "seeded prompt" {
		t.Fatalf("Prompt = %q, want the seeded prompt to beat nib's built-in default", cfg.Prompt)
	}
	// LogLevel is the field the old whitelist dropped on the floor: app falls
	// back to "error" whenever it arrives empty, so a seed that does not land
	// here is silently discarded.
	if cfg.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want debug (seeded)", cfg.LogLevel)
	}
	// Nested struct fields merge per leaf: the file set neither, so both seeds
	// stand and beat withDefaults' built-in 10 / 128000.
	if cfg.AgentOptions.Iterations != 42 {
		t.Fatalf("AgentOptions.Iterations = %d, want 42 (seeded)", cfg.AgentOptions.Iterations)
	}
	if cfg.Compaction.MaxContextTokens != 4096 {
		t.Fatalf("Compaction.MaxContextTokens = %d, want 4096 (seeded)", cfg.Compaction.MaxContextTokens)
	}
	// Untouched by the seeds, so withDefaults still supplies the built-in.
	if cfg.Compaction.KeepRecent != 8 {
		t.Fatalf("Compaction.KeepRecent = %d, want nib's built-in 8", cfg.Compaction.KeepRecent)
	}
}

// The file wins per leaf, not per struct: setting one nested field must not
// discard the seeds for its siblings.
func TestDefaultsMergeNestedStructsPerField(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("agent_options:\n  iterations: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := types.Config{AgentOptions: types.AgentOptions{Iterations: 42, MaxAttempts: 9}}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	if cfg.AgentOptions.Iterations != 3 {
		t.Fatalf("Iterations = %d, want 3 from the file", cfg.AgentOptions.Iterations)
	}
	if cfg.AgentOptions.MaxAttempts != 9 {
		t.Fatalf("MaxAttempts = %d, want the seeded 9 to survive its sibling", cfg.AgentOptions.MaxAttempts)
	}
}

// Maps merge per key: a key the file sets wins whole, a key only the seeds have
// is added. This is where merge libraries usually surprise people, so it is
// pinned rather than assumed.
func TestDefaultsMergeMetadataPerKey(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("metadata:\n  enable_thinking: \"false\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := types.Config{Metadata: map[string]string{
		"enable_thinking": "true", // the file must win this one
		"tenant":          "localai",
	}}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	if got := cfg.Metadata["enable_thinking"]; got != "false" {
		t.Fatalf("Metadata[enable_thinking] = %q, want false from the file", got)
	}
	if got := cfg.Metadata["tenant"]; got != "localai" {
		t.Fatalf("Metadata[tenant] = %q, want the seeded value to fill the gap", got)
	}
}

// The merged map must be nib's own, never the caller's: mutating cfg (plugins
// add mcp_servers entries) must not reach back into the embedder's struct.
func TestDefaultsMapSeedsAreNotAliased(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	defaults := types.Config{Metadata: map[string]string{"tenant": "localai"}}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	if cfg.Metadata == nil {
		t.Fatal("Metadata was not seeded at all")
	}
	cfg.Metadata["injected"] = "yes"
	if _, ok := defaults.Metadata["injected"]; ok {
		t.Fatal("cfg.Metadata aliases the caller's map")
	}
}

// MCP servers merge per server name, and a named server the file defines wins
// as a whole value rather than being field-merged with the seed.
func TestDefaultsMergeMCPServersPerKey(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("mcp_servers:\n  shared:\n    command: from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := types.Config{MCPServers: map[string]types.MCPServer{
		"shared":    {Command: "from-seed", Args: []string{"--seeded"}},
		"seed-only": {Command: "seeded-server"},
	}}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	if got := cfg.MCPServers["shared"].Command; got != "from-file" {
		t.Fatalf("MCPServers[shared].Command = %q, want from-file", got)
	}
	if got := cfg.MCPServers["shared"].Args; len(got) != 0 {
		t.Fatalf("MCPServers[shared].Args = %v, want the file's entry to win whole", got)
	}
	if got := cfg.MCPServers["seed-only"].Command; got != "seeded-server" {
		t.Fatalf("MCPServers[seed-only].Command = %q, want the seeded server to be added", got)
	}
}

// Slices are all or nothing. Merging them element-wise would be meaningless for
// order-dependent lists of named things, so a file that lists any entry keeps
// exactly its own list.
func TestDefaultsSeedSlicesAllOrNothing(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("allowed_tools:\n  - from_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := types.Config{
		AllowedTools:     []string{"seed_a", "seed_b"},
		ReadOnlyCommands: []string{"terraform plan"},
	}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	if len(cfg.AllowedTools) != 1 || cfg.AllowedTools[0] != "from_file" {
		t.Fatalf("AllowedTools = %v, want exactly the file's list", cfg.AllowedTools)
	}
	if len(cfg.ReadOnlyCommands) != 1 || cfg.ReadOnlyCommands[0] != "terraform plan" {
		t.Fatalf("ReadOnlyCommands = %v, want the seeded list to fill the gap", cfg.ReadOnlyCommands)
	}
}

// An adopted slice must be a copy. skill.Apply and plugin.Apply append to
// cfg.Hooks / cfg.Skills / cfg.Commands, and appending into spare capacity in
// the caller's backing array would corrupt the embedder's own value.
func TestDefaultsSliceSeedsAreNotAliased(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	backing := make([]types.HookConfig, 1, 8) // spare capacity: the dangerous shape
	backing[0] = types.HookConfig{Event: "PreToolUse", Command: "seeded"}
	defaults := types.Config{Hooks: backing}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	if len(cfg.Hooks) != 1 || cfg.Hooks[0].Command != "seeded" {
		t.Fatalf("Hooks = %v, want the seeded hook", cfg.Hooks)
	}
	cfg.Hooks = append(cfg.Hooks, types.HookConfig{Event: "PostToolUse", Command: "later"})
	cfg.Hooks[0].Command = "mutated"
	if backing[0].Command != "seeded" {
		t.Fatalf("cfg.Hooks aliases the caller's slice: backing[0] = %+v", backing[0])
	}
	if got := backing[:2][1].Command; got != "" {
		t.Fatalf("append wrote into the caller's spare capacity: %q", got)
	}
}

// seedCarveOuts names the fields the embedder's two config channels,
// LoadOptions.Defaults and LoadOptions.Overrides, deliberately do not carry.
// There is exactly one, it is the same one for both, and it should stay that
// way; the reasoning and the enforcement live in TestDefaultsBaseDirIsNotSeedable
// and TestOverridesBaseDirIsNotOverridable.
var seedCarveOuts = map[string]string{
	"BaseDir": "the root has a single knob, LoadOptions.BaseDir",
}

// The rot guard the whitelist needed and the reflective merge should not: fill
// EVERY field of types.Config with a non-zero value, then load twice against
// the same empty root, once with those seeds and once with none, and require
// every field to come back DIFFERENT. A field the merge cannot carry shows up
// here by name rather than as a silent no-op at some embedder's call site.
//
// The comparison is differential rather than a zero-check because a zero-check
// is blind to anything LoadWith writes after applySeeds: withDefaults filling a
// gap, MergeAgentTypes returning the built-ins, or the BaseDir assignment all
// leave a field non-zero whether or not the seed landed. Those writes happen in
// the unseeded load too, so differencing cancels them out, and a NEW one added
// later is caught with no list to keep in sync.
func TestDefaultsSeedEveryFieldOfConfig(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()

	var defaults types.Config
	fillNonZero(t, reflect.ValueOf(&defaults).Elem())

	seeded := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	unseeded := LoadWith(LoadOptions{BaseDir: dir, SkipBareEnv: true})

	s, u := reflect.ValueOf(seeded), reflect.ValueOf(unseeded)
	for i := range s.NumField() {
		name := s.Type().Field(i).Name
		differs := !reflect.DeepEqual(s.Field(i).Interface(), u.Field(i).Interface())
		if why, carved := seedCarveOuts[name]; carved {
			if differs {
				t.Errorf("types.Config.%s responded to a seed, but it is a carve-out (%s)", name, why)
			}
			continue
		}
		if !differs {
			t.Errorf("types.Config.%s is identical seeded and unseeded (%v): the seed never landed",
				name, s.Field(i).Interface())
		}
	}
}

// The one carve-out. LoadOptions.BaseDir is the single knob for the root, so a
// seeded Defaults.BaseDir must lose to it, and must lose to an EMPTY one too:
// empty is not "unset", it is the explicit request for standalone nib's XDG
// resolution. Honoring the seed would also leave cfg claiming a root the config
// file was not read from, since loadFromFileIn already used LoadOptions.BaseDir.
func TestDefaultsBaseDirIsNotSeedable(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	defaults := types.Config{BaseDir: "/seeded/root"}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	if cfg.BaseDir != dir {
		t.Fatalf("BaseDir = %q, want the LoadOptions root %q", cfg.BaseDir, dir)
	}

	// Empty LoadOptions.BaseDir still wins, and BaseDirOf must resolve to the
	// XDG root rather than to anything the seed named.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	cfg = LoadWith(LoadOptions{Defaults: defaults, SkipBareEnv: true})
	if cfg.BaseDir != "" {
		t.Fatalf("BaseDir = %q, want empty: a seed must not supply the root", cfg.BaseDir)
	}
	if got, want := BaseDirOf(cfg), filepath.Join(xdg, "nib"); got != want {
		t.Fatalf("BaseDirOf = %q, want the XDG root %q", got, want)
	}
}

// Agents was one of the fields the whitelist dropped. The rot guard only proves
// the seeded list changed the result; assert the seeded agent by name, and that
// it joined the built-in types rather than displacing them.
func TestDefaultsSeedAgents(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	defaults := types.Config{Agents: []types.AgentTypeConfig{
		{Name: "localai-seeded", Description: "seeded by the embedder"},
	}}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	var found *types.AgentTypeConfig
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "localai-seeded" {
			found = &cfg.Agents[i]
		}
	}
	if found == nil {
		t.Fatalf("seeded agent missing from %d merged agents", len(cfg.Agents))
	}
	if found.Description != "seeded by the embedder" {
		t.Fatalf("seeded agent Description = %q", found.Description)
	}
	if len(cfg.Agents) < 2 {
		t.Fatalf("seeded agent replaced the built-ins: %v", cfg.Agents)
	}
}

// A file that lists agents keeps exactly its own list, seeds and all-or-nothing
// slice semantics included.
func TestDefaultsSeedAgentsLoseToTheFile(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("agents:\n  - name: from-file\n    description: written by the user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := types.Config{Agents: []types.AgentTypeConfig{{Name: "localai-seeded"}}}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	for _, a := range cfg.Agents {
		if a.Name == "localai-seeded" {
			t.Fatalf("seeded agent survived a file that lists its own: %v", cfg.Agents)
		}
	}
}

// Bools are the documented sharp edge: false is indistinguishable from unset,
// so a seeded true beats an explicit `false:` in the file for EVERY bool. This
// pins the behavior the comment on applySeeds promises, Browser.AllowPrivateURLs
// above all, since it gates localhost and RFC1918 access.
func TestSeededBoolsBeatAnExplicitFalse(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"compaction:\n  disabled: false\n"+
			"agent_options:\n  force_reasoning: false\n"+
			"browser:\n  enabled: false\n  allow_private_urls: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := types.Config{
		Compaction:   types.CompactionConfig{Disabled: true},
		AgentOptions: types.AgentOptions{ForceReasoning: true},
		Browser:      types.BrowserConfig{Enabled: true, AllowPrivateURLs: true},
	}

	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: defaults, SkipBareEnv: true})
	if !cfg.Compaction.Disabled {
		t.Fatal("Compaction.Disabled = false; the documented bool behavior changed")
	}
	if !cfg.AgentOptions.ForceReasoning {
		t.Fatal("AgentOptions.ForceReasoning = false; the documented bool behavior changed")
	}
	if !cfg.Browser.Enabled || !cfg.Browser.AllowPrivateURLs {
		t.Fatalf("Browser = %+v; the documented bool behavior changed", cfg.Browser)
	}
}

// The other half of the bool rule: an unseeded bool is untouched, so nib's own
// defaults still apply and nothing is silently switched on.
func TestUnseededBoolsStayFalse(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: types.Config{Model: "m"}, SkipBareEnv: true})
	if cfg.Compaction.Disabled || cfg.Browser.Enabled || cfg.Browser.AllowPrivateURLs ||
		cfg.Computer.Enabled || cfg.AgentOptions.ForceReasoning {
		t.Fatalf("an unseeded bool was switched on: %+v %+v %+v",
			cfg.Compaction, cfg.Browser, cfg.AgentOptions)
	}
}

// fillNonZero recursively sets every field reachable from v to a non-zero
// value. Containers get exactly one element, which is all mergo's emptiness
// test looks at.
func fillNonZero(t *testing.T, v reflect.Value) {
	t.Helper()
	fillVariant{str: "seeded", b: true, i: 7, f: 0.5}.into(t, v)
}

// fillVariant is fillNonZero with the leaf values chosen by the caller. The
// override rot guard needs TWO distinguishable fills, one for the config file
// and one for the overrides, so that "the override landed" can be told apart
// from "both layers happened to agree".
//
// b is the one that has to be chosen rather than inherited: a merge cannot tell
// false from unset, so the lower of the two layers must be filled with false or
// every bool reads as a failure to override.
type fillVariant struct {
	str string
	b   bool
	i   int64
	f   float64
}

func (fv fillVariant) into(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(fv.str)
	case reflect.Bool:
		v.SetBool(fv.b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(fv.i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(fv.i))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(fv.f)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fv.into(t, s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fv.into(t, key)
		val := reflect.New(v.Type().Elem()).Elem()
		fv.into(t, val)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Field(i); f.CanSet() {
				fv.into(t, f)
			}
		}
	case reflect.Ptr:
		p := reflect.New(v.Type().Elem())
		fv.into(t, p.Elem())
		v.Set(p)
	case reflect.Interface:
		// Nothing sensible to synthesize; leave it. types.Config has none today,
		// and the caller's IsZero check would flag one that appeared.
	default:
		t.Fatalf("fillVariant: unhandled kind %s", v.Kind())
	}
}

// TraceDir is runtime-only (yaml:"-"), so a seed is the only way an embedder
// can preset it, and nothing in the file can shadow it.
func TestDefaultsSeedTraceDir(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	cfg := LoadWith(LoadOptions{BaseDir: dir, Defaults: types.Config{TraceDir: "/seeded/traces"}})
	if cfg.TraceDir != "/seeded/traces" {
		t.Fatalf("TraceDir = %q, want /seeded/traces", cfg.TraceDir)
	}
}

// The zero Defaults must change nothing: an unseeded load still yields empty
// credentials, nib's own built-in prompt, and no metadata, not some accidental
// placeholder the merge invented.
func TestZeroDefaultsSeedNothing(t *testing.T) {
	clearBareEnv(t)
	dir := t.TempDir()
	cfg := LoadWith(LoadOptions{BaseDir: dir})
	if cfg.Model != "" || cfg.APIKey != "" || cfg.BaseURL != "" || cfg.ApprovalMode != "" || cfg.TraceDir != "" {
		t.Fatalf("zero Defaults seeded something: %+v", cfg)
	}
	if cfg.LogLevel != "" || cfg.Metadata != nil || cfg.AllowedTools != nil {
		t.Fatalf("zero Defaults seeded something: LogLevel=%q Metadata=%v AllowedTools=%v",
			cfg.LogLevel, cfg.Metadata, cfg.AllowedTools)
	}
	if cfg.Prompt != defaultPrompt {
		t.Fatalf("Prompt = %q, want nib's built-in default", cfg.Prompt)
	}
	if cfg.Compaction.KeepRecent != 8 || cfg.AgentOptions.Iterations != 10 {
		t.Fatalf("built-in defaults disturbed: %+v %+v", cfg.Compaction, cfg.AgentOptions)
	}
}

func TestWritablePathInPrefersInjectedRoot(t *testing.T) {
	dir := t.TempDir()
	got := WritablePathIn(dir)
	want := filepath.Join(dir, "config.yaml")
	if got != want {
		t.Fatalf("WritablePathIn = %q, want %q", got, want)
	}
}
