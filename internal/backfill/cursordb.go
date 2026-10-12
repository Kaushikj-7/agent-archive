package backfill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"unicode/utf8"
)

// CursorDatabaseChat is what the plan lists of one Cursor chat in Cursor's
// database: its ID, to leave out chats with a transcript on disk and to ask
// the archive about it, its start, and where its project may be named. The
// plan reads the chat whole (CursorDatabaseResult.ReadChat) only when it may
// be imported. Nothing here is ever printed.
type CursorDatabaseChat = agentapi.DatabaseChat

// CursorUncheckedReason says why Cursor's database was not checked.
type CursorUncheckedReason = cursorstore.Reason

const (
	// CursorUncheckedLocked means a rollback journal shows an unfinished
	// write (which may be a hot journal only Cursor can roll back) or that
	// Cursor held a lock past the busy timeout.
	CursorUncheckedLocked = cursorstore.Locked
	// CursorUncheckedUnreadable means the file is not a database SQLite can
	// open, or its side files are in a state that can't be read without
	// changing them.
	CursorUncheckedUnreadable = cursorstore.Unreadable
	// CursorUncheckedUnknownFormat means the table, a composerData value, or
	// its _v is not a shape this release knows.
	CursorUncheckedUnknownFormat = cursorstore.UnknownFormat
	// CursorUncheckedChangedDuringRead means Cursor wrote the file while it
	// was read in place with Cursor closed.
	CursorUncheckedChangedDuringRead = cursorstore.ChangedDuringRead
	// CursorUncheckedTranscriptsUnreadable means part of Cursor's transcript
	// store could not be listed, so a chat with a transcript can't be told
	// apart from one stored only in the database. The database is not opened.
	CursorUncheckedTranscriptsUnreadable CursorUncheckedReason = "transcripts_unreadable"
)

// CursorDatabaseResult is one read of Cursor's database. Chats are every
// chat with messages that is not a draft and not a subagent, including those
// with a transcript on disk; the plan leaves those out. Reason is set when
// Checked is false.
type CursorDatabaseResult struct {
	// RecoveryRows and RecoveryBytes count bounded catalog values read for
	// recovery. RecoveryBudgetExhausted reports an incomplete bounded catalog.
	RecoveryRows            int
	RecoveryBytes           int64
	RecoveryBudgetExhausted bool
	Chats                   []CursorDatabaseChat
	Checked                 bool
	Reason                  CursorUncheckedReason
	// NewerFormat counts the composerData rows with a _v newer than this
	// release knows, read anyway.
	NewerFormat int
	// Subagents maps a chat's ID to the IDs of its subagent chats
	// (subagentComposerIds) that have messages and are not drafts.
	Subagents map[string][]string
	// ReadChat reads one listed chat whole, by its KeyID, as the collector
	// will: through one cursorstore.Reader for the whole plan, so at most
	// one snapshot of the database. It is not safe for concurrent use. A
	// chat Cursor deleted since the listing wraps fs.ErrNotExist. Set when
	// Checked.
	ReadChat     func(ctx context.Context, id string) (cursorstore.Composer, error)
	ReadSnapshot func(context.Context, string) (cursorstore.Composer, agentapi.SourceSnapshot, error)
	// ReadRecoverySnapshot supplies optional bounded in-place native evidence.
	ReadRecoverySnapshot func(context.Context, string, agentapi.RecoveryReadBudget) (cursorstore.Composer, agentapi.SourceSnapshot, error)
	recoveryReadBudget   *cursorRecoveryReadBudget
	// Close removes the snapshot ReadChat took, if any. Set when Checked.
	Close func() error
}

// CursorDatabaseReaderFor returns the Environment.CursorDatabase reader for
// the state.vscdb where env (its Home, OS and Getenv) says Cursor keeps it
// (see platform.Locations). A missing database is checked with no
// chats (Cursor is not installed). One that can't be read safely is not
// checked, with a reason; that never fails the plan. Only a cancelled
// context is an error. The chats are listed in place; each one the plan
// reads whole goes through one cursorstore.Reader, whose snapshot, when
// Cursor is running, lives in the per-user temporary directory until Close.
// Nothing is written beside the database.
func CursorDatabaseReaderFor(env Environment) func(context.Context) (CursorDatabaseResult, error) {
	path := env.cursorStateDatabase()
	return func(ctx context.Context) (CursorDatabaseResult, error) {
		res := readCursorDatabase(ctx, env.DatabaseCatalogs, path, cursorstore.Options{})
		if err := ctx.Err(); err != nil {
			return CursorDatabaseResult{}, err
		}
		return openCursorDatabaseSource(ctx, env, res)
	}
}

// CursorRecoveryDatabaseReaderFor composes bounded recovery evidence separately
// from ordinary filtered Cursor database output planning.
func CursorRecoveryDatabaseReaderFor(env Environment) func(context.Context, int, int64) (CursorDatabaseResult, error) {
	return func(ctx context.Context, rows int, bytes int64) (CursorDatabaseResult, error) {
		res := readCursorRecoveryDatabase(ctx, env.DatabaseCatalogs, env.cursorStateDatabase(), rows, bytes)
		if err := ctx.Err(); err != nil {
			return CursorDatabaseResult{}, err
		}
		return openCursorDatabaseSource(ctx, env, res)
	}
}

func openCursorDatabaseSource(ctx context.Context, env Environment, res CursorDatabaseResult) (CursorDatabaseResult, error) {
	if res.Checked {
		// A copy an earlier plan left when it was killed goes before this
		// plan can take another.
		cursorstore.RemoveStaleSnapshots()
		if env.Sources == nil {
			return CursorDatabaseResult{}, errors.New("source integrations are required")
		}
		provider, _, ok := env.Sources.LookupSources(archive.HarnessCursor)
		if !ok {
			return CursorDatabaseResult{}, errors.New("cursor source integration unavailable")
		}
		pass, err := provider.OpenPass(ctx, agentapi.SourceEnvironment{Database: env.cursorStateDatabase()})
		if err != nil {
			return CursorDatabaseResult{}, err
		}
		read := func(ctx context.Context, id string, recoveryBudget agentapi.RecoveryReadBudget) (cursorstore.Composer, agentapi.SourceSnapshot, error) {
			var snap agentapi.SourceSnapshot
			var err error
			if recoveryBudget != nil {
				bounded, ok := pass.(agentapi.RecoverySourcePass)
				if !ok {
					return cursorstore.Composer{}, nil, agentapi.Wrap(agentapi.Unavailable, errors.New("bounded recovery source unavailable"))
				}
				snap, err = bounded.ReadRecovery(ctx, agentapi.SourceRef{Kind: archive.SourceKindCursorSQLite, Key: id}, agentapi.ReadLimits{RawBytes: collector.DefaultMaxRawTranscriptBytes, RecordBytes: archive.MaxRecordBytes}, recoveryBudget)
			} else {
				snap, err = pass.Read(ctx, agentapi.SourceRef{Kind: archive.SourceKindCursorSQLite, Key: id}, agentapi.ReadLimits{RawBytes: collector.DefaultMaxRawTranscriptBytes, RecordBytes: archive.MaxRecordBytes})
			}
			if err != nil {
				return cursorstore.Composer{}, nil, err
			}
			var c cursorstore.Composer
			records := snap.Input().Records
			for {
				r, ok, err := records.Next(ctx)
				if err != nil {
					return c, nil, errors.Join(err, snap.Close())
				}
				if !ok {
					break
				}
				switch r.Kind {
				case agentapi.ComposerRecord:
					c.Composer = r.Raw
				case agentapi.CodexHistoryHeader, agentapi.CodexHistoryRecord:
					return c, nil, errors.Join(errors.New("unexpected Codex history record in Cursor source"), snap.Close())
				case agentapi.BubbleRecord:
					c.Bubbles = append(c.Bubbles, cursorstore.Bubble{ID: r.Key, Value: r.Raw, Missing: r.Missing})
				}
			}
			return c, snap, nil
		}
		res.ReadSnapshot = func(ctx context.Context, id string) (cursorstore.Composer, agentapi.SourceSnapshot, error) {
			return read(ctx, id, nil)
		}
		if _, ok := pass.(agentapi.RecoverySourcePass); ok {
			res.ReadRecoverySnapshot = func(ctx context.Context, id string, budget agentapi.RecoveryReadBudget) (cursorstore.Composer, agentapi.SourceSnapshot, error) {
				return read(ctx, id, budget)
			}
		}
		res.Close = pass.Close
	}
	return res, nil
}

func unchecked(reason CursorUncheckedReason) CursorDatabaseResult {
	return CursorDatabaseResult{Reason: reason}
}

// readCursorDatabase reads path through cursorstore.Read, which neither
// writes it nor creates anything beside it (see cursorstore's resolve for
// how it opens a database with Cursor running and closed).
func readCursorDatabase(ctx context.Context, catalogs agentapi.DatabaseCatalogLookup, path string, opts cursorstore.Options) CursorDatabaseResult {
	if catalogs == nil {
		return unchecked(cursorstore.UnknownFormat)
	}
	inspector, ok := catalogs.LookupDatabaseCatalog("cursor")
	if !ok {
		return unchecked(cursorstore.UnknownFormat)
	}
	var res CursorDatabaseResult
	err := cursorstore.Read(ctx, path, opts, func(ctx context.Context, db *sql.DB) error {
		catalog, err := inspector.InspectCatalog(ctx, databaseCatalogHost{db})
		res = CursorDatabaseResult{Chats: catalog.Chats, Checked: err == nil, NewerFormat: catalog.NewerFormat, Subagents: catalog.Subagents}
		return err
	})
	switch {
	case errors.Is(err, cursorstore.ErrNoDatabase):
		return CursorDatabaseResult{Checked: true}
	case err != nil:
		return unchecked(cursorstore.ReasonOf(err))
	}
	return res
}

// planCursorDatabase adds the chats found only in Cursor's database to the
// plan as candidates, each with exactly one outcome, as file sessions get.
// The database is not opened when --harness leaves Cursor out, or when
// Cursor's transcript store could not be fully listed. A chat with a
// transcript on disk is left out (the file wins); so are drafts and
// subagent chats, which the listing already leaves out.
//
// The archive's own reasons (already_archived, removed_*) and --since and
// --until (on createdAt) are decided from the listing. Every chat that may
// still be imported is then read whole and filtered with the collector's own
// code, through one Reader, so the plan's empty, unsafe_format, and
// too_large are what the collector would decide. Its project is the folder
// workspaceIdentifier.uri names, then the folder of the workspace
// workspaceIdentifier.id names (workspaceStorage/<id>/workspace.json), then
// the one folder its messages' workspaceUris agree on; the folder then goes
// through the same project rules as any working directory, and a chat with
// none is project_unknown. If a chat can't be read safely after all (Cursor
// held a lock, the copy failed), the database counts as not checked and none
// of its chats is included, as when it can't be listed.
func planCursorDatabase(ctx context.Context, env Environment, state ArchiveState, r *resolver, projectFilter []string, since, until time.Time, workers int, plan *Plan) (err error) {
	if env.CursorDatabase == nil || !harnessMatches(plan.Filters.Harnesses, "cursor") {
		return nil
	}
	if plan.cursorIncomplete {
		plan.CursorDatabaseChecked, plan.CursorDatabaseUnchecked = false, CursorUncheckedTranscriptsUnreadable
		return nil
	}
	res, err := env.CursorDatabase(ctx)
	if err != nil {
		return fmt.Errorf("read Cursor's database: %w", err)
	}
	if res.Close != nil {
		// The plan's copy of the database holds every chat; one that can't
		// be removed is reported, not left silently in the temporary folder
		// (the next sweep removes it once it is stale).
		defer func() {
			if closeErr := res.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}()
	}
	plan.CursorDatabaseChecked, plan.CursorDatabaseUnchecked = res.Checked, res.Reason
	plan.CursorDatabaseNewerFormat = res.NewerFormat
	if !res.Checked {
		return nil
	}
	items, toRead, err := selectCursorDatabaseChats(res.Chats, plan.Candidates, state, plan.Filters.IncludeRemoved, since, until)
	if err != nil {
		return err
	}
	if res.ReadChat == nil && res.ReadSnapshot == nil {
		plan.CursorDatabaseChecked, plan.CursorDatabaseUnchecked = false, CursorUncheckedUnreadable
		return nil
	}
	if err := readCursorDatabaseChats(ctx, env, workers, res.ReadChat, res.ReadSnapshot, toRead, env.Sources); err != nil {
		if ctx.Err() != nil || fatalSourceFailure(err) {
			return err
		}
		plan.CursorDatabaseChecked, plan.CursorDatabaseUnchecked = false, cursorstore.ReasonOf(err)
		return nil
	}
	appendCursorDatabaseChats(env, r, projectFilter, items, plan)
	plan.CursorSubagentsNotImported += cursorDatabaseSubagentCount(plan.Candidates, res.Subagents)
	return nil
}

// selectCursorDatabaseChats decides which listed rows may need a whole-chat
// read. Archive classification is the only IO in this step.
func selectCursorDatabaseChats(chats []CursorDatabaseChat, candidates []Candidate, state ArchiveState, includeRemoved bool, since, until time.Time) (items, toRead []*work, err error) {
	fileChats := map[string]bool{}
	for _, c := range candidates {
		if c.Harness == "cursor" {
			fileChats[c.NativeSessionID] = true
		}
	}
	// Two rows with one composerId are one chat found twice. The row kept is
	// one whose key is its ID, which the chat can be read back by, so a
	// stray row naming another chat's ID never displaces the real one.
	kept := map[string]int{}
	for i, chat := range chats {
		if k, ok := kept[chat.ID]; !ok || (chats[k].KeyID != chat.ID && chat.KeyID == chat.ID) {
			kept[chat.ID] = i
		}
	}
	for i, chat := range chats {
		if fileChats[chat.ID] {
			continue
		}
		w := &work{
			t:    &transcript{harness: harnessCursor, nativeID: chat.ID},
			chat: chat,
			c: Candidate{
				Harness: string(harnessCursor), NativeSessionID: chat.ID,
				SourceKind: archive.SourceKindCursorSQLite, SourceKey: chat.ID,
				StartedAt: chat.CreatedAt, StartedAtSource: archive.StartedAtSourceCursorComposer,
			},
		}
		items = append(items, w)
		if kept[chat.ID] != i {
			w.duplicate = true
			continue
		}
		var reason SkipReason
		if !utf8.ValidString(chat.ID) {
			w.unsafe = true
			continue
		}
		if strings.TrimSpace(chat.ID) != "" {
			reason, err = state.Classify("cursor", chat.ID)
			if err != nil {
				return nil, nil, fmt.Errorf("check the archive: %w", err)
			}
		}
		decision := decideCursorDatabaseChat(chat, reason, includeRemoved, since, until)
		w.state, w.filtered, w.unsafe = decision.state, decision.filtered, decision.unsafe
		w.t.identityMismatch = decision.identityMismatch
		if decision.read {
			toRead = append(toRead, w)
		}
	}
	return items, toRead, nil
}

type cursorDatabaseDecision struct {
	state            SkipReason
	filtered         bool
	unsafe           bool
	identityMismatch bool
	read             bool
}

// decideCursorDatabaseChat is the pure gate before a whole-chat read. The
// archive reason is supplied by the caller so no storage access occurs here.
func decideCursorDatabaseChat(chat CursorDatabaseChat, reason SkipReason, includeRemoved bool, since, until time.Time) cursorDatabaseDecision {
	if includeRemoved && (reason == SkipRemovedByUndo || reason == SkipRemovedByRetention) {
		reason = ""
	}
	d := cursorDatabaseDecision{
		state:    reason,
		filtered: (!since.IsZero() || !until.IsZero()) && !inRange(chat.CreatedAt, since, until),
		unsafe:   chat.Malformed,
		// A row whose composerId is not its key's cannot be read back
		// under the ID it would register.
		identityMismatch: chat.ID != chat.KeyID,
	}
	d.read = d.state == "" && !d.filtered && !d.unsafe && !d.identityMismatch
	return d
}

// readCursorDatabaseChats serializes reads through one Reader and filters
// independent chats on the workers. A database failure aborts the whole set.
func readCursorDatabaseChats(ctx context.Context, env Environment, workers int, readChat func(context.Context, string) (cursorstore.Composer, error), readSnapshot func(context.Context, string) (cursorstore.Composer, agentapi.SourceSnapshot, error), toRead []*work, sources agentapi.SourcesLookup) error {
	// Reads go one at a time through the plan's one Reader; filtering, the
	// costly part, runs on the workers.
	var mu sync.Mutex
	var readErr error
	if err := forEach(ctx, workers, toRead, func(w *work) {
		mu.Lock()
		var c cursorstore.Composer
		var snap agentapi.SourceSnapshot
		var err error
		if readSnapshot != nil {
			c, snap, err = readSnapshot(ctx, w.chat.KeyID)
		} else {
			c, err = readChat(ctx, w.chat.KeyID)
		}
		if snap != nil {
			defer func() {
				mu.Lock()
				readErr = errors.Join(readErr, snap.Close())
				mu.Unlock()
			}()
		}
		// A value of this chat's that does not decode is the chat's
		// problem, unsafe_format; only a failure of the database itself
		// (a lock, a failed copy, a changed file) leaves it unchecked.
		chatOnly := err != nil && !fatalSourceFailure(err) && (isNotExist(err) || cursorstore.ReasonOf(err) == cursorstore.UnknownFormat || agentapi.HasFailure(err, agentapi.Limit))
		if err != nil && !chatOnly && readErr == nil {
			readErr = err
		}
		mu.Unlock()
		if err != nil {
			// A chat Cursor deleted since the listing is not counted at all.
			w.vanished = isNotExist(err)
			w.tooLarge = agentapi.HasFailure(err, agentapi.Limit)
			w.unsafe = !w.vanished && !w.tooLarge
			return
		}
		w.c.Bytes = collector.CursorChatSize(c)
		w.messageFolders = messageWorkspaceFolders(env, c)
		var filtered archive.FilteredTranscript
		if snap == nil {
			filtered, err = collector.FilterCursorChat(c, sources)
		} else {
			_, filter, ok := sources.LookupSources(archive.HarnessCursor)
			if !ok {
				err = errors.New("cursor filter unavailable")
			} else {
				filtered, err = filter.Filter(ctx, snap.Input(), agentapi.FilterContext{Limits: agentapi.ReadLimits{RawBytes: collector.DefaultMaxRawTranscriptBytes, RecordBytes: archive.MaxRecordBytes}})
				if err == nil && int64(filtered.Boundary.RetainedBytes) > collector.DefaultMaxTranscriptBytes {
					err = archive.ErrRecordTooLarge
				}
			}
		}
		if fatalSourceFailure(err) {
			mu.Lock()
			readErr = errors.Join(readErr, err)
			mu.Unlock()
			return
		}
		switch {
		case errors.Is(err, archive.ErrRecordTooLarge):
			w.tooLarge = true
		case err != nil:
			w.unsafe = true
		default:
			if env.Imports == nil {
				w.unsafe = true
				break
			}
			inspector, ok := env.Imports.LookupImport(string(w.t.harness))
			if !ok {
				w.unsafe = true
				break
			}
			applyImportInspection(ctx, inspector, w, filtered)
		}
	}); err != nil {
		return err
	}
	return readErr
}

// appendCursorDatabaseChats resolves projects and adds only rows that still
// exist. No candidate is committed until all database reads have succeeded.
func appendCursorDatabaseChats(env Environment, r *resolver, projectFilter []string, items []*work, plan *Plan) {
	for _, w := range items {
		if w.vanished {
			continue
		}
		if !w.duplicate {
			if folder := cursorChatFolder(env, w.chat, w.messageFolders); folder != "" {
				w.res = r.resolve(folder)
			} else {
				w.res = resolution{skip: SkipProjectUnknown}
			}
			if !projectMatches(env, projectFilter, w.res.root) {
				w.filtered = true
			}
		}
		w.c.HistoricalProject = w.res.historical
		w.c.ProjectRoot, w.c.ProjectKind, w.c.ProjectIncluded = w.res.root, w.res.kind, w.res.included
		if w.res.root != "" {
			w.c.ProjectExists = env.exists(w.res.root)
		}
		w.c.Skip = w.reason(plan.GeneratedAt)
		w.c.Diagnostic = candidateDiagnostic(w.c.Skip, w.res.outcome, w.res.cause)
		plan.Candidates = append(plan.Candidates, w.c)
	}
}

func cursorDatabaseSubagentCount(candidates []Candidate, subagents map[string][]string) int {
	// Subagent chats are not imported yet: count those of every Cursor chat
	// this plan imports, file or database, so the plan can say so.
	count := 0
	for _, c := range candidates {
		if c.Harness == "cursor" && c.Skip == "" {
			count += len(subagents[c.NativeSessionID])
		}
	}
	return count
}

// cursorChatFolder is the folder a database chat's project is resolved
// from: workspaceIdentifier.uri, then the workspace.json of the workspace
// workspaceIdentifier.id names, then the one folder the chat's messages name
// in workspaceUris. "" when none is known.
func cursorChatFolder(env Environment, chat CursorDatabaseChat, messageFolders []string) string {
	if chat.Folder != "" {
		return chat.Folder
	}
	if id := chat.WorkspaceID; id != "" && id == filepath.Base(id) && id != "." && id != ".." {
		data, err := env.readFile(filepath.Join(cursorWorkspaceStorage(env), id, "workspace.json"))
		if err == nil {
			if folder := workspaceMetadataFolder(env, "cursor", data); folder != "" {
				return folder
			}
		}
	}
	if len(messageFolders) == 1 {
		return messageFolders[0]
	}
	return ""
}

// messageWorkspaceFolders are the distinct local folders a chat's messages
// name in workspaceUris, in order. Only that field of each message is kept.
func messageWorkspaceFolders(env Environment, c cursorstore.Composer) []string {
	if env.Workspaces == nil {
		return nil
	}
	provider, ok := env.Workspaces.LookupWorkspace("cursor")
	if !ok {
		return nil
	}
	var folders []string
	seen := map[string]bool{}
	for _, b := range c.Bubbles {
		for _, folder := range provider.WorkspaceFolders(agentapi.WorkspaceEvidence{Purpose: agentapi.WorkspaceMessageRecord, Bytes: b.Value}) {
			if !seen[folder] {
				seen[folder] = true
				folders = append(folders, folder)
			}
		}
	}
	return folders
}

// databaseCatalogHost supplies read-only rows; native query/schema stay behind the port.
type databaseCatalogHost struct{ db *sql.DB }

func (h databaseCatalogHost) Query(ctx context.Context, query string, visit func(agentapi.DatabaseRecord) error) (err error) {
	rows, err := h.db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		if err := visit(agentapi.DatabaseRecord{Key: key, Value: value}); err != nil {
			return err
		}
	}
	return rows.Err()
}
