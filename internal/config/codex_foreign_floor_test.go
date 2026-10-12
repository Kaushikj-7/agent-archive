package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCodexFloorPublicPauseRefusesAtomically(t *testing.T) {
	for _, paused := range []bool{false, true} {
		for _, scope := range []string{"scope", "source"} {
			t.Run(scope+map[bool]string{true: "resume", false: "pause"}[paused], func(t *testing.T) {
				c, at := blanketConfig(t)
				home := t.TempDir()
				if paused {
					if err := transitionDiscoveryPause(&c, true, at.Add(time.Minute)); err != nil {
						t.Fatal(err)
					}
					c.Paused = true
				}
				bad := c.CodexCapture.Authorization
				if scope == "source" {
					bad = c.CodexCapture.SourceAuthorization
				}
				bad.NativeStartFloor = at.Add(2 * time.Minute)
				bad.Intervals = []DiscoveryInterval{{Start: at.Add(2 * time.Minute)}}
				if paused {
					bad.Intervals = nil
				}
				if err := Save(home, c); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(filepath.Join(home, "config.json"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := SetPaused(home, !paused, at.Add(time.Minute)); err == nil {
					t.Fatal("invalid transition accepted")
				}
				after, err := os.ReadFile(filepath.Join(home, "config.json"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("refusal changed durable state", err)
				}
			})
		}
	}
	c, at := blanketConfig(t)
	before, _ := json.Marshal(c)
	if err := transitionDiscoveryPause(&c, true, at); err == nil {
		t.Fatal("pause equality accepted")
	}
	after, _ := json.Marshal(c)
	if !bytes.Equal(before, after) {
		t.Fatal("equality refusal mutated shared histories")
	}
}

func TestCodexPausedScopeAndSourceCreationKeepImmutableFloors(t *testing.T) {
	c, at := blanketConfig(t)
	previous := c
	c.Paused = true
	c.Storage.Bucket = "changed"
	floor := at.Add(time.Hour)
	if err := ReconcileDiscovery(&c, previous, floor); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*DiscoveryAuthorization{c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization} {
		if !a.NativeStartFloor.Equal(floor) || len(a.Intervals) != 0 {
			t.Fatalf("paused creation lost floor: %#v", a)
		}
	}
	home := t.TempDir()
	if err := Save(home, c); err != nil {
		t.Fatal(err)
	}
	if _, err := SetPaused(home, false, floor.Add(-time.Nanosecond)); err == nil {
		t.Fatal("early resume accepted")
	}
	resumed, err := SetPaused(home, false, floor)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resumed.CodexDiscoveryGeneration("/new", "/new", floor.Add(-time.Nanosecond), floor); ok {
		t.Fatal("pre-floor native start accepted")
	}
	if _, ok := resumed.CodexDiscoveryGeneration("/new", "/new", floor, floor); !ok {
		t.Fatal("floor equality rejected")
	}
}

func TestCodexLegacyWriterMigrationRenewsUnknownEmptyHistories(t *testing.T) {
	for _, retained := range []bool{false, true} {
		c, at := blanketConfig(t)
		for _, a := range []*DiscoveryAuthorization{c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization} {
			a.NativeStartFloor = time.Time{}
			if !retained {
				a.Intervals = nil
			}
		}
		c.SkillEvidence = SkillEvidence(string(c.EffectiveSkillEvidence()) + legacyCodexWriterMarker)
		raw, err := json.Marshal(configJSON(c))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		doc["schema_version"] = json.RawMessage(`{"version":3,"writer":"codex-scope-v3"}`)
		raw, err = json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		path := filepath.Join(home, "config.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		loaded, found, err := Load(home)
		if err != nil || !found {
			t.Fatal(err)
		}
		old := loaded.CodexCapture.Authorization.Generation
		if err := ProtectIdentityWriter(home); err != nil {
			t.Fatal(err)
		}
		migrated, _, fenced, err := loadConfig(home)
		if err != nil || !fenced {
			t.Fatal(err)
		}
		if !retained {
			if _, ok := migrated.CodexGeneration("/new", "/new", at, at); ok {
				t.Fatal("empty legacy history authorized")
			}
			next := migrated
			next.Paused = true
			if err := ReconcileDiscovery(&next, migrated, at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if next.CodexCapture.Authorization.Generation == old || !next.CodexCapture.Authorization.NativeStartFloor.Equal(at.Add(time.Hour)) {
				t.Fatal("unknown generation was not renewed")
			}
		} else if !migrated.CodexCapture.Authorization.NativeStartFloor.Equal(at) {
			t.Fatal("retained floor lost")
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var prior struct {
			SchemaVersion writerVersion `json:"schema_version"`
		}
		if err := json.Unmarshal(before, &prior); err != nil {
			t.Fatal(err)
		}
		if prior.SchemaVersion.Writer == legacyCodexWriter {
			t.Fatal("old writer can flatten floor")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("prior decoder refusal changed bytes")
		}
	}
}

func TestCodexReconciliationErrorPreservesCallerAndPriorSlices(t *testing.T) {
	c, at := blanketConfig(t)
	previous := c
	c.Paused = true
	source := *c.CodexCapture.SourceAuthorization
	source.Intervals = []DiscoveryInterval{{Start: at.Add(time.Hour)}}
	p := *c.CodexCapture
	p.SourceAuthorization = &source
	previous.CodexCapture = &p
	before := c
	if err := ReconcileDiscovery(&c, previous, at.Add(time.Minute)); err == nil {
		t.Fatal("source clock refusal missing")
	}
	if !reflect.DeepEqual(c, before) {
		t.Fatal("failed reconciliation partially changed caller")
	}
	if !previous.CodexCapture.SourceAuthorization.Intervals[0].End.IsZero() {
		t.Fatal("failed reconciliation mutated previous slice")
	}
}

func TestCodexFloorRollbackSnapshotKeepsFenceWithoutReopeningConsent(t *testing.T) {
	current, at := blanketConfig(t)
	rolled := Config{RetentionDays: 90, MachineID: current.MachineID, Harnesses: current.Harnesses, Archive: current.Archive}
	PreserveWriterFence(&rolled, current)
	home := t.TempDir()
	if err := Save(home, rolled); err != nil {
		t.Fatal(err)
	}
	loaded, found, fenced, err := loadConfig(home)
	if err != nil || !found || !fenced {
		t.Fatal(err)
	}
	if loaded.EffectiveCodexCaptureScope() != CodexIncludedProjects || loaded.CodexCapture.Authorization != nil || loaded.CodexCapture.SourceAuthorization != nil {
		t.Fatal("rollback copied live blanket permission")
	}
	if _, ok := loaded.CodexGeneration("/new", "/new", at, at); ok {
		t.Fatal("rollback opened new starts")
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		SchemaVersion writerVersion `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.SchemaVersion.Writer != codexWriter {
		t.Fatal("rollback downgraded writer fence")
	}
}
