package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// handoffDir is the data-directory entry holding untrimmed handoffs saved when
// the budget trimmed the printed one, and the directories holding the copies
// launched agents read (launch-*/). uninstall's localStateEntries lists it.
const handoffDir = "handoffs"

// handoffMaxAge is how long a saved full handoff is kept.
const handoffMaxAge = 7 * 24 * time.Hour

// handoffFallbackRows is how many recent sessions `--latest` lists when it
// finds no session for the project.
const handoffFallbackRows = 5

// handoffTarget is one resolved session: its filtered bundle, its metadata
// when it came from the archive, and where it came from.
type handoffTarget struct {
	native   *nativesessions.Candidate
	bundle   archive.SourceBundle
	analysis archive.Analysis
	parseErr error
	analyzed bool
	metadata *archive.Metadata
	source   string
	filePath string
	// describe is the stderr line naming a --latest choice.
	describe string
	// startedAt and lastActivityAt are what this machine knows about a local
	// session (its registration, its transcript's modification time), used
	// only where the transcript records no timestamps.
	startedAt      time.Time
	lastActivityAt time.Time
}

var errHandoffNotSetUp = errors.New("handoff not set up")

// runHandoffCommand implements `agent-archive handoff`. It prints transcript
// content, which the command itself is the explicit request for; every
// rendered byte comes from a filtered bundle, whether that bundle was
// downloaded or built in memory from a local transcript.
func runHandoffCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env handoffCommandDependencies) int {
	// Every question below (the picker, "Continue in:", running a launched
	// agent here) depends on this, which is false inside an agent's shell
	// even when that shell is a pseudo-terminal.
	interactive := browseInteractive(env, stdin, stdout)
	opts, ok := parseHandoffOptions(args, stderr, env, interactive)
	if !ok {
		return 2
	}
	home, err := env.readHome()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: handoff: resolve home: %v\n", err)
		return 1
	}
	// Every prompt reads through one buffer, so an answer typed (or
	// scripted) ahead for a later prompt is not lost to an earlier one's.
	// A launched agent gets stdin itself: it must see the terminal.
	input := newTypedInput(stdin)
	answers := input.answers
	// Only an argument the person typed is a query; an ID the picker or the
	// calling agent chose below is already a session's.

	var target handoffTarget
	if opts.file == "" {
		cfg, found, loadErr := env.loadHandoffConfig(home)
		if loadErr != nil {
			terminal.Printf(stderr, "agent-archive: handoff: load config: %v\n", loadErr)
			return 1
		}
		opts.config = &handoffConfigState{cfg, found}
		if !found {
			if setupjournal.TransactionPending(home) {
				terminal.Println(stderr, "agent-archive: handoff: setup recovery is pending; run agent-archive setup")
				return 1
			}
			if _, e := os.Lstat(draftPath(home)); !errors.Is(e, os.ErrNotExist) {
				terminal.Println(stderr, "agent-archive: handoff: saved setup is pending or inaccessible; run agent-archive setup")
				return 1
			}
			opts.native = true
			var selected bool
			var code int
			target, selected, code = resolveNativeHandoff(opts, interactive, input, stdout, stderr, env)
			if code != 0 || !selected {
				return code
			}
		}
	}
	if opts.file != "" && (opts.to != "" || offersDestinations(opts, interactive)) {
		cfg, found, loadErr := env.loadHandoffConfig(home)
		if loadErr != nil {
			terminal.Printf(stderr, "agent-archive: handoff: load config: %v\n", loadErr)
			return 1
		}
		opts.config = &handoffConfigState{cfg: cfg, found: found}
	}
	if !opts.native {
		if opts.sessionID != "" {
			if code, done := resolveHandoffQuery(&opts, home, interactive, input, stdout, stderr, env); done {
				return code
			}
		}
		if opts.sessionID == "" && !opts.latest && opts.file == "" {
			if code, done := chooseHandoffSession(&opts, home, interactive, input, stdout, stderr, env); done {
				return code
			}
		}
		target, err = resolveHandoffTarget(opts, home, stderr, env, newRepoMatchGate(opts, interactive, answers, stderr, stdin))
		if err != nil {
			if errors.Is(err, errRepoMatchNotUsed) {
				// The gate said why, and how to use the session.
				return 1
			}
			if errors.Is(err, errHandoffNotSetUp) {
				terminal.Println(stderr, notSetUpMessage)
			} else {
				terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
			}
			return 1
		}
	}
	if target.describe != "" {
		terminal.Printf(stderr, "handoff: using %s\n", target.describe)
	}
	if !target.analyzed {
		target, err = analyzeTarget(context.Background(), parsersFor(env), target)
		if err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
			return 1
		}
	}
	h, err := archive.BuildHandoffWithAnalysis(target.bundle, target.analysis, target.metadata, archive.HandoffOptions{Source: target.source, StartedAt: target.startedAt, LastActivityAt: target.lastActivityAt,
		Checkout: handoffCheckout(opts, env)})
	if err != nil {
		terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
		return 1
	}
	if target.native != nil {
		h.Session.ArchiveSessionID = ""
	}
	noteBranchDifference(h, stderr)
	rendered := prepareHandoff(h, target.bundle, opts, home, stderr, env)
	dest := handoffDestination(opts.to)
	if offersDestinations(opts, interactive) {
		p := newPrompter(answers, stdout)
		p.source = stdin
		defer p.close()
		var choice handoffChoice
		var err error
		if opts.config != nil {
			choice, err = askHandoffDestinationConfig(p, h, opts.config.cfg, env)
		} else {
			choice, err = askHandoffDestination(p, h, home, env)
		}
		if err == nil && choice.action != handoffLaunch {
			err = deliverHandoff(choice, p, rendered, target, opts, stdout, stderr, env)
		}
		if err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
			return 1
		}
		if choice.action != handoffLaunch {
			if opts.worktree {
				terminal.Println(stderr, "handoff: nothing launched, so no worktree was created")
			}
			return 0
		}
		dest = choice.dest
		p.close()
	}
	if dest != "" {
		// Run from inside an agent (interaction is off there), without a
		// terminal, or asked to, the new agent gets a terminal of its own.
		here := interactive && !opts.newWindow
		if err := launchPreparedHandoff(rendered, h, target, dest, here, opts, home, stdin, answers, stdout, stderr, env); err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
			return 1
		}
		return 0
	}
	if err := writeHandoffResult(rendered, opts, stdout, stderr); err != nil {
		terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
		return 1
	}
	return 0
}

func resolveHandoffTarget(opts handoffOptions, home string, stderr io.Writer, env handoffTargetDependencies, gate repoMatchGate) (handoffTarget, error) {
	if opts.file != "" {
		return handoffFromFile(opts.file, opts.harness, env)
	}
	cfg, found, err := handoffConfig(home, opts)
	if err != nil {
		return handoffTarget{}, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return handoffTarget{}, errHandoffNotSetUp
	}
	skip := currentSessions(env)
	if opts.to != "" {
		// The calling agent is the source of a direct handoff.
		skip = nil
	}
	ctx, stopReads := readCommandContext(env)
	defer stopReads()
	resolver := handoffResolver{ctx: ctx, env: env, home: home, cfg: cfg,
		harness: opts.harness, source: opts.source, skip: skip, gate: gate, stderr: stderr}
	if !opts.latest {
		return resolver.byID(opts.sessionID)
	}
	dir := opts.project
	if dir == "" {
		dir, err = env.workingDir()
	}
	if err != nil {
		return handoffTarget{}, err
	}
	// The lookup finds the repository from any directory inside it, so a
	// subdirectory has its checkout's key and a parent of several
	// repositories has none. It needs an absolute path.
	if abs, absErr := filepath.Abs(dir); absErr == nil {
		resolver.repoKey = env.repoKeyResolver()(abs)
	}
	return resolver.latest(dir)
}

func renderHandoff(h archive.Handoff, opts handoffOptions) []byte {
	if opts.format == "json" {
		data, _ := json.MarshalIndent(h, "", "  ")
		return append(archive.DisplayJSON(data), '\n')
	}
	return archive.RenderHandoffMarkdown(h, archive.HandoffRenderOptions{Preamble: !opts.noPreamble})
}

type handoffRenderPlan struct {
	fullPath string
	full     []byte
	fitted   archive.Handoff
}

// planHandoffRendering decides what to trim and where an untrimmed copy
// would live. It leaves saving that copy to prepareHandoff.
func planHandoffRendering(h archive.Handoff, bundle archive.SourceBundle, opts handoffOptions, home string) handoffRenderPlan {
	fullPath := handoffFullPath(home, bundle, opts.format)
	full := renderHandoff(h, opts)
	h.FullRecordPath = fullPath
	fitted, _ := archive.FitHandoff(h, opts.maxBytes, func(h archive.Handoff) int { return len(renderHandoff(h, opts)) })
	return handoffRenderPlan{fullPath: fullPath, full: full, fitted: fitted}
}

func prepareHandoff(h archive.Handoff, bundle archive.SourceBundle, opts handoffOptions, home string, stderr io.Writer, env handoffFileDependencies) []byte {
	if !opts.native {
		pruneHandoffs(home, env.now())
	}
	plan := planHandoffRendering(h, bundle, opts, home)
	fitted := plan.fitted
	if len(fitted.Elisions) > 0 {
		// The data directory exists once setup has run. `--file` works
		// without setup, and must not create it just to hold a copy of a
		// transcript nobody opted in to archiving.
		if _, statErr := os.Stat(home); opts.native || statErr != nil {
			terminal.Println(stderr, "agent-archive: handoff: note: output was trimmed and, without setup, the untrimmed version is not saved; use --max-bytes 0 for all of it")
			fitted.FullRecordPath = ""
		} else if err := local.WriteBytes(plan.fullPath, plan.full); err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: warning: could not save the untrimmed handoff: %v\n", err)
			fitted.FullRecordPath = ""
		}
	} else {
		fitted.FullRecordPath = ""
	}
	rendered := renderHandoff(fitted, opts)
	if opts.maxBytes <= 0 || len(rendered) <= opts.maxBytes {
		return rendered
	}
	// FitHandoff could not fit: one part (where it left off, the plan, the
	// file list, the last exchange) is over the limit by itself, or the
	// session has so many exchanges that their prompts' first lines are.
	// Markdown is cut at a line;
	// JSON cannot be cut and still parse, so it is left whole.
	if opts.format == "json" {
		terminal.Printf(stderr, "agent-archive: handoff: warning: still %d bytes after trimming, over the %d-byte limit\n", len(rendered), opts.maxBytes)
		return rendered
	}
	terminal.Printf(stderr, "agent-archive: handoff: warning: still %d bytes after trimming; cut to the %d-byte limit\n", len(rendered), opts.maxBytes)
	return archive.CutHandoffMarkdown(rendered, fitted, opts.maxBytes)
}

func writeHandoffResult(rendered []byte, opts handoffOptions, stdout, stderr io.Writer) error {
	if opts.output == "" {
		if err := writeOutput(stdout, rendered); err != nil {
			return err
		}
		return nil
	}
	if err := writeHandoffOutput(opts.output, rendered, opts.force); err != nil {
		return err
	}
	terminal.Printf(stderr, "handoff: wrote %s (%d bytes)\n", opts.output, len(rendered))
	return nil
}

func (env Env) workingDir() (string, error) {
	if env.WorkingDir != nil {
		return env.WorkingDir()
	}
	return os.Getwd()
}

// handoffFromFile filters a native transcript that may have no registration.
// Its modification time stands in for a start time, which only Cursor's text
// fallback needs.
func handoffFromFile(path, harness string, env handoffFileDependencies) (handoffTarget, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return handoffTarget{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return handoffTarget{}, err
	}
	filtered, adapter, err := collector.FilterTranscriptFile(harness, abs, info.ModTime(), registryFor(env))
	if err != nil {
		return handoffTarget{}, fmt.Errorf("filter %s: %w", path, err)
	}
	sum := sha256.Sum256([]byte(abs))
	nativeID := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	if len(filtered.SessionIDs) > 0 {
		nativeID = filtered.SessionIDs[0]
	}
	reg := archive.SessionRegistration{
		ArchiveSessionID: "file-" + hex.EncodeToString(sum[:])[:16],
		NativeSessionID:  nativeID,
		ProjectID:        "file",
		ProjectRoot:      filepath.Dir(abs),
		Harness:          archive.Harness{Name: adapter.Name()},
		SessionStartedAt: info.ModTime(),
	}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, env.now().UTC(), nil)
	if err != nil {
		return handoffTarget{}, err
	}
	return analyzeTarget(context.Background(), parsersFor(env), handoffTarget{bundle: bundle, source: "file", filePath: abs, lastActivityAt: info.ModTime()})
}

// errNotRegisteredHere means a session ID has no registration on this machine,
// which is expected for a session captured elsewhere and not worth reporting
// beside an archive error.
var errNotRegisteredHere = errors.New("no session registered on this machine")

// currentSessions returns the qualified session identities of the agent this command is
// running inside, if it says.
func currentSessions(env currentSessionDependencies) map[agentmeta.SessionKey]bool {
	ids := map[agentmeta.SessionKey]bool{}
	for _, observation := range runtimeObservations(env) {
		if observation.NativeID != "" {
			key, err := agentmeta.NewSessionKey(string(observation.Agent), observation.NativeID)
			if err == nil {
				ids[key] = true
			}
		}
	}
	return ids
}

func handoffSessionKey(harness, nativeID string) agentmeta.SessionKey {
	return agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(harness)), NativeID: nativeID}
}

// localTarget builds a handoff target from a registration's transcript.
func (r handoffResolver) localTarget(reg archive.SessionRegistration) (handoffTarget, error) {
	bundle, err := collector.ReadLocalBundle(r.ctx, r.home, reg, r.env.now().UTC(), r.env.cursorDatabase(), registryFor(r.env))
	if err != nil {
		return handoffTarget{}, err
	}
	lastActivityAt, _ := collector.LastActivity(r.ctx, reg, r.env.cursorDatabase(), registryFor(r.env))
	return analyzeTarget(r.ctx, parsersFor(r.env), handoffTarget{bundle: bundle, source: "local", startedAt: reg.SessionStartedAt, lastActivityAt: lastActivityAt})
}

// hasPrompt reports whether a bundle holds anything the person said, so
// `--latest` passes over a session that has only just started. It is
// archive.SessionLabels's second result, without deriving the labels.
func hasPrompt(analysis archive.Analysis) bool {
	_, ok := archive.LabelsFromAnalysis(analysis)
	return ok
}

type handoffResolver struct {
	ctx     context.Context
	env     handoffResolverDependencies
	home    string
	cfg     config.Config
	harness string
	source  string
	// skip holds qualified session identities `--latest` must pass over: the agent
	// session running the command.
	skip map[agentmeta.SessionKey]bool
	// repoKey is the key of the repository the current directory is in (a
	// hash of its origin, see archive.RepoKey), or "" when it has none.
	repoKey string
	// gate is asked about a session chosen only by repository, before any of
	// its source is read (see acceptRepoMatch). Nil accepts none.
	gate repoMatchGate
	// stderr receives warnings, such as a skipped metadata sidecar.
	stderr io.Writer
}

// byID resolves an explicit archive session ID: the local registration first
// (fresh, and available while paused or offline), then the archive.
func (r handoffResolver) byID(id string) (handoffTarget, error) {
	// localErr is why the local transcript could not be used; with --source
	// auto the archive's published copy is tried next, and both reasons are
	// reported if it fails too.
	var localErr error
	if r.source != "archive" {
		reg, found, err := state.OpenReadOnly(r.home).LoadRegistration(id)
		switch {
		case err != nil:
			localErr = err
		case found && (r.harness == "" || archive.CanonicalHarness(reg.Harness.Name) == archive.CanonicalHarness(r.harness)):
			target, err := r.localTarget(reg)
			if err == nil {
				return target, nil
			}
			localErr = err
		default:
			localErr = fmt.Errorf("%w: %q", errNotRegisteredHere, id)
		}
		if r.source == "local" {
			return handoffTarget{}, localErr
		}
	}
	target, err := r.archiveByID(id)
	if err != nil && localErr != nil && !errors.Is(localErr, errNotRegisteredHere) {
		return handoffTarget{}, fmt.Errorf("local transcript: %w; archive: %w", localErr, err)
	}
	return target, err
}

func (r handoffResolver) archiveByID(id string) (handoffTarget, error) {
	store, err := r.env.openStore(r.cfg)
	if err != nil {
		return handoffTarget{}, fmt.Errorf("open storage: %w", err)
	}
	key, err := locateMetadataKey(r.ctx, store, r.harness, id)
	if err != nil {
		return handoffTarget{}, err
	}
	return r.fromArchive(store, key, "")
}

func (r handoffResolver) fromArchive(store storage.ObjectStore, key, describe string) (handoffTarget, error) {
	metadata, bundle, err := reader.RefreshAndLoad(r.ctx, store, key, reader.Limits{})
	if err != nil {
		if errors.Is(err, reader.ErrRefreshRequired) {
			return handoffTarget{}, errors.New("the session's source bundle is not available (it may have just been replaced or deleted by retention); retry")
		}
		return handoffTarget{}, err
	}
	return analyzeTarget(r.ctx, parsersFor(r.env), handoffTarget{bundle: bundle, metadata: &metadata, source: "archive", describe: describe})
}

// latest resolves `--latest`. A session is for the project when it ran at
// this path (or inside it), or in a checkout of the same repository, which is
// how a session from another machine, or another clone, matches. Path matches
// come first and a repository-only match is considered only when there is no
// path match anywhere: a repository key is a claim a repository's own
// configuration or a writer of the archive can make, so it must never
// displace a session that matched by path. The order is this machine's path
// matches (most recently active first), the archive's path matches (most
// recently captured first), this machine's repository-only matches, the
// archive's, then an error listing recent sessions to choose from.
// --source archive skips this machine's; --source local skips the archive's.
func (r handoffResolver) latest(dir string) (handoffTarget, error) {
	dir = filepath.Clean(dir)
	now := r.env.now()
	var local localMatches
	var skipped []string
	if r.source != "archive" {
		regs, err := state.OpenReadOnly(r.home).LoadRegistrations()
		if err != nil {
			return handoffTarget{}, err
		}
		local = r.localCandidates(regs, dir)
		if target, found, err := r.firstLocal(local.byPath, now, &skipped); found || err != nil {
			return target, err
		}
	}
	var archived archiveMatches
	var archiveErr error
	if r.source != "local" {
		if archived, archiveErr = r.archiveCandidates(dir); archiveErr == nil {
			if target, found, err := r.firstArchive(archived, archived.byPath, now); found || err != nil {
				return target, err
			}
		}
	}
	if r.source != "archive" {
		if target, found, err := r.firstLocal(local.byRepo, now, &skipped); found || err != nil {
			return target, err
		}
		if r.source == "local" {
			message := fmt.Sprintf("no session for %s is registered on this machine with a readable transcript", dir)
			if len(skipped) > 0 {
				message += "; passed over: " + strings.Join(skipped, ", ")
			}
			return handoffTarget{}, errors.New(message)
		}
	}
	if archiveErr != nil {
		return handoffTarget{}, archiveErr
	}
	if target, found, err := r.firstArchive(archived, archived.byRepo, now); found || err != nil {
		return target, err
	}
	return handoffTarget{}, r.noMatch(dir, archived.sessions, now)
}

type localHandoffCandidate struct {
	reg    archive.SessionRegistration
	active time.Time
	// byRepo is set when the registration matched only by repository key.
	byRepo bool
}

// localMatches are the registrations that fit the project, by path and (only)
// by repository, each most recently active first.
type localMatches struct {
	byPath []localHandoffCandidate
	byRepo []localHandoffCandidate
}

// localCandidates reads activity only for registrations that match the
// requested project and harness. Sorting is stable for equal activity times.
// A registration that matches by path is never a repository-only match, even
// when its key matches too.
func (r handoffResolver) localCandidates(regs []archive.SessionRegistration, dir string) localMatches {
	var matches localMatches
	for _, reg := range regs {
		if reg.IsChild() || reg.SubagentID != "" || reg.Replay != nil || r.skip[handoffSessionKey(reg.Harness.Name, reg.NativeSessionID)] {
			continue
		}
		byPath := sameProject(reg.ProjectRoot, dir)
		byRepo := !byPath && r.repoKey != "" && reg.RepoKey == r.repoKey
		if !byPath && !byRepo {
			continue
		}
		if r.harness != "" && reg.Harness.Name != r.harness {
			continue
		}
		active := reg.RegisteredAt
		if at, ok := collector.LastActivity(r.ctx, reg, r.env.cursorDatabase(), registryFor(r.env)); ok {
			active = at
		}
		c := localHandoffCandidate{reg: reg, active: active, byRepo: byRepo}
		if byRepo {
			matches.byRepo = append(matches.byRepo, c)
		} else {
			matches.byPath = append(matches.byPath, c)
		}
	}
	for _, list := range [][]localHandoffCandidate{matches.byPath, matches.byRepo} {
		sort.SliceStable(list, func(i, j int) bool { return list[i].active.After(list[j].active) })
	}
	return matches
}

// firstLocal reads candidates in order and returns the first with a readable
// transcript and a prompt; the others are passed over, and named in skipped
// when their failure is worth reporting. A repository-only candidate is put to
// r.gate first, which may end the search with an error.
func (r handoffResolver) firstLocal(candidates []localHandoffCandidate, now time.Time, skipped *[]string) (handoffTarget, bool, error) {
	for _, c := range candidates {
		// Untrusted locator text cannot become a filename or a diagnostic before
		// the existing bounded repository-only refusal has been offered.
		if c.byRepo && !wellFormedID.MatchString(c.reg.ArchiveSessionID) {
			if err := r.acceptRepoMatch(localRepoMatch(c.reg, archive.Analysis{})); err != nil {
				return handoffTarget{}, false, err
			}
		}
		target, err := r.localTarget(c.reg)
		if err == nil && !hasPrompt(target.analysis) {
			err = errors.New("no prompt yet")
		}
		if err != nil {
			if !errors.Is(err, collector.ErrNoTranscript) {
				*skipped = append(*skipped, fmt.Sprintf("%s (%v)", shownID(c.reg.ArchiveSessionID), err))
			}
			continue
		}
		if c.byRepo && wellFormedID.MatchString(c.reg.ArchiveSessionID) {
			if err := r.acceptRepoMatch(localRepoMatch(c.reg, target.analysis)); err != nil {
				return handoffTarget{}, false, err
			}
		}
		target.describe = fmt.Sprintf("%s session %s, active %s (%s)", cappedLine(c.reg.Harness.Name, 20), shownID(c.reg.ArchiveSessionID), relativeAge(now, c.active), machineThis)
		return target, true, nil
	}
	return handoffTarget{}, false, nil
}

// localRepoMatch describes a registration on this machine that matched only by
// repository, from what the registration and its transcript say.
func localRepoMatch(reg archive.SessionRegistration, analysis archive.Analysis) repoMatch {
	project := ""
	if reg.ProjectRoot != "" {
		project = filepath.Base(filepath.Clean(reg.ProjectRoot))
	}
	labels, _ := archive.LabelsFromAnalysis(analysis)
	return repoMatch{id: reg.ArchiveSessionID, machine: machineThis, started: reg.SessionStartedAt, project: project, title: labels.Title}
}

// archiveMatches are the archive's top-level sessions, newest capture first,
// and those that fit the project by path and (only) by repository.
type archiveMatches struct {
	store      storage.ObjectStore
	sessions   []archive.Metadata
	projectIDs map[string]bool
	byPath     []archive.Metadata
	byRepo     []archive.Metadata
}

// archiveCandidates lists the archive's metadata, without reading any session's
// source.
func (r handoffResolver) archiveCandidates(dir string) (archiveMatches, error) {
	store, err := r.env.openStore(r.cfg)
	if err != nil {
		return archiveMatches{}, fmt.Errorf("no local session for %s, and the archive could not be opened: %w", dir, err)
	}
	sessions, err := reader.ListMetadataWithOptions(r.ctx, store, archiveSessionsPrefix, reader.Filter{Harness: r.harness, Replays: reader.ReplaysHidden}, reader.ListOptions{Cache: listCache(r.env, false), Skipped: warnSkippedSidecar(r.stderr, "handoff")})
	if err != nil {
		return archiveMatches{}, err
	}
	sessions = topLevelSessions(sessions)
	sortByActivity(sessions)
	projectIDs := r.projectIDs(dir)
	byPath, byRepo := archiveHandoffCandidates(sessions, projectIDs, r.repoKey, r.skip)
	return archiveMatches{store: store, sessions: sessions, projectIDs: projectIDs, byPath: byPath, byRepo: byRepo}, nil
}

// firstArchive downloads the first of candidates whose metadata is usable. A
// repository-only candidate is put to r.gate first, from its metadata alone:
// nothing of the session's source is read until it is accepted.
func (r handoffResolver) firstArchive(a archiveMatches, candidates []archive.Metadata, now time.Time) (handoffTarget, bool, error) {
	for _, m := range candidates {
		key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
		if err != nil {
			continue
		}
		machine := r.machineLabel(m.MachineID)
		if !a.projectIDs[m.ProjectID] {
			match := repoMatch{id: m.SessionID, machine: machine, started: m.StartedAt, project: m.ProjectName, title: m.Title}
			if err := r.acceptRepoMatch(match); err != nil {
				return handoffTarget{}, false, err
			}
		}
		describe := fmt.Sprintf("%s session %s, captured %s (%s)", cappedLine(m.Harness.Name, 20), shownID(m.SessionID), relativeAge(now, m.CapturedAt), machine)
		target, err := r.fromArchive(a.store, key, describe)
		return target, true, err
	}
	return handoffTarget{}, false, nil
}

// archiveHandoffCandidates is the selection policy; its inputs are already
// loaded and sorted, so it can be checked without storage or a transcript. A
// session matches by path when its project ID is one of projectIDs, and
// otherwise by repository when repoKey is not empty and is its repository
// key (the same repository, wherever it was checked out). Each list keeps the
// order given; a session that matches by path is never in byRepo.
func archiveHandoffCandidates(sessions []archive.Metadata, projectIDs map[string]bool, repoKey string, skip map[agentmeta.SessionKey]bool) (byPath, byRepo []archive.Metadata) {
	for _, m := range sessions {
		if m.IsReplay() || skip[handoffSessionKey(m.Harness.Name, m.NativeSessionID)] || (m.Counts.Turns != nil && *m.Counts.Turns == 0) {
			continue
		}
		switch {
		case projectIDs[m.ProjectID]:
			byPath = append(byPath, m)
		case repoKey != "" && m.RepoKey == repoKey:
			byRepo = append(byRepo, m)
		}
	}
	return byPath, byRepo
}

// sameProject reports whether dir belongs to the project at root: it is the
// root or inside it, compared both as written and with symlinks resolved
// (macOS's /var is /private/var). A root inside dir does not count, so
// running from a parent directory such as ~ does not pick up every project
// beneath it.
func sameProject(root, dir string) bool {
	if root == "" || dir == "" {
		return false
	}
	for _, a := range pathForms(root) {
		for _, b := range pathForms(dir) {
			if local.PathWithin(b, a) {
				return true
			}
		}
	}
	return false
}

func pathForms(path string) []string {
	clean := filepath.Clean(path)
	if resolved, err := local.ResolveExistingSymlinks(clean); err == nil && resolved != clean {
		return []string{clean, filepath.Clean(resolved)}
	}
	return []string{clean}
}

// projectIDs returns the archive project IDs dir can belong to: the
// configured project whose root contains it, and dir's own ID.
func (r handoffResolver) projectIDs(dir string) map[string]bool {
	return archiveProjectIDs(r.cfg, dir)
}

func topLevelSessions(sessions []archive.Metadata) []archive.Metadata {
	out := sessions[:0:0]
	for _, m := range sessions {
		if !m.IsChild() {
			out = append(out, m)
		}
	}
	return out
}

// subagentSessions is the subagent sessions among sessions: those with a
// parent.
func subagentSessions(sessions []archive.Metadata) []archive.Metadata {
	out := sessions[:0:0]
	for _, m := range sessions {
		if m.IsChild() {
			out = append(out, m)
		}
	}
	return out
}

// machineThis and machineOther say whose machine captured a session.
const (
	machineThis  = "this machine"
	machineOther = "another machine"
)

func (r handoffResolver) machineLabel(machineID string) string {
	if machineID != "" && machineID == r.cfg.MachineID {
		return machineThis
	}
	return machineOther
}

// noMatch builds the --latest failure, listing the most recent archived
// sessions from metadata only so one can be picked directly.
func (r handoffResolver) noMatch(dir string, sessions []archive.Metadata, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "no session for %s on this machine or in the archive", dir)
	if r.harness != "" {
		fmt.Fprintf(&b, " (harness %s)", r.harness)
	}
	if len(sessions) == 0 {
		b.WriteString(".\nThe archive has no sessions yet; run `agent-archive list` after the next sync.")
		return errors.New(b.String())
	}
	// Say what was tried, and how to make a session from another machine
	// match: by repository when the directory has one, else by path.
	if r.repoKey != "" {
		b.WriteString(".\nTried this directory's repository (its remote origin), then its path. A session\nmatches when it ran in a checkout of the same origin or at this path.")
	} else {
		b.WriteString(".\nTried this directory's path only: it is not in a git repository with a remote\nnamed origin, so it cannot match by repository. Matching a session from another\nmachine by repository needs that remote (see `git remote -v`); without it, the\nrepository must be at the same path.")
	}
	if r.repoKey != "" {
		b.WriteString("\nSessions captured before repository keys (parser 0.16.0) carry none until they are\nrefreshed on the Mac that captured them.")
	}
	b.WriteString(" Recent archived sessions:\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for i, m := range sessions {
		if i == handoffFallbackRows {
			break
		}
		// The table is built in memory, where writes cannot fail.
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", cappedLine(m.Harness.Name, 20), relativeAge(now, lastActivity(m)), r.machineLabel(m.MachineID), handoffCommandFor(m.SessionID))
	}
	_ = tw.Flush()
	return errors.New(strings.TrimRight(b.String(), "\n"))
}

// relativeAge renders how long ago t was, coarsely.
func relativeAge(now, t time.Time) string {
	if t.IsZero() {
		return "at an unknown time"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	default:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// handoffFullPath names where the untrimmed rendering of a bundle is saved.
func handoffFullPath(home string, bundle archive.SourceBundle, format string) string {
	ext := ".md"
	if format == "json" {
		ext = ".json"
	}
	return filepath.Join(home, handoffDir, handoffFileName(bundle)+ext)
}

// handoffFileName is the file name stem for a bundle's saved handoffs. The
// archive session ID is already a safe file component; anything else is
// hashed.
func handoffFileName(bundle archive.SourceBundle) string {
	name := bundle.ArchiveSessionID
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		sum := sha256.Sum256([]byte(bundle.ArchiveSessionID + "\x00" + bundle.NativeSessionID))
		name = hex.EncodeToString(sum[:])[:16]
	}
	return name
}

// launchHandoffPrefix begins the name of each launch copy's directory under
// handoffDir.
const launchHandoffPrefix = "launch-"

// pruneHandoffs deletes saved handoffs, and launch copies' directories,
// older than handoffMaxAge. It is best effort: a failure leaves them for the
// next run.
func pruneHandoffs(home string, now time.Time) {
	dir := filepath.Join(home, handoffDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), launchHandoffPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) <= handoffMaxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
	}
}

// writeHandoffOutput writes the rendered handoff with mode 0600, refusing to
// replace an existing file unless force is set.
func writeHandoffOutput(path string, data []byte, force bool) error {
	err := writeStatsHTMLFile(path, data, force)
	// Report the requested destination, rather than a private temporary name.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return &os.PathError{Op: pathErr.Op, Path: path, Err: pathErr.Err}
	}
	return err
}
