package backfill

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

const (
	cursorRecoveryRows     = 1024
	cursorRecoveryBytes    = 128 << 20
	cursorRecoveryRenewals = 8
	cursorRecoveryRecords  = 65536
)

type recoveryCatalogState struct{}

func (recoveryCatalogState) Classify(string, string) (SkipReason, error) { return "", nil }

type recoveryFileStamp struct {
	info   fs.FileInfo
	absent bool
}

func recoveryStamps(env Environment, paths []string) ([]recoveryFileStamp, bool) {
	stamps := make([]recoveryFileStamp, len(paths))
	for i, path := range paths {
		info, err := env.lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			stamps[i].absent = true
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return nil, false
		}
		stamps[i].info = info
	}
	return stamps, true
}

func sameRecoveryStamps(a, b []recoveryFileStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for i, old := range a {
		fresh := b[i]
		if old.absent != fresh.absent {
			return false
		}
		if !old.absent && (!os.SameFile(old.info, fresh.info) || old.info.Size() != fresh.info.Size() || !old.info.ModTime().Equal(fresh.info.ModTime()) || old.info.Mode() != fresh.info.Mode()) {
			return false
		}
	}
	return true
}

type cursorRecoveryEpoch struct {
	works    []*work
	complete bool
	budget   bool
	// gaps names known causes of incompleteness; an incomplete epoch without
	// one is an unavailable database (see cursorRecoveryGaps).
	gaps    recoveryGaps
	digest  [32]byte
	paths   []string
	stamps  []recoveryFileStamp
	settled bool
}

// readCursorRecoveryEpoch validates supported database candidates before any
// output filter. It retains only content-free ownership/eligibility facts.
func readCursorRecoveryEpoch(ctx context.Context, env Environment, r *resolver, unread unreadable) (epoch cursorRecoveryEpoch, err error) {
	path := env.cursorStateDatabase()
	if path != "" {
		epoch.paths = []string{path, path + "-wal", path + "-shm", path + "-journal"}
	}
	before, stable := recoveryStamps(env, epoch.paths)
	if env.CursorRecoveryDatabase == nil {
		epoch.complete = env.CursorDatabase == nil && (path == "" || (stable && before[0].absent))
		epoch.stamps, epoch.settled = before, stable
		return epoch, nil
	}
	if unread.cursorIncomplete {
		epoch.gaps.add(string(harnessCursor), CauseNativeStoreUnreadable)
		return epoch, nil
	}
	res, err := env.CursorRecoveryDatabase(ctx, cursorRecoveryRows, cursorRecoveryBytes)
	if err != nil {
		if fatalSourceFailure(err) {
			return epoch, err
		}
		return epoch, nil
	}
	if res.Close != nil {
		defer func() { err = errors.Join(err, res.Close()) }()
	}
	epoch.budget = res.RecoveryBudgetExhausted
	if !res.Checked || res.ReadRecoverySnapshot == nil {
		return epoch, nil
	}
	if len(res.Chats) > cursorRecoveryRows {
		epoch.budget = true
		return epoch, nil
	}
	epoch.paths = append(epoch.paths, cursorRecoveryWorkspacePaths(env, res.Chats)...)
	// Bracket workspace observations as well as database reads. A workspace
	// changed while resolving its folder cannot become a settled witness.
	allBefore, allStable := recoveryStamps(env, epoch.paths)
	works, toRead, err := selectCursorDatabaseChats(res.Chats, nil, recoveryCatalogState{}, true, time.Time{}, time.Time{})
	if err != nil {
		return epoch, err
	}
	readSnapshot, budget := cursorRecoveryReaders(res, cursorRecoveryBytes-res.RecoveryBytes)
	if err := readCursorDatabaseChats(ctx, env, 1, nil, readSnapshot, toRead, env.Sources); err != nil {
		if ctx.Err() != nil || fatalSourceFailure(err) {
			return epoch, err
		}
		epoch.budget = budget.exhausted
		return epoch, nil
	}
	epoch.complete, epoch.gaps, err = observeCursorRecoveryWorks(ctx, env, r, works, budget)
	if err != nil {
		return epoch, err
	}
	epoch.budget = epoch.budget || budget.exhausted
	epoch.works = works
	epoch.digest = cursorRecoveryDigest(works)
	settleCursorRecoveryEpoch(env, &epoch, before, allBefore, stable, allStable)

	return epoch, nil
}

// prepareCursorRecoveryWitnesses makes one prefilter evidence observation,
// then validates settled database/side-state/workspace stamps per admission
// slice. Virtual/unsettled sources require one renewed bounded epoch instead.
func prepareCursorRecoveryWitnesses(ctx context.Context, env Environment, r *resolver, items []*work, unread unreadable) ([]*work, recoveryGaps, error) {
	relevant := false
	for _, w := range items {
		if !env.exists(w.t.cwd) && (archive.IsRepoKey(w.t.repoKey) || r.filters.ProjectMappings[filepath.Clean(w.t.cwd)] != "") {
			relevant = true
			break
		}
	}
	if !relevant {
		return nil, nil, nil
	}
	epoch, err := readCursorRecoveryEpoch(ctx, env, r, unread)
	if err != nil {
		return nil, nil, err
	}
	gaps := cursorRecoveryGaps(epoch)
	for _, w := range epoch.works {
		// An incomplete epoch cannot be renewed: its chats stay clone evidence
		// but cannot propose a destination.
		w.evidenceOnly = !epoch.complete
	}
	renewals := 0
	r.databaseRecoveryCurrent = func(ctx context.Context) bool {
		if ctx.Err() != nil {
			return false
		}
		if !epoch.complete {
			// The Cursor gap was accepted at planning for the recoveries it does
			// not block (Cursor's own stay blocked); there is nothing to renew.
			return true
		}
		if epoch.settled {
			current, ok := recoveryStamps(env, epoch.paths)
			return ok && sameRecoveryStamps(epoch.stamps, current)
		}
		if renewals >= cursorRecoveryRenewals {
			return false // A new plan is needed to renew an unsettled source again.
		}
		renewals++
		renewed, err := readCursorRecoveryEpoch(ctx, env, r, unread)
		return err == nil && renewed.complete && renewed.digest == epoch.digest
	}
	return epoch.works, gaps, nil
}

// cursorRecoveryGaps is empty for a complete epoch. An incomplete one names its
// known causes, the budget when it was reached, and otherwise an unavailable
// database (locked, unreadable, unsettled or holding unusable rows).
func cursorRecoveryGaps(epoch cursorRecoveryEpoch) recoveryGaps {
	if epoch.complete {
		return nil
	}
	var gaps recoveryGaps
	gaps.merge(epoch.gaps)
	if epoch.budget {
		gaps.add(string(harnessCursor), CauseRecoveryBudget)
	}
	if len(gaps) == 0 {
		gaps.add(string(harnessCursor), CauseCursorDatabaseUnavailable)
	}
	return gaps
}

type cursorRecoveryReadBudget struct {
	remaining int64
	rows      int
	exhausted bool
}

func (b *cursorRecoveryReadBudget) RemainingRows() int { return b.rows }

func (b *cursorRecoveryReadBudget) RemainingBytes() int64 { return b.remaining }

func (b *cursorRecoveryReadBudget) Charge(rows int, bytes int64) error {
	if b.exhausted || rows < 0 || bytes < 0 || rows > b.rows || bytes > b.remaining {
		b.exhausted = true
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	b.rows -= rows
	b.remaining -= bytes
	return nil
}

func cursorRecoveryReaders(res CursorDatabaseResult, remaining int64) (func(context.Context, string) (cursorstore.Composer, agentapi.SourceSnapshot, error), *cursorRecoveryReadBudget) {
	budget := res.recoveryReadBudget
	if budget == nil {
		budget = &cursorRecoveryReadBudget{remaining: remaining, rows: cursorRecoveryRecords}
	}
	readSnapshot := func(ctx context.Context, id string) (cursorstore.Composer, agentapi.SourceSnapshot, error) {
		if budget.exhausted || budget.remaining <= 0 {
			budget.exhausted = true
			return cursorstore.Composer{}, nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
		}
		c, snap, err := res.ReadRecoverySnapshot(ctx, id, budget)
		if agentapi.HasFailure(err, agentapi.Limit) {
			budget.exhausted = true
		}
		return c, snap, err
	}
	return readSnapshot, budget
}

func cursorRecoveryDigest(works []*work) [32]byte {
	// Membership, native identity, original creation, format eligibility and
	// ownership all participate. Hashes stay private; no message bytes are kept.
	type fact struct {
		ID          string     `json:"id"`
		KeyID       string     `json:"key_id"`
		CreatedAt   time.Time  `json:"created_at"`
		Folder      string     `json:"folder"`
		WorkspaceID string     `json:"workspace_id"`
		Workspace   bool       `json:"workspace"`
		Malformed   bool       `json:"malformed"`
		Root        string     `json:"root"`
		Skip        SkipReason `json:"skip"`
		Unsafe      bool       `json:"unsafe"`
		Empty       bool       `json:"empty"`
		Large       bool       `json:"large"`
		Vanished    bool       `json:"vanished"`
		Duplicate   bool       `json:"duplicate"`
		Mismatch    bool       `json:"mismatch"`
		Validated   bool       `json:"validated"`
	}
	facts := make([]fact, 0, len(works))
	for _, w := range works {
		facts = append(facts, fact{ID: w.chat.ID, KeyID: w.chat.KeyID, CreatedAt: w.chat.CreatedAt, Folder: w.chat.Folder, WorkspaceID: w.chat.WorkspaceID, Workspace: w.chat.WorkspaceIdentifier, Malformed: w.chat.Malformed, Root: w.res.root, Skip: w.res.skip, Unsafe: w.unsafe, Empty: w.empty, Large: w.tooLarge, Vanished: w.vanished, Duplicate: w.duplicate, Mismatch: w.t.identityMismatch, Validated: w.validated})
	}
	sort.Slice(facts, func(i, j int) bool {
		a, _ := json.Marshal(facts[i])
		b, _ := json.Marshal(facts[j])
		return string(a) < string(b)
	})
	data, _ := json.Marshal(facts)
	return sha256.Sum256(data)
}

func cursorRecoveryWorkspacePaths(env Environment, chats []CursorDatabaseChat) []string {
	var paths []string
	for _, chat := range chats {
		if id := chat.WorkspaceID; id != "" && id == filepath.Base(id) && id != "." && id != ".." {
			paths = append(paths, filepath.Join(cursorWorkspaceStorage(env), id, "workspace.json"))
		}
	}
	return paths
}

// observeCursorRecoveryWorks resolves each chat's folder. A chat without any
// folder evidence (cursorChatFolderless) is a non-witness, not a gap. A chat
// whose workspace reference or folder cannot be resolved stays a gap: that
// evidence could name a second clone.
func observeCursorRecoveryWorks(ctx context.Context, env Environment, r *resolver, works []*work, budget *cursorRecoveryReadBudget) (bool, recoveryGaps, error) {
	complete := !budget.exhausted
	var gaps recoveryGaps
	workspaceEnv := env
	var workspaceErr error
	workspaceRead := true
	workspaceEnv.ReadFile = recoveryWorkspaceReader(ctx, env, budget, &workspaceRead, &workspaceErr)
	for _, w := range works {
		if w.vanished || w.duplicate || w.unsafe || w.tooLarge || w.t.identityMismatch || w.empty {
			complete = false
			continue
		}
		folder := cursorChatFolder(workspaceEnv, w.chat, w.messageFolders)
		w.t.cwd = folder
		w.res = r.resolve(folder)
		if !workspaceRead || (w.res.skip == SkipProjectUnknown && !cursorChatFolderless(w.chat, w.messageFolders)) {
			complete = false
			workspaceRead = true
			gaps.add(string(harnessCursor), CauseCursorChatFolderUnavailable)
		}
	}
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	return complete, gaps, workspaceErr
}

func settleCursorRecoveryEpoch(env Environment, epoch *cursorRecoveryEpoch, before, allBefore []recoveryFileStamp, stable, allStable bool) {
	after, ok := recoveryStamps(env, epoch.paths)
	if stable && allStable && ok && len(before) > 0 && sameRecoveryStamps(before, after[:len(before)]) && sameRecoveryStamps(allBefore, after) && (!before[0].absent || len(epoch.works) == 0) {
		epoch.stamps, epoch.settled = after, true
	} else if !allStable || !ok || !sameRecoveryStamps(allBefore, after) || (len(before) > 0 && !sameRecoveryStamps(before, after[:min(len(before), len(after))])) {
		epoch.complete = false
	}
}
