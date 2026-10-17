package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestPreparedNativeAdmissionPolicyChangesBeforeEachOwnerCAS(t *testing.T) {
	t.Parallel()
	mutations := []struct {
		name   string
		change func(*config.Config)
	}{
		{name: "pause", change: func(c *config.Config) { c.Paused = true; c.PauseGeneration = "paused" }},
		{name: "destination", change: func(c *config.Config) { c.Storage.Prefix += "-changed" }},
		{name: "root", change: func(c *config.Config) { c.NativeRoots[0].Revision += "-changed" }},
		{name: "project_excluded", change: func(c *config.Config) { c.Archive.Projects[0].Included = false }},
		{name: "project_identity", change: func(c *config.Config) { c.Archive.Projects[0].ProjectID = "another-configured-project" }},
		{name: "activation_after_native_T", change: func(c *config.Config) {
			c.Archive.Projects[0].ActivatedAt = time.UnixMicro(1792083855882704).UTC().Add(time.Second)
		}},
	}
	for _, mutation := range mutations {
		for _, registrationStage := range []bool{false, true} {
			stage := "reservation"
			if registrationStage {
				stage = "registration"
			}
			t.Run(mutation.name+"/"+stage, func(t *testing.T) {
				t.Parallel()
				fixture := newNativeAdmissionFixture(t)
				prepared, err := fixture.prepare(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				called := false
				change := func() {
					if called {
						return
					}
					called = true
					current, found, err := config.Load(fixture.store.home)
					if err != nil || !found {
						t.Fatal(err)
					}
					next := current
					next.NativeRoots = append([]archive.NativeRoot(nil), current.NativeRoots...)
					next.Archive.Projects = append([]archive.ProjectActivation(nil), current.Archive.Projects...)
					mutation.change(&next)
					next, err = config.ReconcileMuseCapture(next, current, current.MuseCapture.Since.Add(time.Second))
					if err != nil {
						t.Fatal(err)
					}
					if err := config.Save(fixture.store.home, next); err != nil {
						t.Fatal(err)
					}
				}
				if registrationStage {
					fixture.store.onWriteSync = change
				} else {
					fixture.store.onIndexSync = change
				}
				if _, err := fixture.store.CommitNativeAdmission(t.Context(), prepared); err == nil {
					t.Fatal("changed policy committed owner")
				}
				if !called {
					t.Fatal("policy seam not reached")
				}
				if _, found, err := fixture.store.LoadRegistration(prepared.reg.ArchiveSessionID); err != nil || found {
					t.Fatal("policy race exposed owner", found, err)
				}
				key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: fixture.candidate.Intent.NativeSessionID}
				entry, found, err := fixture.store.readQualifiedIndex(key)
				if err != nil || found != registrationStage || found && !supportedNativeReservation(entry.NativeReservation) {
					t.Fatal("wrong reservation boundary", entry, found, err)
				}
				if err := prepared.Close(); err != nil {
					t.Fatal(err)
				}
				fixture.assertReleased(t)
			})
		}
	}
}

func TestPreparedNativeAdmissionCompetingAbsentOwnerCAS(t *testing.T) {
	t.Parallel()
	fixture := newNativeAdmissionFixture(t)
	first, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := fixture.store.CommitNativeAdmission(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CommitNativeAdmission(t.Context(), second); err == nil {
		t.Fatal("stale absent-owner preparation overwrote owner")
	}
	current, found, err := fixture.store.LoadRegistration(owner.ArchiveSessionID)
	if err != nil || !found || current.NativeAdmission == nil || current.NativeBinding == nil {
		t.Fatal("complete original owner lost", found, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleased(t)
}

type nativeAdmissionRevocation string

const (
	nativePauseResumeRevocation  nativeAdmissionRevocation = "pause_resume_ABA"
	nativeSetupPendingRevocation nativeAdmissionRevocation = "setup_pending"
	nativeRemovalRevocation      nativeAdmissionRevocation = "removed"
)

func TestPreparedNativeAdmissionRevocationAtRegistrationStage(t *testing.T) {
	t.Parallel()
	for _, name := range []nativeAdmissionRevocation{nativePauseResumeRevocation, nativeSetupPendingRevocation, nativeRemovalRevocation} {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			fixture := newNativeAdmissionFixture(t)
			prepared, err := fixture.prepare(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			called := false
			fixture.store.onWriteSync = func() {
				if called {
					return
				}
				called = true
				switch name {
				case nativePauseResumeRevocation:
					original, found, err := config.Load(fixture.store.home)
					if err != nil || !found {
						t.Fatal(err)
					}
					paused := original
					paused.Paused = true
					paused.PauseGeneration = "paused"
					paused, err = config.ReconcileMuseCapture(paused, original, original.MuseCapture.Since.Add(time.Second))
					if err != nil {
						t.Fatal(err)
					}
					if err := config.Save(fixture.store.home, paused); err != nil {
						t.Fatal(err)
					}
					resumed := original
					resumed.PauseGeneration = "resumed"
					resumed, err = config.ReconcileMuseCapture(resumed, paused, original.MuseCapture.Since.Add(2*time.Second))
					if err != nil {
						t.Fatal(err)
					}
					if err := config.Save(fixture.store.home, resumed); err != nil {
						t.Fatal(err)
					}
				case nativeSetupPendingRevocation:
					if err := os.WriteFile(filepath.Join(fixture.store.home, "setup-transaction.json"), []byte(`{"pending":true}`), 0600); err != nil {
						t.Fatal(err)
					}
				case nativeRemovalRevocation:
					if err := fixture.store.RecordRemoval("muse", fixture.candidate.Intent.NativeSessionID, RemovalReasonUndo, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := fixture.store.CommitNativeAdmission(t.Context(), prepared); err == nil {
				t.Fatal("revoked startup committed")
			}
			if !called {
				t.Fatal("registration seam not reached")
			}
			if _, found, err := fixture.store.LoadRegistration(prepared.reg.ArchiveSessionID); err != nil || found {
				t.Fatal("revocation exposed owner", found, err)
			}
			if err := prepared.Close(); err != nil {
				t.Fatal(err)
			}
			fixture.assertReleased(t)
		})
	}
}
