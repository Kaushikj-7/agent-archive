package cli

import (
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

type handoffConfigState struct {
	cfg   config.Config
	found bool
}

func handoffConfig(home string, opts handoffOptions) (config.Config, bool, error) {
	if opts.config != nil {
		return opts.config.cfg, opts.config.found, nil
	}
	return config.Load(home)
}

type handoffOptions struct {
	config    *handoffConfigState
	native    bool
	sessionID string
	revision  string
	project   string
	harness   string
	// allProjects searches and lists every project, not only the working
	// directory's repository.
	allProjects bool
	file        string
	source      string
	format      string
	output      string
	to          string
	latest      bool
	force       bool
	noPreamble  bool
	maxBytes    int
	// worktree launches in a new git worktree on branch (default
	// handoff/<short id>) instead of the current checkout.
	worktree bool
	branch   string
	// here and newWindow force where a launched agent runs.
	here      bool
	newWindow bool
	// agentArgs are the arguments after `--`, given to the launched agent.
	agentArgs []string
}

type handoffDestination string

const (
	handoffDestinationClaude handoffDestination = "claude"
	handoffDestinationCodex  handoffDestination = "codex"
	handoffDestinationCursor handoffDestination = "cursor"
)

func parseHandoffOptions(args []string, stderr io.Writer, env handoffOptionsDependencies, interactive bool) (handoffOptions, bool) {
	fs := env.newCommandFlags("handoff", stderr)
	revision := fs.String("revision", "", "read this exact parent native revision ID (requires SESSION_ID and --source archive)")
	latest := fs.Bool("latest", false, "the most recent session for the project, by path or by repository (remote origin)")
	project := fs.String("project", "", "the project to pick from and search, as a directory or a project name, and the directory --latest searches (default: the current directory's repository)")
	allProjects := fs.Bool("all-projects", false, "pick from and search every project, not only the current repository's")
	harness := fs.String("harness", "", "only sessions from this harness (claude, codex, cursor)")
	file := fs.String("file", "", "render this native transcript file directly (requires --harness)")
	source := fs.String("source", "auto", "where session content comes from: auto, local, or archive")
	maxBytes := fs.Int("max-bytes", archive.DefaultHandoffMaxBytes, "output budget in bytes; 0 means no limit")
	format := fs.String("format", "markdown", "markdown or json")
	output := fs.String("output", "", "write to this file (mode 0600) instead of stdout")
	force := fs.Bool("force", false, "with --output, replace an existing file")
	noPreamble := fs.Bool("no-preamble", false, "omit the note addressed to the receiving agent")
	to := fs.String("to", "", "launch a local claude, codex, or cursor session with this handoff")
	worktree := fs.Bool("worktree", false, "launch the agent in a new git worktree carrying this checkout's uncommitted and untracked files")
	branch := fs.String("branch", "", "with --worktree, the new branch (default: handoff/ and the first 8 characters of SESSION_ID)")
	here := fs.Bool("here", false, "run the launched agent in this terminal")
	newWindow := fs.Bool("new-window", false, "open the launched agent in a new terminal window or tab")
	// Everything after the first `--` belongs to the agent, not to flag
	// parsing, which would otherwise read it as a session ID.
	var agentArgs []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, agentArgs = args[:i], slices.Clone(args[i+1:])
	}
	sessionID, ok := fs.parseWithArgument(args)
	if !ok {
		return handoffOptions{}, false
	}
	if checkNonemptyRevisionFlag(fs, *revision) != 0 {
		return handoffOptions{}, false
	}
	opts := handoffOptions{revision: *revision, sessionID: sessionID, project: *project, allProjects: *allProjects, harness: *harness,
		file: *file, source: *source, maxBytes: *maxBytes, format: *format, to: *to,
		output: *output, latest: *latest, force: *force, noPreamble: *noPreamble, here: *here, newWindow: *newWindow, agentArgs: agentArgs,
		worktree: *worktree, branch: *branch}
	if message := validateHandoffOptionsWithCatalog(&opts, interactive, catalogFor(env)); message != "" {
		fs.usageError("%s", message)
		return handoffOptions{}, false
	}
	return opts, true
}

// validateHandoffOptions returns the usage error for opts, or "" when they
// are valid, in which case opts.harness is now the canonical harness name.
func validateHandoffOptionsWithCatalog(opts *handoffOptions, interactive bool, c agentmeta.Catalog) string {
	if opts.to != "" {
		opts.to = agentmeta.Canonical(c, opts.to)
		if opts.to == "" {
			return "--to must be claude, codex, or cursor"
		}
	}
	if message := validateHandoffFlagCombinations(*opts, interactive); message != "" {
		return message
	}
	if message := validateHandoffLaunchOptionsWithCatalog(opts.to, opts.output, opts.format, opts.noPreamble, c); message != "" {
		return message
	}
	if message := validateHandoffWindowOptions(*opts, interactive); message != "" {
		return message
	}
	canonical, ok := harnessFlagWithCatalog(c, opts.harness)
	if !ok {
		return harnessFlagError(opts.harness)
	}
	opts.harness = canonical
	return validateHandoffSourceOptions(*opts)
}

// validateHandoffFlagCombinations checks which session is selected and the
// flags that apply only alongside another.
func validateHandoffFlagCombinations(opts handoffOptions, interactive bool) string {
	selectors := 0
	for _, set := range []bool{opts.sessionID != "", opts.latest, opts.file != ""} {
		if set {
			selectors++
		}
	}
	switch {
	case invalidHandoffRevisionSelection(opts):
		return "--revision requires SESSION_ID and --source archive; it cannot read latest or local transcripts"
	// An argument of only spaces names nothing, and must not stand for the
	// no selector that lets --to take the calling agent's own session.
	case opts.sessionID != "" && strings.TrimSpace(opts.sessionID) == "":
		return "the session ID or title is empty"
	// With --to the command decides after parsing: the calling agent's own
	// session, else the picker on a terminal (runHandoffCommand).
	case selectors == 0 && !interactive && opts.to == "":
		return noSelectorMessage
	case selectors > 1:
		return "a session ID, --latest, and --file are mutually exclusive"
	case opts.file != "" && opts.harness == "":
		return "--file requires --harness (claude, codex, or cursor)"
	case opts.project != "" && opts.allProjects:
		return "--project and --all-projects cannot be used together: --project picks one project, --all-projects searches every project"
	case opts.project != "" && opts.file != "":
		return "--project does not apply to --file, which names a transcript"
	case opts.allProjects && (opts.latest || opts.file != ""):
		return "--all-projects applies to the picker and to a title, not to --latest or --file"
	case opts.force && opts.output == "":
		return "--force applies only to --output"
	case opts.maxBytes < 0:
		return "--max-bytes must be 0 or more"
	case len(opts.agentArgs) > 0 && opts.to == "":
		return "arguments after -- go to the launched agent; name it with --to"
	case opts.worktree && opts.to == "" && !offersDestinations(opts, interactive):
		return "--worktree applies to a launched agent: name it with --to, or choose one on a terminal"
	case opts.branch != "" && !opts.worktree:
		return "--branch applies only to --worktree"
	default:
		return ""
	}
}

// validateHandoffSourceOptions checks --source and --format.
func validateHandoffSourceOptions(opts handoffOptions) string {
	if !slices.Contains([]string{"auto", "local", "archive"}, opts.source) {
		return fmt.Sprintf("--source must be auto, local, or archive, not %q", opts.source)
	}
	if !slices.Contains([]string{"markdown", "json"}, opts.format) {
		return fmt.Sprintf("--format must be markdown or json, not %q", opts.format)
	}
	if opts.file != "" && opts.source == "archive" {
		return "--file reads a local transcript; --source archive does not apply"
	}
	return ""
}

// noSelectorMessage is the usage error for a handoff with nothing selected
// and no terminal to pick on.
const noSelectorMessage = "name a session ID or title, --latest, or --file PATH; run on a terminal to pick a session"

// noCurrentSessionMessage is noSelectorMessage for --to, which can also take
// the agent session it runs in.
const noCurrentSessionMessage = "name a session ID or title, --latest, or --file PATH; with none, --to hands off the Claude Code, Codex, or Cursor session it runs in, or asks on a terminal"

func validateHandoffLaunchOptionsWithCatalog(to, output, format string, noPreamble bool, c agentmeta.Catalog) string {
	switch {
	case to != "" && !launchSupported(c, to):
		return "--to must be claude, codex, or cursor"
	case to != "" && output != "":
		return "--to and --output cannot be used together"
	case to != "" && format != "markdown":
		return "--to requires markdown format"
	case to != "" && noPreamble:
		return "--to includes the receiving-agent preamble"
	default:
		return ""
	}
}

// validateHandoffWindowOptions checks --here and --new-window, which say
// where a launched agent runs.
func validateHandoffWindowOptions(opts handoffOptions, interactive bool) string {
	switch {
	case opts.here && opts.newWindow:
		return "--here and --new-window are mutually exclusive"
	// Without --to, only the destination prompt can launch an agent.
	case (opts.here || opts.newWindow) && opts.to == "" && !offersDestinations(opts, interactive):
		return "--here and --new-window apply to a launched agent: name it with --to, or choose one on a terminal"
	case opts.here && !interactive:
		return "--here needs a terminal: stdin and stdout must both be one, with " + envNonInteractive + " off (it is on inside coding agents)"
	default:
		return ""
	}
}

func (e Env) loadHandoffConfig(home string) (config.Config, bool, error) {
	if e.handoffConfigLoad != nil {
		return e.handoffConfigLoad(home)
	}
	return config.LoadWithCatalog(home, e.agentRegistry().Catalog())
}

func invalidHandoffRevisionSelection(opts handoffOptions) bool {
	return opts.revision != "" && (opts.sessionID == "" || opts.latest || opts.file != "" || opts.source != "archive")
}
