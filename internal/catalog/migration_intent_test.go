package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

type migrationIntentFault struct {
	*qualifiedStore
	stage   string
	after   bool
	fired   bool
	blocked bool
	writes  int
}

func migrationWriteStage(key string, raw []byte) string {
	if key == MigrationKey {
		var checkpoint CatalogMigration
		if json.Unmarshal(raw, &checkpoint) != nil {
			return "invalid"
		}
		return "checkpoint-" + string(checkpoint.Phase)
	}
	if key != CoordinatorKey {
		return "other"
	}
	var state admissions
	if json.Unmarshal(raw, &state) != nil {
		return "invalid"
	}
	if state.Intent == nil {
		return "clear-intent"
	}
	kind := string(state.Intent.Kind)
	if state.Mode == admissionRollback {
		return kind + "-deactivate"
	}
	if state.Seal != "" {
		return kind + "-seal"
	}
	return kind + "-intent"
}

func (s *migrationIntentFault) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	s.writes++
	if s.stage == "" || s.fired || migrationWriteStage(key, raw) != s.stage {
		return s.qualifiedStore.PutConditional(ctx, key, raw, condition)
	}
	s.fired = true
	if s.after {
		if _, err := s.qualifiedStore.PutConditional(ctx, key, raw, condition); err != nil {
			return "", err
		}
		s.blocked = true // Model crash/unknown acknowledgment until fresh rerun.
	}
	return "", errors.New("private migration transition interrupted")
}

func (s *migrationIntentFault) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	if s.blocked {
		return nil, storage.CatalogObjectVersion{}, errors.New("private crash prevented acknowledgment readback")
	}
	return s.qualifiedStore.GetCatalogVersion(ctx, key, limit)
}

func TestMigrationInitializationIntentResumesEveryFirstWriteBoundary(t *testing.T) {
	for _, stage := range []string{"initialize-intent", "initialize-seal", "checkpoint-copying", "clear-intent"} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after=%t", stage, after), func(t *testing.T) {
				source, target, src, dst, authority := migrationFixture(t, 0)
				fault := &migrationIntentFault{qualifiedStore: target, stage: stage, after: after}
				if _, err := OpenMigration(t.Context(), source, fault, src, dst, authority); err == nil || !fault.fired {
					t.Fatal("initial transition interruption not reached", err)
				}
				fault.blocked = false
				restarted, err := OpenMigration(t.Context(), source, fault, src, dst, authority)
				if err != nil || restarted.State.Phase != MigrationCopying {
					t.Fatal("fresh initial resume", err)
				}
				state, _, err := restarted.writer.Coordinator().read(t.Context())
				if err != nil || state.Intent != nil || state.Seal != restarted.State.Owner || len(state.Owners) != 0 {
					t.Fatal("initial authority incomplete", err)
				}
				done, err := restarted.Step(t.Context())
				if err != nil || !done {
					t.Fatal("empty archive migration", done, err)
				}
			})
		}
	}
}

func TestMigrationInitializationIntentRejectsForeignAndMixedNamespace(t *testing.T) {
	for _, kind := range []migrationInitializationFault{migrationMixed, migrationForeignProof, migrationBareCoordinator} {
		t.Run(string(kind), func(t *testing.T) {
			source, target, src, dst, authority := migrationFixture(t, 0)
			fault := &migrationIntentFault{qualifiedStore: target, stage: "initialize-seal"}
			if _, err := OpenMigration(t.Context(), source, fault, src, dst, authority); err == nil {
				t.Fatal("fixture did not stop before seal")
			}
			raw, err := target.Get(t.Context(), CoordinatorKey)
			if err != nil {
				t.Fatal(err)
			}
			var state admissions
			if err = json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case migrationMixed:
				err = target.Put(t.Context(), "sessions/foreign/metadata.json", []byte(`{}`))
			case migrationForeignProof:
				state.Intent.State.Proof = "foreign"
			case migrationBareCoordinator:
				state.Intent = nil
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind != migrationMixed {
				raw, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err = target.Put(t.Context(), CoordinatorKey, raw); err != nil {
					t.Fatal(err)
				}
			}
			before := fault.writes
			if _, err = OpenMigration(t.Context(), source, fault, src, dst, authority); err == nil || fault.writes != before {
				t.Fatal("foreign initialization adopted", err, fault.writes-before)
			}
		})
	}
}

func TestMigrationRollbackIntentResumesEveryTransition(t *testing.T) {
	for _, stage := range []string{"rollback-intent", "rollback-seal", "checkpoint-rollback-preparing", "rollback-deactivate", "checkpoint-rollback", "clear-intent"} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after=%t", stage, after), func(t *testing.T) {
				source, target, src, dst, authority := migrationFixture(t, 0)
				fault := &migrationIntentFault{qualifiedStore: target}
				migration, err := OpenMigration(t.Context(), source, fault, src, dst, authority)
				if err != nil {
					t.Fatal(err)
				}
				if done, err := migration.Step(t.Context()); err != nil || !done {
					t.Fatal(done, err)
				}
				if err = migration.Activate(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err = migration.FinishActivation(t.Context()); err != nil {
					t.Fatal(err)
				}
				fault.stage, fault.after = stage, after
				if err = migration.Rollback(t.Context()); err == nil || !fault.fired {
					t.Fatal("rollback interruption not reached", err)
				}
				fault.blocked = false
				restarted, err := OpenMigration(t.Context(), source, fault, src, dst, authority)
				if err != nil {
					t.Fatal("fresh rollback open", err)
				}
				if err = restarted.Rollback(t.Context()); err != nil {
					t.Fatal("fresh rollback resume", err)
				}
				if err = restarted.Rollback(t.Context()); err != nil {
					t.Fatal("completed rollback config retry", err)
				}
				state, _, err := restarted.writer.Coordinator().read(t.Context())
				if err != nil || state.Intent != nil || state.Mode != admissionRollback || state.Seal != restarted.State.Owner {
					t.Fatal("rollback authority incomplete", err)
				}
			})
		}
	}
}

func TestRollbackIntentRefusesLiveOwnerAndChangedHeadWithoutFencing(t *testing.T) {
	source, target, src, dst, authority := migrationFixture(t, 0)
	counted := &migrationIntentFault{qualifiedStore: target}
	migration, err := OpenMigration(t.Context(), source, counted, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	if done, err := migration.Step(t.Context()); err != nil || !done {
		t.Fatal(done, err)
	}
	if err = migration.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = migration.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	coordinator := migration.writer.Coordinator()
	if err = coordinator.Admit(t.Context(), "live-writer", "exact-lifecycle", nil); err != nil {
		t.Fatal(err)
	}
	before := counted.writes
	if err = migration.Rollback(t.Context()); err == nil || before != counted.writes {
		t.Fatal("live writer fenced", err)
	}
	state, _, err := coordinator.read(t.Context())
	if err != nil || state.Intent != nil || state.Seal != "" || len(state.Owners) != 1 {
		t.Fatal("live writer authority changed", err)
	}
	if err = coordinator.Complete(t.Context(), "live-writer", "exact-lifecycle"); err != nil {
		t.Fatal(err)
	}
	fresh, err := OpenMigration(t.Context(), source, counted, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	next := mutation(t, fresh.writer, "post-cutover")
	if _, err = fresh.writer.Commit(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	before = counted.writes
	if err = fresh.Rollback(t.Context()); err == nil || before != counted.writes {
		t.Fatal("changed head rollback intent installed", err)
	}
	state, _, err = coordinator.read(t.Context())
	if err != nil || state.Intent != nil || state.Seal != "" {
		t.Fatal("changed head was fenced", err)
	}
}

func TestMigrationIntentRefusesAlteredCheckpointDescriptor(t *testing.T) {
	for _, phase := range []string{"initialize", "rollback"} {
		for _, field := range []string{"owner", "source-profile"} {
			t.Run(phase+"/"+field, func(t *testing.T) {
				source, target, src, dst, authority := migrationFixture(t, 0)
				stage := ""
				if phase == "initialize" {
					stage = "clear-intent"
				}
				fault := &migrationIntentFault{qualifiedStore: target, stage: stage}
				migration, err := OpenMigration(t.Context(), source, fault, src, dst, authority)
				if phase == "initialize" {
					if err == nil {
						t.Fatal("initial checkpoint fixture did not interrupt")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if done, err := migration.Step(t.Context()); err != nil || !done {
						t.Fatal(done, err)
					}
					if err = migration.Activate(t.Context()); err != nil {
						t.Fatal(err)
					}
					if err = migration.FinishActivation(t.Context()); err != nil {
						t.Fatal(err)
					}
					fault.stage = "rollback-seal"
					if err = migration.Rollback(t.Context()); err == nil {
						t.Fatal("rollback predecessor fixture did not interrupt")
					}
				}
				raw, err := target.Get(t.Context(), MigrationKey)
				if err != nil {
					t.Fatal(err)
				}
				checkpoint, err := decodeMigration(raw)
				if err != nil {
					t.Fatal(err)
				}
				if field == "owner" {
					checkpoint.Owner, err = NewMutationID()
					if err != nil {
						t.Fatal(err)
					}
				} else {
					identity := destinationIdentity(checkpoint.Source)
					checkpoint.Source.AWSProfile = "foreign-profile"
					if destinationIdentity(checkpoint.Source) != identity {
						t.Fatal("fixture changed canonical identity rather than descriptor")
					}
				}
				if err = checkpoint.validate(); err != nil {
					t.Fatal("altered checkpoint must remain structurally valid", err)
				}
				raw, err = json.Marshal(checkpoint)
				if err != nil {
					t.Fatal(err)
				}
				if err = target.Put(t.Context(), MigrationKey, raw); err != nil {
					t.Fatal(err)
				}
				before := fault.writes
				if _, err = OpenMigration(t.Context(), source, fault, src, dst, authority); err == nil || fault.writes != before {
					t.Fatal("unrelated complete checkpoint adopted", err)
				}
			})
		}
	}
}

type migrationInitializationFault string

const (
	migrationMixed           migrationInitializationFault = "mixed"
	migrationForeignProof    migrationInitializationFault = "foreign-proof"
	migrationBareCoordinator migrationInitializationFault = "bare-coordinator"
)
