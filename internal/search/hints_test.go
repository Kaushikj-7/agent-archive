package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestHintsNeverCreateOrUpgradeContentStorage(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "missing")
	if err := RecordSessionHints(t.Context(), dir, "synthetic", []SessionIdentity{{Harness: "codex", SessionID: "one"}}); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("hint created missing directory", err)
	}
	dir = t.TempDir()
	if err := RecordSessionHints(t.Context(), dir, "synthetic", []SessionIdentity{{Harness: "codex", SessionID: "one"}}); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "content.sqlite")); !os.IsNotExist(err) {
		t.Fatal("hint created database", err)
	}
	index, err := Open(t.Context(), dir, "synthetic")
	hintMust(t, err)
	_, err = index.db.ExecContext(t.Context(), "PRAGMA user_version=3")
	hintMust(t, err)
	hintMust(t, index.Close())
	err = RecordSessionHints(t.Context(), dir, "synthetic", []SessionIdentity{{Harness: "codex", SessionID: "one"}})
	if !errors.Is(err, ErrUpgradeRequired) {
		t.Fatal(err)
	}
	// Existing-only admission must not call initialize or migrate old schema.
	db, err := sql.Open("sqlite", filepath.Join(dir, "content.sqlite"))
	hintMust(t, err)
	defer func() { _ = db.Close() }()
	var version int
	hintMust(t, db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version))
	if version != 3 {
		t.Fatal("hint migrated schema", version)
	}
}

func TestHintsCapAccumulatedUnknownIdentitiesAndDoNotRewindSweep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	index, err := Open(t.Context(), dir, "synthetic")
	hintMust(t, err)
	defer func() { _ = index.Close() }()
	_, err = index.db.ExecContext(t.Context(), "UPDATE search_discovery SET validated='keep-cursor'")
	hintMust(t, err)
	for batch := range 12 {
		identities := make([]SessionIdentity, 300)
		for i := range identities {
			identities[i] = SessionIdentity{Harness: "codex", SessionID: fmt.Sprintf("session-%d-%d", batch, i)}
		}
		hintMust(t, RecordSessionHints(t.Context(), dir, "synthetic", identities))
	}
	var count int
	hintMust(t, index.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_observed").Scan(&count))
	if count != unobservedHintLimit {
		t.Fatal("accumulated hint state exceeded cap", count)
	}
	var cursor string
	hintMust(t, index.db.QueryRowContext(t.Context(), "SELECT validated FROM search_discovery WHERE id=1").Scan(&cursor))
	if cursor != "keep-cursor" {
		t.Fatal("hint rewound fair sweep", cursor)
	}
	var fabricated int
	hintMust(t, index.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_work").Scan(&fabricated))
	if fabricated != 0 {
		t.Fatal("hint fabricated authoritative metadata")
	}
	hintMust(t, RecordSessionHints(t.Context(), dir, "synthetic", []SessionIdentity{{Harness: "codex", SessionID: "session-0-0"}}))
}

func TestHintsCoalesceAndRespectNamespaceCancellationAndContention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	index, err := Open(t.Context(), dir, "synthetic")
	hintMust(t, err)
	defer func() { _ = index.Close() }()
	ids := []SessionIdentity{{Harness: "codex", SessionID: "one"}, {Harness: "codex", SessionID: "one"}, {Harness: "codex", SessionID: "../../invalid"}}
	hintMust(t, RecordSessionHints(t.Context(), dir, "synthetic", ids))
	var count int
	hintMust(t, index.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_observed").Scan(&count))
	if count != 1 {
		t.Fatal("hints failed dedup/validation", count)
	}
	if err = RecordSessionHints(t.Context(), dir, "other", ids); err == nil {
		t.Fatal("cross namespace hint accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = RecordSessionHints(ctx, dir, "synthetic", ids); err == nil {
		t.Fatal("cancelled hints accepted")
	}
	tx, err := index.db.BeginTx(t.Context(), nil)
	hintMust(t, err)
	_, err = tx.ExecContext(t.Context(), "UPDATE search_state SET generation=generation+1")
	hintMust(t, err)
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	started := time.Now()
	err = RecordSessionHints(ctx, dir, "synthetic", []SessionIdentity{{Harness: "codex", SessionID: "busy"}})
	elapsed := time.Since(started)
	cancel()
	hintMust(t, tx.Rollback())
	t.Logf("actual driver open/admission/transaction/close busy wall time: %s", elapsed)
	if err == nil || elapsed > 250*time.Millisecond {
		t.Fatal("busy hint did not drop boundedly", elapsed, err)
	}
	hintMust(t, index.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_observed").Scan(&count))
	if count != 1 {
		t.Fatal("busy hint partially committed")
	}
}

func hintMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAdmittedHintsInvalidateIdentityCompletenessUntilAuthoritativeConsumption(t *testing.T) {
	t.Parallel()
	a := newSearchArchive(t)
	m := a.session(t, "known")
	parsers := builtin.NewBuiltins()
	dir := t.TempDir()
	index, err := Open(t.Context(), dir, "synthetic")
	hintMust(t, err)
	defer func() { _ = index.Close() }()
	at := m.CapturedAt
	for i := range 16 {
		_, err = index.Reconcile(t.Context(), a.remote, parsers, at.Add(time.Duration(i)*time.Second), ReconcileBudget{})
		hintMust(t, err)
		_, err = index.Run(t.Context(), a.remote, parsers, at.Add(time.Minute), Budget{VerifyCurrent: true})
		hintMust(t, err)
		observed, e := index.Observation(t.Context())
		hintMust(t, e)
		if observed.Complete {
			break
		}
	}
	before, err := index.Observation(t.Context())
	hintMust(t, err)
	if !before.Complete {
		t.Fatal("fixture lacks complete historical snapshot", before)
	}
	hintMust(t, RecordSessionHints(t.Context(), dir, "synthetic", []SessionIdentity{{Harness: m.Harness.Name, SessionID: m.SessionID}, {Harness: "codex", SessionID: "unpublished"}}))
	hinted, err := index.Observation(t.Context())
	hintMust(t, err)
	if hinted.Rechecks != 2 || hinted.IdentityChecksComplete || hinted.Complete || !hinted.DiscoveryComplete || !hinted.LastReconciled.Equal(before.LastReconciled) {
		t.Fatal("hint fabricated freshness or retained false completion", hinted)
	}
	for i := range 8 {
		_, err = index.Reconcile(t.Context(), a.remote, parsers, at.Add(time.Duration(i+60)*time.Second), ReconcileBudget{})
		hintMust(t, err)
		observed, e := index.Observation(t.Context())
		hintMust(t, e)
		if observed.Complete {
			if observed.Rechecks != 0 {
				t.Fatal("completed with pending identities")
			}
			return
		}
	}
	t.Fatal("periodic known sweep did not consume present/absent hint identities")
}

func TestPendingRecheckCoverageUsesExistingObservedTimeIndex(t *testing.T) {
	dir := t.TempDir()
	index, err := Open(t.Context(), dir, "synthetic")
	hintMust(t, err)
	defer func() { _ = index.Close() }()
	tx, err := index.db.BeginTx(t.Context(), nil)
	hintMust(t, err)
	for i := range 5000 {
		_, err = tx.ExecContext(t.Context(), "INSERT INTO search_observed VALUES(?,?,?,?)", fmt.Sprintf("key-%05d", i), fmt.Sprintf("codex/%d", i), "real-revision", i+1)
		hintMust(t, err)
	}
	hintMust(t, tx.Commit())
	const query = "SELECT coalesce(max(observed),0),(SELECT count(*) FROM search_observed WHERE observed=0) FROM search_observed"
	rows, err := index.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query)
	hintMust(t, err)
	defer func() { _ = rows.Close() }()
	indexed := 0
	for rows.Next() {
		var id, parent, unused int
		var detail string
		hintMust(t, rows.Scan(&id, &parent, &unused, &detail))
		t.Log(detail)
		if strings.Contains(detail, "SCAN search_observed") {
			t.Fatal("coverage scans entire identity table", detail)
		}
		if strings.Contains(detail, "COVERING INDEX search_observed_time") {
			indexed++
		}
	}
	hintMust(t, rows.Err())
	if indexed != 2 {
		t.Fatal("MAX and pending range did not both use existing covering index", indexed)
	}
	hintMust(t, rows.Close())
	started := time.Now()
	for range 100 {
		var maximum int64
		var pending int
		hintMust(t, index.db.QueryRowContext(t.Context(), query).Scan(&maximum, &pending))
		if maximum != 5000 || pending != 0 {
			t.Fatal(maximum, pending)
		}
	}
	t.Logf("actual5000-identity MAX plus indexed zero-pending count: %s/op", time.Since(started)/100)
}

func TestHintsRetainDiscoveryTimeAndConservativelyInvalidateObservation(t *testing.T) {
	t.Parallel()
	index, err := Open(t.Context(), t.TempDir(), "synthetic")
	hintMust(t, err)
	defer func() { _ = index.Close() }()
	_, err = index.db.ExecContext(t.Context(), "UPDATE search_discovery SET observed=100,mode='catalog',complete_root='root',checked_root='root',seeded=3")
	hintMust(t, err)
	key, err := archive.MetadataObjectKey("codex", "known")
	hintMust(t, err)
	_, err = index.db.ExecContext(t.Context(), "INSERT INTO search_observed VALUES(?,'codex/known','real-revision',200)", key)
	hintMust(t, err)
	before, err := index.Observation(t.Context())
	hintMust(t, err)
	hintMust(t, insertSessionHints(t.Context(), index.db, []SessionIdentity{{Harness: "codex", SessionID: "known"}}))
	after, err := index.Observation(t.Context())
	hintMust(t, err)
	if !after.LastReconciled.Equal(before.LastReconciled) || !after.LastObserved.Equal(after.LastReconciled) || !after.LastObserved.Before(before.LastObserved) || after.Rechecks != 1 || after.IdentityChecksComplete || after.Complete {
		t.Fatal("hint misreported settled observations or pending coverage", before, after)
	}
	var revision string
	hintMust(t, index.db.QueryRowContext(t.Context(), "SELECT revision FROM search_observed WHERE key=?", key).Scan(&revision))
	if revision != "real-revision" {
		t.Fatal("hint fabricated a metadata revision", revision)
	}
}
