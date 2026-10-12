package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

type configObservationCase string

const (
	observationSupported    configObservationCase = "supported"
	observationLegacyFloor  configObservationCase = "legacy-five-to-seven"
	observationFuture       configObservationCase = "future"
	observationCorrupt      configObservationCase = "corrupt"
	observationFutureRead   configObservationCase = "future-read"
	observationCorruptRead  configObservationCase = "corrupt-read"
	observationMissing      configObservationCase = "missing"
	observationDirectory    configObservationCase = "directory"
	observationReadError    configObservationCase = "read-error"
	observationUnsafeHome   configObservationCase = "unsafe-home"
	observationReplacedHome configObservationCase = "replaced-home"
)

func TestRootedConfigReadObservationClassification(t *testing.T) {
	for _, kind := range []configObservationCase{observationSupported, observationLegacyFloor, observationFuture, observationCorrupt, observationFutureRead, observationCorruptRead, observationMissing, observationDirectory, observationReadError, observationUnsafeHome, observationReplacedHome} {
		t.Run(string(kind), func(t *testing.T) {
			home := durableTestHome(t)
			initial := Config{RetentionDays: 90, MachineID: "synthetic", GenerationProtection: kind == observationLegacyFloor, CodexHistoryProtection: kind == observationLegacyFloor}
			if err := Save(home, initial); err != nil {
				t.Fatal(err)
			}
			if kind == observationFutureRead || kind == observationCorruptRead {
				raw := []byte(`{"schema_version":{"version":8,"writer":"publication-composition-v8"},"publication_composition_protection":true}`)
				if kind == observationCorruptRead {
					raw = []byte("{")
				}
				if err := os.WriteFile(filepath.Join(home, "config.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			held, err := local.OpenRootedHome(home)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := held.Close(); err != nil {
					t.Error(err)
				}
			}()
			before, err := held.Root.Lstat("config.json")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := held.Root.ReadFile("config.json")
			if err != nil {
				t.Fatal(err)
			}
			var readErr error
			switch kind {
			case observationSupported, observationFutureRead, observationCorruptRead:
				err = local.RootedWrite(held.Root, "config.json", Config{RetentionDays: 90, MachineID: "replacement"})
			case observationLegacyFloor:
				initial.DurableStorageProtection = true
				err = local.RootedWrite(held.Root, "config.json", initial)
			case observationFuture, observationCorrupt:
				raw := []byte(`{"schema_version":{"version":8,"writer":"publication-composition-v8"},"publication_composition_protection":true}`)
				if kind == observationCorrupt {
					raw = []byte("{")
				}
				err = os.WriteFile(filepath.Join(home, "replacement"), raw, 0600)
				if err == nil {
					err = os.Rename(filepath.Join(home, "replacement"), filepath.Join(home, "config.json"))
				}
			case observationMissing, observationDirectory, observationReadError:
				err = held.Root.Remove("config.json")
				if err == nil && kind == observationDirectory {
					err = held.Root.Mkdir("config.json", 0700)
				}
				if err == nil && kind == observationReadError {
					_, readErr = held.Root.ReadFile("config.json")
					if !errors.Is(readErr, os.ErrNotExist) {
						t.Fatalf("expected actual failed read: %v", readErr)
					}
					err = local.RootedWrite(held.Root, "config.json", Config{RetentionDays: 90, MachineID: "replacement"})
				}
			case observationUnsafeHome:
				err = os.Chmod(home, 0755)
			case observationReplacedHome:
				err = os.Rename(home, home+"-old")
				if err == nil {
					t.Cleanup(func() { _ = os.Remove(home); _ = os.Rename(home+"-old", home) })
					err = os.Mkdir(home, 0700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			observation := rootedConfigReadUnchanged(held, before, raw, readErr)
			changed := kind == observationSupported || kind == observationLegacyFloor || kind == observationFuture || kind == observationCorrupt
			if observation == nil || errors.Is(observation, errRootedConfigObservationChanged) != changed {
				t.Fatalf("observation classification: %v", observation)
			}
			if changed {
				cfg, present, err := loadDurableStoragePreflight(held, time.Now().Add(time.Second))
				if kind == observationSupported || kind == observationLegacyFloor {
					if err != nil || !present {
						t.Fatalf("supported replacement: %v", err)
					}
					if kind == observationLegacyFloor && (cfg.SchemaVersion != 7 || !cfg.DurableStorageProtection || !cfg.GenerationProtection || !cfg.CodexHistoryProtection) {
						t.Fatal("replacement lost actual protected floor")
					}
				} else if err == nil || errors.Is(err, errRootedConfigObservationChanged) {
					t.Fatalf("unknown replacement was not refused by complete decoder: %v", err)
				}
			}
			if _, err := os.Lstat(filepath.Join(home, "hooks.lock")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read preflight allocated a lock: %v", err)
			}
		})
	}
}

func TestDurablePreflightExpiredDeadlineAllocatesNothing(t *testing.T) {
	home := durableTestHome(t)
	if err := Save(home, Config{RetentionDays: 90, MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	held, err := local.OpenRootedHome(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Error(err)
		}
	}()
	before, err := held.Root.ReadFile("config.json")
	if err != nil {
		t.Fatal(err)
	}
	_, present, err := loadDurableStoragePreflight(held, time.Now().Add(-time.Second))
	if !errors.Is(err, local.ErrBusy) || present {
		t.Fatalf("expired preflight: present=%v err=%v", present, err)
	}
	after, err := held.Root.ReadFile("config.json")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("expired preflight changed config", err)
	}
	if _, err := held.Root.Lstat("hooks.lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired preflight allocated lock: %v", err)
	}
}

func TestDurableConcurrentInitialFloorLargeConfiguration(t *testing.T) {
	home := durableTestHome(t)
	if err := Save(home, Config{RetentionDays: 90, MachineID: strings.Repeat("synthetic", 64*1024)}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- WithDurableStorage(home, func(g DurableStorageGuard) error { return g.CheckHome(home) })
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, present, err := Load(home)
	if err != nil || !present || !cfg.DurableStorageProtection || cfg.SchemaVersion != 7 {
		t.Fatalf("actual concurrent floor: present=%v version=%d err=%v", present, cfg.SchemaVersion, err)
	}
}
