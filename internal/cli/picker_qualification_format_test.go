package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/destination"
)

const qualificationFormat = 11

func qualificationSeed(tb testing.TB, raw *qualifiedCLIStore) catalog.CatalogHead {
	tb.Helper()
	scoped := &scopedMigrationCLIStore{raw}
	cfg := destination.Config{Provider: "s3", Bucket: "private-picker-qualification", ArchiveFormat: destination.FormatCatalogV4}
	u, err := catalog.OpenPickerUpgrade(context.Background(), scoped, cfg, scoped, "")
	if err != nil {
		tb.Fatal(err)
	}
	if done, e := u.Step(context.Background(), 128); e != nil || !done {
		tb.Fatal("empty construction", done, e)
	}
	if err = u.Activate(context.Background()); err != nil {
		tb.Fatal(err)
	}
	if err = u.FinishActivation(context.Background()); err != nil {
		tb.Fatal(err)
	}
	w, err := catalog.New(raw)
	if err != nil {
		tb.Fatal(err)
	}
	h, _, err := w.Head(context.Background())
	if err != nil {
		tb.Fatal(err)
	}
	return h
}

func qualificationEntry(tb testing.TB, e catalog.CatalogEntry) catalog.CatalogEntry {
	tb.Helper()
	key, err := archive.MetadataObjectKey(e.Summary.Harness.Name, e.Summary.SessionID)
	if err != nil {
		tb.Fatal(err)
	}
	e.Picker, err = catalog.BuildPickerRow(key, e)
	if err != nil {
		tb.Fatal(err)
	}
	return e
}

func qualificationCompact(e catalog.CatalogEntry) catalog.CatalogEntry {
	m := e.Summary
	e.SummaryOverflow = "metadata-v10"
	e.Summary = archive.Metadata{SchemaVersion: m.SchemaVersion, SessionID: m.SessionID, Harness: archive.Harness{Name: m.Harness.Name}, ProjectID: m.ProjectID, CapturedAt: m.CapturedAt, EndedAt: m.EndedAt, ParentSessionID: m.ParentSessionID, NativeChild: m.NativeChild}
	if m.Replay != nil {
		e.Summary.Replay = &archive.Replay{}
	}
	if m.Origin == archive.SessionOriginImport {
		e.Summary.Origin = m.Origin
		e.Summary.StartedAt = m.StartedAt
	}
	return e
}

func qualificationPickerLeaves(tb testing.TB, key string, e catalog.CatalogEntry) []qualificationLeaf {
	tb.Helper()
	keys, err := catalog.PickerMembershipKeys(key, *e.Picker)
	if err != nil {
		tb.Fatal(err)
	}
	canonical, err := catalog.PickerIdentityKey(*e.Picker)
	if err != nil {
		tb.Fatal(err)
	}
	keys = append(keys, canonical)
	raw := qualificationJSON(tb, qualificationCompact(e))
	var result []qualificationLeaf
	for _, k := range keys {
		if _, err = catalog.DecodePickerRow(k, raw); err != nil {
			tb.Fatal(err)
		}
		result = append(result, qualificationLeaf{k, json.RawMessage(raw)})
	}
	return result
}
