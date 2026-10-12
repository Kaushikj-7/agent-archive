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
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// showLookup is the result of resolving a show argument to one session.
type showLookup struct {
	SessionID string
	Harness   string
	Metadata  reader.MetadataLookup
	// Cancelled is set when the user quit the browser opened on several
	// matches without choosing a session, or browsed them (it showed the
	// session itself): the caller has nothing left to read, and exits 0.
	Cancelled bool
}

// Catalog identities have a bounded canonical key; longer safe text remains
// searchable as a title. Legacy full IDs retain their established direct path.
func showExactIDCandidate(store storage.ObjectStore, harness, query string) bool {
	if len(query) < 32 {
		return false
	}
	if harness == "" {
		harness = "probe"
	}
	key, err := archive.MetadataObjectKey(harness, query)
	return err == nil && (!reader.CatalogAuthority(store) || len(key) <= 1024)
}

// resolveShowQuery turns a show argument into a session. Exact SESSION_ID
// lookups win. Otherwise the argument is words, combining identity prefixes
// with text matches (sessionQuery) over every archived sidecar, in the tiers of the search: the
// working directory's repository first, then every project, subagents last.
// Several matches on a terminal open the browser over them, with the words in
// its filter: it shows the sessions' details itself (and the lookup is
// Cancelled, nothing left for the caller to read), unless pickOne is set
// because the caller has something to do with one session (--transcript or
// --json), when it returns the chosen session. Off a terminal they print the
// candidates and exit 1.
func resolveShowQuery(ctx context.Context, store storage.ObjectStore, env showQueryDependencies, stdin io.Reader, stdout, stderr io.Writer, harness, query string, cfgProjects map[string]string, noPager, pickOne bool) (showLookup, int) {
	// Full archive IDs use the direct-read path. With --harness, a short ID
	// or title would otherwise be mistaken for a literal object key.
	if showExactIDCandidate(store, harness, query) {
		lookup, err := locateShowMetadata(ctx, store, harness, query)
		if err == nil {
			parts := strings.Split(strings.TrimPrefix(lookup.Key, archiveSessionsPrefix+"/"), "/")
			if len(parts) >= 2 {
				return showLookup{SessionID: parts[1], Harness: parts[0], Metadata: lookup}, 0
			}
			return showLookup{SessionID: query, Harness: harness, Metadata: lookup}, 0
		}
		// Exact-id misses fall through to short-id / title search. Keep
		// ambiguous harnesses and storage failures as errors.
		miss := !errors.Is(err, reader.ErrInvalidMetadata) && (errors.Is(err, storage.ErrNotFound) || strings.Contains(err.Error(), "no archived session") ||
			strings.Contains(err.Error(), "invalid archive session ID"))
		if !miss {
			terminal.Printf(stderr, "agent-archive: show: %v\n", err)
			return showLookup{}, 1
		}
	}

	scope, err := scopeFor(env, "", false)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return showLookup{}, 1
	}
	// Keep complete summaries for child counts, scope tiers and ambiguity.
	stopSearch := startActivity(stdout, "Finding sessions…")
	candidates, err := readShowCandidates(ctx, store, env, harness, query, stderr)
	stopSearch()
	defer candidates.close()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return showLookup{}, 1
	}
	sessions := candidates.sessions
	sortByActivity(sessions)
	// Search every listed sidecar — do not apply list's --limit window, or
	// older title matches would be silently invisible.
	q := parseSessionQuery(query)
	fields := func(m archive.Metadata) sessionFields {
		return fieldsOf(m, sessionProjectName(m, cfgProjects))
	}
	// A replay opens only by its ID, never through a search of titles and
	// names, as it stays out of list and handoff's pickers.
	sessions = slices.DeleteFunc(sessions, func(m archive.Metadata) bool {
		return m.IsReplay() && len(exactIDWins([]archive.Metadata{m}, q, fields)) == 0
	})
	found := searchShowSessions(sessions, q, scope, fields)
	matches := found.matches
	if note := found.outsideNote(scope); note != "" {
		terminal.Println(stderr, note)
	}
	switch len(matches) {
	case 0:
		terminal.Printf(stderr, "agent-archive: show: no archived session %q (see `agent-archive list`)\n", archive.DisplayLine(query))
		return showLookup{}, 1
	case 1:
		lookup, err := candidates.lookup(ctx, matches[0])
		if err != nil {
			terminal.Printf(stderr, "agent-archive: show: %v\n", err)
			return showLookup{}, 1
		}
		return lookup, 0
	}
	format := listFormatOptions{Now: env.now(), Projects: cfgProjects, Children: childCounts(sessions)}
	if !browseInteractive(env, stdin, stdout) {
		shown := matches[:min(len(matches), handoffCandidateLimit)]
		label := ""
		if found.inScope {
			label = scope.Label
		}
		var listFlags []string
		if harness != "" {
			listFlags = append(listFlags, "--harness "+harness)
		}
		candidateList{
			command: "show", query: query, label: label, total: len(matches), rows: formatSessionRows(shown, format), listFlags: listFlags,
			next: func(row listRow) string { return "agent-archive show " + archive.DisplayLine(row.ShortID) },
		}.print(stderr)
		return showLookup{}, 1
	}
	format.Style, format.GroupByProject, format.Numbered = styleFor(stdout), true, true
	choices, words := rowChoices(formatSessionRows(matches, format), format), strings.Join(strings.Fields(query), " ")
	spec := browserSpec{Mode: pickSession, Verb: "Show", Choices: choices, Query: words, Command: "show"}
	if !pickOne {
		spec = browserSpec{Mode: browseSessions, Choices: choices, Query: words, Command: "show", Store: store, NoPager: noPager}
	}
	row, picked, code := runBrowser(ctx, env, newPrompter(stdin, stdout), stdout, stderr, spec)
	if code != 0 {
		return showLookup{}, code
	}
	if !picked {
		return showLookup{Cancelled: true}, 0
	}
	for _, metadata := range matches {
		if metadata.SessionID == row.SessionID && metadata.Harness.Name == row.HarnessKey {
			lookup, err := candidates.lookup(ctx, metadata)
			if err != nil {
				terminal.Printf(stderr, "agent-archive: show: %v\n", err)
				return showLookup{}, 1
			}
			return lookup, 0
		}
	}
	return showLookup{SessionID: row.SessionID, Harness: row.HarnessKey}, 0
}

// shortSessionID is the first minShortSessionID bytes of id, cut on a
// character boundary.
func shortSessionID(id string) string {
	return archive.TruncateUTF8(id, minShortSessionID)
}

func showMetadataLookup(metadata archive.Metadata) showLookup {
	key, _ := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	return showLookup{SessionID: metadata.SessionID, Harness: metadata.Harness.Name, Metadata: reader.MetadataLookup{Key: key, Metadata: metadata}}
}

var showMetadataFinder = reader.NewMetadataFinder(agentmeta.Builtins())

func locateShowMetadata(ctx context.Context, store storage.ObjectStore, harness, sessionID string) (reader.MetadataLookup, error) {
	if harness != "" {
		key, err := archive.MetadataObjectKey(harness, sessionID)
		if err != nil {
			return reader.MetadataLookup{}, err
		}
		metadata, err := reader.ReadMetadata(ctx, store, key)
		return reader.MetadataLookup{Key: key, Metadata: metadata}, err
	}
	lookups, err := showMetadataFinder.FindMetadata(ctx, store, archiveSessionsPrefix, sessionID)
	if len(lookups) > 1 {
		harnesses := make([]string, 0, len(lookups))
		for _, lookup := range lookups {
			harnesses = append(harnesses, strings.Split(strings.TrimPrefix(lookup.Key, archiveSessionsPrefix+"/"), "/")[0])
		}
		return reader.MetadataLookup{}, fmt.Errorf("session %q exists under more than one harness (%s); pass --harness", sessionID, strings.Join(harnesses, ", "))
	}
	if err != nil {
		return reader.MetadataLookup{}, err
	}
	if len(lookups) == 0 {
		return reader.MetadataLookup{}, fmt.Errorf("no archived session %q (see `agent-archive list`)", sessionID)
	}
	return lookups[0], nil
}
