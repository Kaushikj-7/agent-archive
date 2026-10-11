package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// handoffTitleScanLimit is how many of this machine's sessions, newest activity
// first, a title search reads. A local session's title is its first prompt,
// which takes reading its transcript, so the search is bounded as the picker
// is; a session past it that was uploaded is found in the archive instead.
const handoffTitleScanLimit = defaultListLimit

// archiveSessionIDLength is the length of an archive session ID, 32 lower-case
// hexadecimal digits: the one shape worth a direct archive read before
// searching titles (as show does), so a title as long is never sent there.
const archiveSessionIDLength = 32

// handoffQueryWidth is how many columns of the query a message repeats, so a
// pasted paragraph is not echoed back in full.
const handoffQueryWidth = 80

func isArchiveSessionID(query string) bool {
	if len(query) != archiveSessionIDLength {
		return false
	}
	for _, c := range query {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// queryLabel is the query as a message names it: one printable line, cut to
// handoffQueryWidth.
func queryLabel(query string) string {
	return ellipsize(archive.DisplayLine(query), handoffQueryWidth)
}

// skippedSessions is the calling agent's own session, which a title never
// offers: the person names another. A direct handoff (--to) starts from it.
func skippedSessions(opts *handoffOptions, env currentSessionDependencies) map[agentmeta.SessionKey]bool {
	if opts.to != "" {
		return nil
	}
	return currentSessions(env)
}

// handoffCandidateLimit is how many matching sessions a title lists on stderr
// before saying how many more there are. The caller may be
// an agent, whose context a title as common as "fix" must not flood.
const handoffCandidateLimit = 20

// resolveHandoffQuery turns the positional argument, a session ID or words,
// into the one session to hand off, replacing opts.sessionID (and opts.harness
// when it was not given) with that session's archive ID and harness. Words
// match as list and show match them (sessionQuery: every word in some field of
// a session, or the start of its ID, an exact ID winning), over metadata,
// never transcript content:
//
//  1. a session ID registered on this machine (a subagent's too, as handoff
//     always took), then, for a full ID, the archive (one read), which a
//     title match may not shadow;
//  2. this machine's sessions, which need no network;
//  3. only when none of those match, the archive's.
//
// Inside a repository (or with --project), the words look at that scope's
// top-level sessions first: this machine's, then the archive's, and only when none
// match there, everywhere; a note on stderr says how many more match outside
// the scope. Subagents are the last tier, in the scope and then everywhere.
//
// Several matches print the candidates to stderr and exit 1 without a
// terminal, and open the handoff picker limited to them with one. done is set
// when the command should exit with code instead of handing off.
func resolveHandoffQuery(opts *handoffOptions, home string, interactive bool, in *typedInput, stdout, stderr io.Writer, env handoffCommandDependencies) (code int, done bool) {
	// Titles are stored as one line of single spaces, so a query with a
	// newline or a doubled space is matched as that line.
	query := strings.Join(strings.Fields(opts.sessionID), " ")
	cfg, found, err := handoffConfig(home, *opts)
	if err != nil || !found {
		// resolveHandoffTarget reports both.
		return 0, false
	}
	r := handoffQueryResolver{opts: opts, query: query, q: parseSessionQuery(query), labels: projectLabels(cfg), skip: skippedSessions(opts, env), home: home, cfg: cfg, interactive: interactive, in: in, stdout: stdout, stderr: stderr, env: env, knownAgents: agentmeta.Names(catalogFor(env))}
	code, done = r.resolve()
	if done {
		return code, true
	}
	// The ID names a registration file and bucket keys and, however it was
	// found, may have come from stored data: the parse no longer vets it.
	harness := opts.harness
	if harness == "" {
		harness = archive.HarnessClaude
	}
	if _, err := archive.MetadataObjectKey(harness, opts.sessionID); err != nil {
		terminal.Printf(stderr, "agent-archive: handoff: the matching session has an unusable ID: %v\n", err)
		return 1, true
	}
	return 0, false
}

type handoffQueryResolver struct {
	knownAgents []string
	opts        *handoffOptions
	query       string
	// q is the query's words, and labels the configured project names the
	// matcher reads.
	q           sessionQuery
	labels      map[string]string
	home        string
	cfg         config.Config
	interactive bool
	in          *typedInput
	stdout      io.Writer
	stderr      io.Writer
	env         handoffCommandDependencies

	// skip holds the qualified identities of the agent session running the command,
	// which a title does not offer (as --latest passes over it).
	skip  map[agentmeta.SessionKey]bool
	store storage.ObjectStore
	// scope is where the title looks first.
	scope sessionScope
	// archive is the archive's top-level sessions, read once (archiveRead).
	archive []handoffPickerRow
	// replays are available to explicit short IDs, never to title search.
	replays []handoffPickerRow
	// subagents are the archive's subagent sessions, read with archive.
	subagents   []handoffPickerRow
	archiveRead bool
	// inLabel names the scope the matches are in, when they are.
	inLabel string
	// archiveErr is why the archive could not be read, for the no-match
	// message.
	archiveErr error
}

func (r *handoffQueryResolver) resolve() (code int, done bool) {
	ctx := context.Background()
	opts := r.opts
	var regs []archive.SessionRegistration
	if opts.source != "archive" {
		var err error
		if regs, err = state.OpenReadOnly(r.home).LoadRegistrations(); err != nil {
			terminal.Printf(r.stderr, "agent-archive: handoff: note: could not read this machine's sessions, searching the archive only: %v\n", err)
		}
		for _, reg := range regs {
			if reg.ArchiveSessionID == r.query && (opts.harness == "" || archive.CanonicalHarness(reg.Harness.Name) == opts.harness) {
				opts.sessionID = r.query
				return 0, false
			}
		}
	}
	if opts.source != "local" && isArchiveSessionID(r.query) {
		readCtx, stopReads := readCommandContext(r.env)
		harness, ok := r.exactArchiveSession(readCtx)
		stopReads()
		if ok {
			opts.sessionID, opts.harness = r.query, harness
			return 0, false
		}
	}
	// Only a title needs the scope, and looking it up can run git.
	var err error
	if r.scope, err = scopeFor(r.env, opts.project, opts.allProjects); err != nil {
		terminal.Printf(r.stderr, "agent-archive: handoff: %v\n", err)
		return 1, true
	}
	scanned := false
	var local []handoffPickerRow
	if opts.source != "archive" {
		stop := startActivity(r.stdout, "Finding sessions…")
		picker := handoffPicker{ctx: ctx, env: r.env, home: r.home, harness: opts.harness, source: "local"}
		rows, _, truncated := picker.rows(regs, nil, handoffTitleScanLimit)
		stop()
		local, scanned = rows, truncated
	}
	// The picker hides replays, but the short ID list displays still names
	// one explicitly. Its identity needs no transcript read to resolve.
	for _, reg := range regs {
		if reg.Replay != nil && (opts.harness == "" || archive.CanonicalHarness(reg.Harness.Name) == opts.harness) {
			local = append(local, handoffPickerRow{metadata: archive.Metadata{
				SessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID,
				Harness: reg.Harness, Replay: reg.Replay,
			}})
		}
	}
	if exact := r.exactID(local); len(exact) > 0 {
		return r.choose(exact)
	}
	if r.scope.narrowed() {
		if code, done, found := r.inScope(local); found {
			return code, done
		}
	}
	if opts.source != "archive" {
		if matches := r.match(local); len(matches) > 0 {
			return r.choose(matches)
		}
	}
	if opts.source != "local" {
		if matches := r.match(r.archiveRows()); len(matches) > 0 {
			return r.choose(matches)
		}
		// Subagents are the last tier: in the scope, then everywhere.
		if r.scope.narrowed() {
			if matches := r.match(r.within(r.subagents)); len(matches) > 0 {
				r.inLabel = r.scope.Label
				return r.choose(matches)
			}
		}
		if matches := r.match(r.subagents); len(matches) > 0 {
			return r.choose(matches)
		}
	}
	terminal.Println(r.stderr, r.noMatchMessage(scanned))
	return 1, true
}

// inScope settles on the matches in the scope, this machine's before the
// archive's, and says how many more match outside it. found is false when
// nothing in the scope matches, and the search goes on everywhere; otherwise
// code and done are choose's.
func (r *handoffQueryResolver) inScope(local []handoffPickerRow) (code int, done, found bool) {
	matches := r.match(r.within(local))
	if len(matches) == 0 && r.opts.source != "local" {
		matches = r.match(r.within(r.archiveRows()))
	}
	if len(matches) == 0 {
		return 0, false, false
	}
	// The count covers what was searched. A title answered on this machine did not
	// ask the archive, which needs the network, so its other matches are not in it.
	seen := map[string]bool{}
	outside := 0
	for _, rows := range [][]handoffPickerRow{local, r.archive} {
		for _, row := range r.match(rows) {
			key := handoffRowKey(row.metadata)
			if !seen[key] && !r.scope.contains(row.metadata, row.reg) {
				outside++
			}
			seen[key] = true
		}
	}
	if outside > 0 {
		terminal.Println(r.stderr, outsideNote(len(matches), outside, r.scope.Label))
	}
	r.inLabel = r.scope.Label
	code, done = r.choose(matches)
	return code, done, true
}

// exactID is the sessions whose ID is the query's one word, whole or as the
// short ID a table shows, before any tier, as list and show find them: an
// exact ID wins outright, over a title in the scope that happens to contain
// it. This machine's sessions are looked at first; the archive (its subagents
// too) only for a word shaped like a short ID, so other words still need no
// network when this machine answers them.
func (r *handoffQueryResolver) exactID(local []handoffPickerRow) []handoffPickerRow {
	if len(r.q.words) != 1 {
		return nil
	}
	exact := func(rows []handoffPickerRow) []handoffPickerRow {
		rows = slices.DeleteFunc(slices.Clone(rows), func(row handoffPickerRow) bool {
			return r.skip[handoffSessionKey(row.metadata.Harness.Name, row.metadata.NativeSessionID)]
		})
		return exactIDWins(rows, r.q, func(row handoffPickerRow) sessionFields { return sessionFields{SessionID: row.metadata.SessionID} })
	}
	if found := exact(local); len(found) > 0 {
		return found
	}
	word := r.q.words[0]
	if r.opts.source == "local" || len(word) != minShortSessionID || strings.Trim(word, "0123456789abcdef") != "" {
		return nil
	}
	// archiveRows reads r.subagents too, so it runs before they are read.
	top := r.archiveRows()
	return exact(append(append(slices.Clone(top), r.subagents...), r.replays...))
}

// within keeps the rows in the scope.
func (r *handoffQueryResolver) within(rows []handoffPickerRow) []handoffPickerRow {
	return slices.DeleteFunc(slices.Clone(rows), func(row handoffPickerRow) bool { return !r.scope.contains(row.metadata, row.reg) })
}

// match keeps the rows the query matches, in order, leaving out the sessions
// whose qualified identity is in skip and archived ones with no prompt, which the
// picker does not offer either (an ID still names those: exactID). An exact
// ID wins outright.
func (r *handoffQueryResolver) match(rows []handoffPickerRow) []handoffPickerRow {
	rows = slices.DeleteFunc(slices.Clone(rows), func(row handoffPickerRow) bool {
		return row.metadata.IsReplay() || r.skip[handoffSessionKey(row.metadata.Harness.Name, row.metadata.NativeSessionID)] || archivedWithoutPrompt(row.metadata)
	})
	return matchPool(rows, r.q, func(row handoffPickerRow) sessionFields {
		return fieldsOf(row.metadata, sessionProjectName(row.metadata, r.labels))
	})
}

// openArchive opens the archive's store once.
func (r *handoffQueryResolver) openArchive() (storage.ObjectStore, error) {
	if r.store != nil {
		return r.store, nil
	}
	store, err := r.env.openStore(r.cfg)
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	r.store = store
	return store, nil
}

// exactArchiveSession reads the archive for the query as a full session ID.
// A miss, and an archive that cannot be read, both leave the title search to
// go on.
func (r *handoffQueryResolver) exactArchiveSession(ctx context.Context) (harness string, ok bool) {
	store, err := r.openArchive()
	if err != nil {
		r.archiveErr = err
		return "", false
	}
	harnesses := r.knownAgents
	if r.opts.harness != "" {
		harnesses = []string{r.opts.harness}
	}
	var found []string
	for _, name := range harnesses {
		key, err := archive.MetadataObjectKey(name, r.query)
		if err != nil {
			return "", false
		}
		if _, err := store.Get(ctx, key); err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				r.archiveErr = err
				return "", false
			}
			continue
		}
		found = append(found, name)
	}
	// The same ID under several harnesses is not guessed; the title search
	// reports whatever else matches.
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// archiveRows is the archive's top-level sessions, read once, which also reads
// its subagents (r.subagents). An archive
// that cannot be read is none, with the reason kept for the no-match message
// (or that of r.opts.source "local", which never reads it).
func (r *handoffQueryResolver) archiveRows() []handoffPickerRow {
	if r.archiveRead || r.opts.source == "local" {
		return r.archive
	}
	r.archiveRead = true
	store, err := r.openArchive()
	if err != nil {
		r.archiveErr = err
		return nil
	}
	stop := startActivity(r.stdout, "Finding sessions…")
	sessions, err := loadSessionsForBrowse(r.env, store, listOptions{filter: reader.Filter{Harness: r.opts.harness}}, r.stderr, "handoff")
	stop()
	if err != nil {
		r.archiveErr = err
		return nil
	}
	for _, m := range sessions {
		row := handoffPickerRow{metadata: m, active: lastActivity(m)}
		if m.IsReplay() {
			r.replays = append(r.replays, row)
			continue
		}
		if !m.IsChild() {
			r.archive = append(r.archive, row)
		} else {
			r.subagents = append(r.subagents, row)
		}
	}
	return r.archive
}

func handoffRowKey(m archive.Metadata) string {
	return m.Harness.Name + "/" + m.SessionID
}

// choose settles on one of several or one matching row.
func (r *handoffQueryResolver) choose(matches []handoffPickerRow) (code int, done bool) {
	row := matches[0]
	if len(matches) > 1 {
		// A parent's hint counts the archive's subagents, when it was read.
		subagents := make([]archive.Metadata, len(r.subagents))
		for i, sub := range r.subagents {
			subagents[i] = sub.metadata
		}
		format := listFormatOptions{Now: r.env.now(), Projects: projectLabels(r.cfg), Children: childCounts(subagents)}
		if !r.interactive {
			r.printCandidates(matches, format)
			return 1, true
		}
		format.Style, format.GroupByProject, format.Numbered = styleFor(r.stdout), true, true
		rows := formatHandoffRows(matches, format)
		picked, selected, code := runBrowser(context.Background(), r.env, r.in.prompter(r.stdout), r.stdout, r.stderr, browserSpec{Mode: pickSession, Verb: "Hand off", Choices: rowChoices(rows, format), Query: r.query, Command: "handoff"})
		if code != 0 {
			return code, true
		}
		if !selected {
			return 0, true
		}
		i := slices.IndexFunc(matches, func(m handoffPickerRow) bool {
			return m.metadata.SessionID == picked.SessionID && m.metadata.Harness.Name == picked.HarnessKey
		})
		if i < 0 {
			// Never a guess: the picked row is one of those listed.
			terminal.Println(r.stderr, "agent-archive: handoff: the picked session is not one of those listed")
			return 1, true
		}
		row = matches[i]
	}
	r.opts.sessionID, r.opts.harness = row.metadata.SessionID, row.metadata.Harness.Name
	return 0, false
}

// printCandidates lists ambiguous matches for a caller with no terminal to
// pick on: a person reading a log, or an agent that must ask which.
func (r *handoffQueryResolver) printCandidates(matches []handoffPickerRow, format listFormatOptions) {
	shown := matches[:min(len(matches), handoffCandidateLimit)]
	var listFlags []string
	if r.opts.harness != "" {
		listFlags = append(listFlags, "--harness "+r.opts.harness)
	}
	if r.opts.allProjects {
		listFlags = append(listFlags, "--all-projects")
	}
	if r.opts.project != "" {
		listFlags = append(listFlags, "--project "+shellWord(r.opts.project))
	}
	candidateList{
		command: "handoff", query: r.query, label: r.inLabel, total: len(matches), rows: formatHandoffRows(shown, format), listFlags: listFlags,
		next: func(row listRow) string {
			return fmt.Sprintf("agent-archive handoff %s --harness %s", archive.DisplayLine(row.ShortID), archive.DisplayLine(row.HarnessKey))
		},
	}.print(r.stderr)
}

func (r *handoffQueryResolver) noMatchMessage(scanLimited bool) string {
	where, ok := map[string]string{"local": "on this machine", "archive": "in the archive"}[r.opts.source]
	if !ok {
		where = "on this machine or in the archive"
	}
	message := fmt.Sprintf("agent-archive: handoff: no session matches %q %s (see `agent-archive list`)", queryLabel(r.query), where)
	if r.opts.harness != "" {
		message += " for " + r.opts.harness
	}
	if scanLimited {
		message += fmt.Sprintf("; only this machine's %d most recently active sessions were searched", handoffTitleScanLimit)
	}
	if r.archiveErr != nil {
		message += fmt.Sprintf("\nagent-archive: handoff: note: the archive could not be read, so only this machine's sessions were searched: %v", r.archiveErr)
	}
	return message
}
