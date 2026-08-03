// Package app is nib's entrypoint, importable so nib can be embedded in
// another binary. nib's own main.go is a thin wrapper over Main.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/mudler/nib/cmd"
	"github.com/mudler/nib/config"
	"github.com/mudler/nib/internal"
	"github.com/mudler/nib/mcp"
	"github.com/mudler/nib/setup"
	"github.com/mudler/nib/trace"
	"github.com/mudler/nib/types"
	"github.com/mudler/xlog"
	"golang.org/x/term"
)

// Options configures a nib invocation. The zero value reproduces standalone
// nib's behavior exactly, so embedders opt in to each difference.
type Options struct {
	// Args are the arguments after the program name (os.Args[1:]).
	Args []string
	// ProgramName is the name shown in the messages this package prints itself:
	// the --version line, the flag package's usage and parse errors, the setup
	// gate's two aborts, and the injected-stream refusal. Empty means "nib".
	//
	// It also renames the --init shell snippets, which is more than a cosmetic
	// substitution: the widget they define invokes this name, so an embedder's
	// Ctrl+Space runs the embedder's command rather than a `nib` its users do
	// not have. A name of several words ("local-ai chat") stays several words
	// in command position, and the widget's function name is derived from it by
	// reducing it to an identifier.
	//
	// The rule for everything below is one line: if a string names a command
	// that someone is expected to RUN, this renames it; if it merely names the
	// tool, it does not.
	//
	// Renamed, beyond the messages and scripts above:
	//
	//   - The tmux split. Inside tmux the Ctrl+Space widget lands in a split
	//     pane, and the pane has to re-enter THIS program. That is the host
	//     binary plus its subcommand, not the executable path alone, so an
	//     embedded nib without this spawns the host's default command instead.
	//   - The management subcommands' instructions: the `usage: ...` lines of
	//     `plugin`, `skill` and `mcp`, and the hints that follow an install
	//     ("Enable later: local-ai chat plugin enable foo", "verify now with:
	//     local-ai chat mcp test bar"). The enable hint is reached on every
	//     non-interactive `plugin install` without --yes.
	//   - The skill installer's suggestions, which reach the user through those
	//     same subcommands: "use `local-ai chat skill update x`" on a duplicate
	//     pack, and "did you mean `local-ai chat plugin install`?" on a source
	//     with no SKILL.md. The catalog install path included.
	//   - The SYSTEM PROMPT paragraph about registering MCP servers. This is the
	//     one the model relays: it paraphrases the advice in its own words at
	//     whatever moment seems relevant, and unlike a usage line the user gets
	//     no cue that the tool is called something else here. It travels via
	//     types.Config.ProgramName, which this field populates during the load
	//     and always wins over a value arriving through Defaults or Overrides.
	//
	// NOT renamed, deliberately, so these still say "nib" whatever this is set
	// to:
	//
	//   - The CLI and TUI branding (theme.BrandName, the banner, the setup
	//     wizard's header). It names the tool; it is not a command.
	//   - Prose in terminal output that names the tool rather than instructing,
	//     specifically the "available on the next nib session" clause of the mcp
	//     add confirmation, whose "verify now with:" half IS renamed. The
	//     surrounding output makes the referent obvious; the system prompt,
	//     which has no such context, renames the equivalent sentence.
	//   - The "nib: ..." diagnostics that config loading and plugin/skill
	//     discovery write straight to os.Stderr on every run.
	//   - Internal identifiers that are never shown or typed: the MCP server's
	//     implementation name and its nib/reply notification method, the
	//     nib-plugin.yaml manifest filename, the ~/.config/nib state root, and
	//     the tmux temp-file prefix and wait-for channel.
	ProgramName string
	// BaseDir overrides the config, plugins, and skills root. Empty means
	// nib's default XDG resolution.
	BaseDir string
	// Stdin, Stdout, Stderr default to the process streams when nil.
	//
	// CLI mode (--cli) reads and writes exactly these. The TUI, which is the
	// DEFAULT mode, does not: it renders on /dev/tty, because the terminal is
	// still there even when stdout is a pipe, and serving that case is why it
	// opens /dev/tty at all. Only the shell-capture line the TUI prints on exit
	// goes to Stdout.
	//
	// For an embedder that is a rule rather than a caveat: injecting a Stdin or
	// a Stdout that is NOT a terminal (a buffer, a pipe, a file) requires --cli,
	// or the run is refused with an error saying so, rather than rendering into
	// a stream the TUI cannot drive. Injecting the process streams, or any other
	// terminal, leaves every mode working.
	//
	// Only those two are gated (see decideStreamRefusal). A non-terminal Stderr
	// is always accepted, because every error this package prints goes through
	// o.stderr() rather than through the TUI, so a TUI session with its error
	// output captured to a buffer or a log file is a supported combination.
	//
	// One consequence is worth spelling out, because it inverts the usual
	// instinct that passing os.Stdout is the safe, explicit choice. An embedder
	// that wants nib's shell-capture idiom, where the user runs
	// `out=$(myprog agent)` and the TUI prints the selected command on stdout
	// for the shell to capture, must leave Stdout NIL rather than set it to
	// os.Stdout. Nil means "not injected": nib falls back to the process stream,
	// the TUI runs, and the capture line lands on stdout as it should. Setting
	// os.Stdout explicitly injects whatever stdout happens to be, and under
	// $(...) that is a pipe, so the run is refused for a stream nib would have
	// used anyway.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Defaults seed config fields the config file leaves empty. Every field of
	// types.Config is seedable except BaseDir, which is ignored: the root has
	// exactly one knob, the BaseDir field above, and it overwrites any seeded
	// value. See config.LoadOptions.Defaults for what "empty" means for maps,
	// slices and booleans, including that a seeded true beats an explicit
	// `false:` in the config file for every bool.
	Defaults types.Config
	// Overrides are the other half of Defaults: they sit ABOVE the config file
	// and above the bare environment variables, so an embedder can hand nib a
	// value the user's config file must not undo. A CLI flag is the case this
	// exists for. Routed through Defaults, `--endpoint` is accepted and then
	// discarded whenever the file already carries a base_url, which is the
	// normal state rather than an edge case, since anything that writes the file
	// once makes the flag dead from the next run on.
	//
	// Every field of types.Config is overridable except BaseDir, ignored for the
	// same reason it is ignored in Defaults. See config.LoadOptions.Overrides for
	// the merge rules, and in particular for the one asymmetry: an override can
	// only raise a field, never blank one, so an override of false CANNOT beat a
	// `true:` in the config file.
	//
	// Two fields stay above it, by design rather than by omission. nib's own
	// --trace-dir and --yolo, and their NIB_TRACE_DIR and NIB_YOLO twins, are
	// resolved after the config load and still win for TraceDir and
	// ApprovalMode. Both are deliberate instructions to nib rather than ambient
	// environment, which is what separates them from the bare MODEL / API_KEY /
	// BASE_URL that SkipBareEnv exists to suppress.
	//
	// One consequence is sharp and worth stating outright: NIB_YOLO=1 ESCALATES
	// PAST A STRICTER OVERRIDE. An embedder that sets Overrides.ApprovalMode to
	// "strict", "allowlist" or "prompt" is silently downgraded to "auto", which
	// approves every tool call. Filtering Args does not help, because this
	// arrives through the environment: an embedder that must prevent it controls
	// the child process environment, i.e. does not pass NIB_YOLO through.
	//
	// TraceDir has no equivalent hazard: the config file cannot set it at all
	// (it is runtime-only), so routing --trace-dir through Defaults already
	// worked and NIB_TRACE_DIR merely retargets where a trace is written.
	Overrides types.Config
	// SkipSetup suppresses the first-run model wizard. Embedders that resolve
	// the model themselves set this. It suppresses the whole gate, so an
	// explicit --setup in Args becomes a silent no-op rather than an error.
	SkipSetup bool
	// SkipBareEnv suppresses the bare MODEL / API_KEY / BASE_URL variables, and
	// only those three. nib's prefixed variables keep reading the ambient
	// environment and still outrank a seed: NIB_TRACE_DIR overrides a seeded
	// TraceDir, NIB_YOLO forces ApprovalMode to "auto" over a seeded one, and
	// LOG_FORMAT selects the log encoding.
	SkipBareEnv bool
}

func (o Options) name() string {
	if o.ProgramName == "" {
		return "nib"
	}
	return o.ProgramName
}

func (o Options) stdin() io.Reader {
	if o.Stdin == nil {
		return os.Stdin
	}
	return o.Stdin
}

func (o Options) stdout() io.Writer {
	if o.Stdout == nil {
		return os.Stdout
	}
	return o.Stdout
}

func (o Options) stderr() io.Writer {
	if o.Stderr == nil {
		return os.Stderr
	}
	return o.Stderr
}

// Main runs nib and returns a process exit code. argv is the full argument
// vector including the program name. It routes through the same code as Run,
// so the process-wide xlog side effect noted there applies here too.
func Main(argv []string) int {
	var args []string
	if len(argv) > 1 {
		args = argv[1:]
	}
	return run(Options{Args: args})
}

// Run runs nib and returns an error. It never calls os.Exit.
//
// Every failure comes back as a bare ExitError carrying nothing but an exit
// code, the cause having been written to Stderr instead: an unparseable flag,
// an unknown --init shell, an MCP transport failure, the setup abort, the
// injected-stream refusal and a management subcommand's non-zero code are all
// indistinguishable to the caller. The code is 1 for every one of those except
// two: an unparseable flag is 2, and a CLI session that had to refuse a tool
// call because stdin could not approve it is ExitCodeApprovalNoInput.
//
// Cancelling ctx unwinds whichever mode is running, the TUI included, and
// comes back as ExitError{1} with the context's error on Stderr. Signals are
// the caller's: this entrypoint installs no handler, and nothing below it
// listens for one either.
//
// Run also reconfigures the process-wide xlog logger from the resolved config,
// which an embedder sharing that logger will observe.
func Run(ctx context.Context, o Options) error {
	if code := runCtx(ctx, o); code != 0 {
		return ExitError{Code: code}
	}
	return nil
}

// ExitError carries a non-zero exit code out of Run.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// ExitCodeApprovalNoInput is the exit code for a CLI session that refused a
// tool call because stdin was closed and nothing could approve it. See
// cmd.ErrApprovalNoInput for why that is not a success.
//
// It is deliberately neither 0 nor 1: a script piping a prompt in has to be
// able to tell "answered" from "refused to act" without reading stdout, and
// from a crash or a bad flag (2) without guessing. Scripts branch on this
// number, so it is part of nib's interface and does not change.
const ExitCodeApprovalNoInput = 3

func run(o Options) int {
	// The management subcommands run BEFORE the handler is installed, exactly as
	// standalone nib's main dispatched them above its signal.Notify.
	//
	// They take no context, so a handler installed around them would have
	// nothing to cancel: the signal would land in the buffered channel and stop
	// there, leaving `nib skill install` over a slow network needing a kill -9.
	// Dispatching first restores the process default disposition, so Ctrl+C kills
	// them outright.
	//
	// Suppressing the handler for those argv shapes instead would work equally
	// well, but this keeps the ordering itself the reason, which is what the
	// baseline expressed and what stays true if a subcommand is added later.
	if code, handled := dispatchManage(o); handled {
		return code
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// run is library code, so the handler has to be torn down on the way out:
	// without signal.Stop the channel stays registered with the runtime, and
	// without the ctx.Done() arm the goroutine blocks on <-sigs forever.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()

	return runCtx(ctx, o)
}

// dispatchManage runs the management subcommands: `plugin`, `skill`, and the
// `mcp` verbs that manage configured servers rather than serving the agent.
// They need config but neither transports nor a context, so they early-exit
// before any of the setup below. handled is false for every other argv.
//
// It is a function rather than inline dispatch because run has to reach it
// before installing its signal handler while runCtx, which Run enters directly,
// still has to reach it at all. Calling it twice on the run path is harmless:
// run has already returned by then whenever handled was true.
func dispatchManage(o Options) (code int, handled bool) {
	args := o.Args
	switch {
	case len(args) >= 1 && args[0] == "plugin":
		return cmd.RunPluginCommand(o.name(), o.BaseDir, args[1:]), true
	case len(args) >= 1 && args[0] == "skill":
		return cmd.RunSkillCommand(o.name(), o.BaseDir, args[1:]), true
	// Bare `nib mcp` / --http / --stdio still serve, so only the management
	// verbs match here.
	case len(args) >= 2 && args[0] == "mcp" && cmd.IsMCPManageSubcommand(args[1]):
		return cmd.RunMCPCommand(o.name(), o.BaseDir, args[1:]), true
	}
	return 0, false
}

func runCtx(ctx context.Context, o Options) int {
	args := o.Args

	// Subcommand dispatch must precede flag parsing.
	if code, handled := dispatchManage(o); handled {
		return code
	}

	// `nib mcp` needs config + transports (built below), so it cannot early-exit
	// like plugin/skill. Capture its args and hide them from the flag parser.
	mcpMode := len(args) >= 1 && args[0] == "mcp"
	var mcpArgs []string
	if mcpMode {
		mcpArgs = args[1:]
		args = nil
	}

	fs := flag.NewFlagSet(o.name(), flag.ContinueOnError)
	fs.SetOutput(o.stderr())
	heightFlag := fs.String("height", "", "Height of the TUI (e.g., '40%' or '20')")
	initFlag := fs.String("init", "", "Output shell integration script (zsh, bash, or fish)")
	versionFlag := fs.Bool("version", false, "Print version and exit")
	tmuxFlag := fs.Bool("tmux", false, "Run in tmux popup (auto-detected if in tmux)")
	noTmuxFlag := fs.Bool("no-tmux", false, "Disable tmux popup even when in tmux")
	tuiFlag := fs.Bool("tui", false, "Start the full-screen TUI directly (no tmux popup)")
	cliFlag := fs.Bool("cli", false, "Run in plain CLI mode instead of the TUI")
	setupFlag := fs.Bool("setup", false, "Run the interactive model setup wizard")
	traceDirFlag := fs.String("trace-dir", "", "Write a session LLM trace (NDJSON) and token totals (usage.json) to this directory; also via NIB_TRACE_DIR")
	yoloFlag := fs.Bool("yolo", false, "Auto-approve every tool call without prompting; also via NIB_YOLO")
	if err := fs.Parse(args); err != nil {
		// ContinueOnError hands back ErrHelp for -h/--help, which the global
		// flag.CommandLine used to turn into a clean exit. Keep that.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *versionFlag {
		fmt.Fprintf(o.stdout(), "%s %s\n", o.name(), internal.PrintableVersion())
		return 0
	}

	if *initFlag != "" {
		script := cmd.GetInitScript(*initFlag, o.name())
		if script == "" {
			fmt.Fprintf(o.stderr(), "Unknown shell: %s. Supported: zsh, bash, fish\n", *initFlag)
			return 1
		}
		fmt.Fprint(o.stdout(), script)
		return 0
	}

	cfg := config.LoadWith(config.LoadOptions{
		BaseDir:     o.BaseDir,
		Defaults:    o.Defaults,
		Overrides:   o.Overrides,
		SkipBareEnv: o.SkipBareEnv,
	})

	// The name travels with the config because the system prompt is rendered
	// from it, and that prompt tells the model what the user can type. Assigned
	// unconditionally, the same way BaseDir is: Options.ProgramName is the one
	// knob, so a name arriving through Defaults or Overrides cannot disagree
	// with the name --version, the init scripts and the management subcommands
	// are already using.
	cfg.ProgramName = o.name()

	// Tracing is runtime-only: the flag wins, otherwise fall back to the env var.
	if *traceDirFlag != "" {
		cfg.TraceDir = *traceDirFlag
	} else if env := os.Getenv("NIB_TRACE_DIR"); env != "" {
		cfg.TraceDir = env
	}

	// Tracing was asked for explicitly, so it either works or the run stops.
	// Checked here, before StartTransports, so a failure spawns no MCP
	// subprocesses — and because this is the only place that can exit cleanly
	// in every mode: the TUI builds its session inside a tea.Cmd, where this
	// error would instead surface as a banner over an already-running UI.
	if cfg.TraceDir != "" {
		if err := trace.Preflight(cfg.TraceDir); err != nil {
			fmt.Fprintf(o.stderr(), "%s: %v\n", o.name(), err)
			return 1
		}
	}

	// "yolo" mode auto-approves every tool call. The flag or env var force
	// "auto" approval, overriding whatever the config file set.
	if *yoloFlag || envTrue(os.Getenv("NIB_YOLO")) {
		cfg.ApprovalMode = "auto"
	}

	if cfg.LogLevel == "" {
		cfg.LogLevel = "error"
	}
	xlog.SetLogger(xlog.NewLogger(xlog.LogLevel(cfg.LogLevel), os.Getenv("LOG_FORMAT")))

	isTTY := false
	if f, ok := o.stdin().(*os.File); ok {
		isTTY = term.IsTerminal(int(f.Fd()))
	}

	// An embedder that resolves the model itself owns that decision, so neither
	// the abort nor the wizard applies: cfg is used as loaded.
	if !o.SkipSetup {
		switch decideSetup(cfg.ResolvedMainModel().Model != "", *setupFlag, isTTY) {
		case setupAbort:
			if *setupFlag {
				fmt.Fprintf(o.stderr(), "%s --setup requires an interactive terminal\n", o.name())
			} else {
				fmt.Fprintf(o.stderr(), "%s: no model configured. Run `%s --setup`, or set MODEL/API_KEY/BASE_URL.\n", o.name(), o.name())
			}
			return 1
		case setupRun:
			newCfg, saved, err := setup.Run(ctx, cfg)
			if err != nil {
				fmt.Fprintf(o.stderr(), "setup: %v\n", err)
				return 1
			}
			if !saved {
				return 0 // user cancelled
			}
			cfg.Model, cfg.APIKey, cfg.BaseURL = newCfg.Model, newCfg.APIKey, newCfg.BaseURL
		}
	}

	// Shared shell-job registry: the shell MCP server starts/manages jobs in it,
	// and the TUI lists them (footer) and backgrounds the foreground one (Ctrl+B).
	shellJobs := mcp.NewShellJobs()

	transports, err := mcp.StartTransports(ctx, cfg, shellJobs)
	if err != nil {
		fmt.Fprintf(o.stderr(), "Error starting MCP servers: %v\n", err)
		return 1
	}

	if mcpMode {
		if err := cmd.RunMCP(ctx, cfg, mcpArgs, shellJobs, transports...); err != nil {
			fmt.Fprintf(o.stderr(), "Error: %v\n", err)
			return 1
		}
		return 0
	}

	// The raw fields, not the o.stdX() accessors: cmd.Streams applies the same
	// nil-means-process-stream defaulting itself, so forwarding the accessors'
	// output would just resolve the defaults twice.
	streams := cmd.Streams{In: o.Stdin, Out: o.Stdout, Err: o.Stderr}

	mode := selectMode(modeInputs{
		cli:    *cliFlag,
		tui:    *tuiFlag,
		tmux:   *tmuxFlag,
		height: *heightFlag,
		inTmux: cmd.IsInTmux(),
	})

	// Only CLI mode can honor injected streams. Rendering the TUI into a
	// buffer or a pipe is not something to attempt and half-succeed at, so say
	// so instead of quietly using /dev/tty and leaving the embedder's writer
	// empty.
	if name := decideStreamRefusal(mode, injectedReader(o.Stdin), injectedWriter(o.Stdout)); name != "" {
		fmt.Fprintf(o.stderr(), "%s: %s was injected as a non-terminal stream, which the TUI cannot render into. Re-run with --cli to use the injected streams.\n", o.name(), name)
		return 1
	}

	switch mode {
	case modeCLI:
		if err := cmd.RunCLI(ctx, cfg, streams, shellJobs, transports...); err != nil {
			fmt.Fprintf(o.stderr(), "Error: %v\n", err)
			// The one CLI failure a caller is expected to branch on rather than
			// just report, so it gets its own code instead of the blanket 1.
			if errors.Is(err, cmd.ErrApprovalNoInput) {
				return ExitCodeApprovalNoInput
			}
			return 1
		}
	case modeInline:
		h := *heightFlag
		if h == "" {
			h = "40%"
		}
		height := parseHeight(h)
		useTmux := *tmuxFlag || (cmd.IsInTmux() && !*noTmuxFlag)
		if useTmux && cmd.IsInTmux() {
			if err := cmd.RunTmuxSplit(o.name(), *heightFlag, cfg.TraceDir); err != nil {
				fmt.Fprintf(o.stderr(), "Error: %v\n", err)
				return 1
			}
		} else {
			if err := cmd.RunTUI(ctx, cfg, height, streams, shellJobs, transports...); err != nil {
				fmt.Fprintf(o.stderr(), "Error: %v\n", err)
				return 1
			}
		}
	default: // modeTUI, fullscreen, direct (no tmux split)
		if err := cmd.RunTUI(ctx, cfg, parseHeight("100%"), streams, shellJobs, transports...); err != nil {
			fmt.Fprintf(o.stderr(), "Error: %v\n", err)
			return 1
		}
	}
	return 0
}

// parseHeight parses a height string like "40%" or "20". A negative result
// means "percentage of terminal height".
func parseHeight(s string) int {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "%") {
		pct, err := strconv.Atoi(strings.TrimSuffix(s, "%"))
		if err != nil || pct <= 0 || pct > 100 {
			return 40 // default
		}
		return -pct // negative means percentage
	}
	height, err := strconv.Atoi(s)
	if err != nil || height <= 0 {
		return 20 // default
	}
	return height
}

// envTrue reports whether an environment variable value is truthy. Empty,
// "0", "false", "no", and "off" (any case) are false; everything else is true,
// so `NIB_YOLO=1`, `NIB_YOLO=true`, and a bare `NIB_YOLO=` set in the shell all
// behave sensibly.
func envTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}
