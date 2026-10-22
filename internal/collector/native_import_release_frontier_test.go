package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

type historicalReleaseFiles map[string][]byte

func historicalReleasePaths(f historicalNativeStage) []string {
	id := f.reg.ArchiveSessionID
	return []string{
		filepath.Join("sessions", id, "import-stage", "stage.json"),
		filepath.Join("sessions", id, "import-stage", "release.json"),
		filepath.Join("sessions", id, "import-stage", "release-progress.json"),
		filepath.Join("pending", id+".json"), filepath.Join("requests", id+".json"),
		filepath.Join("published", id+".json"), filepath.Join("registrations", id+".json"),
	}
}

func captureHistoricalReleaseFiles(t *testing.T, f historicalNativeStage) historicalReleaseFiles {
	t.Helper()
	files := historicalReleaseFiles{}
	for _, relative := range historicalReleasePaths(f) {
		raw, err := os.ReadFile(filepath.Join(f.store.Home(), relative))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		files[relative] = raw
	}
	return files
}

func restoreHistoricalReleaseFiles(t *testing.T, f historicalNativeStage, files historicalReleaseFiles) {
	t.Helper()
	for relative, raw := range files {
		path := filepath.Join(f.store.Home(), relative)
		if raw == nil {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertHistoricalReleaseFiles(t *testing.T, f historicalNativeStage, expected historicalReleaseFiles) {
	t.Helper()
	actual := captureHistoricalReleaseFiles(t, f)
	for path, raw := range expected {
		if !bytes.Equal(actual[path], raw) || (actual[path] == nil) != (raw == nil) {
			t.Fatal("release refusal/read changed protected file", path)
		}
	}
}

type releaseFrontierPhase string

const (
	frontierAck           releaseFrontierPhase = "ack"
	frontierReceipt       releaseFrontierPhase = "receipt"
	frontierVerified      releaseFrontierPhase = "verified"
	frontierLocalComplete releaseFrontierPhase = "local-complete"
	frontierPendingAbsent releaseFrontierPhase = "pending-absent"
	frontierBothAbsent    releaseFrontierPhase = "both-absent"
	frontierReleased      releaseFrontierPhase = "released"
)

type releaseOwnerModifier string

const (
	ownerAttachedOriginal releaseOwnerModifier = "attached-original"
	ownerDetachedOriginal releaseOwnerModifier = "detached-original"
	ownerDetachedLater    releaseOwnerModifier = "detached-later"
	ownerAttachedLater    releaseOwnerModifier = "attached-later"
)

type releaseRoute string

const (
	releaseRouteDirect releaseRoute = "direct"
	releaseRouteResume releaseRoute = "resume"
)

type absentStageChange string

const (
	absentStageOwner          absentStageChange = "owner"
	absentStageForeignReceipt absentStageChange = "foreign-receipt"
	absentStageProgressDigest absentStageChange = "progress-digest"
	absentStageMissingReceipt absentStageChange = "missing-receipt"
	absentStageMissingAck     absentStageChange = "missing-ack"
	absentStageFutureProgress absentStageChange = "future-progress"
)

// These snapshots come from a real typed ACK and completed remote readback.
// Only interrupted phases consistent with those exact bytes are restored.
func historicalReleaseFrontier(t *testing.T, phase releaseFrontierPhase) historicalNativeStage {
	t.Helper()
	f := newHistoricalNativeStage(t)
	f.remote.failRelease = true
	result, err := Run(t.Context(), f.store, f.remote, f.opts)
	closeStagedControlResult(result)
	if err != nil || len(result.Errors) == 0 {
		t.Fatal("expected actual post-ACK readback interruption", err, result.Errors)
	}
	initial := captureHistoricalReleaseFiles(t, f)
	f.remote.failRelease = false
	result, err = Run(t.Context(), f.store, f.remote, f.opts)
	closeStagedControlResult(result)
	if err != nil || len(result.Errors) != 0 {
		t.Fatal("actual immutable release receipt", err, result.Errors)
	}
	if phase == frontierReleased {
		return f
	}
	complete := captureHistoricalReleaseFiles(t, f)
	id := f.reg.ArchiveSessionID
	stage := filepath.Join("sessions", id, "import-stage", "stage.json")
	receipt := filepath.Join("sessions", id, "import-stage", "release.json")
	progress := filepath.Join("sessions", id, "import-stage", "release-progress.json")
	pending := filepath.Join("pending", id+".json")
	request := filepath.Join("requests", id+".json")
	complete[stage], complete[pending], complete[request] = initial[stage], initial[pending], initial[request]
	if complete[receipt] == nil || complete[progress] == nil || initial[stage] == nil || initial[pending] == nil {
		t.Fatal("actual completed/initial controls absent")
	}
	switch phase {
	case frontierReleased:
	case frontierAck:
		complete[receipt], complete[progress] = nil, nil
	case frontierReceipt:
		complete[progress] = nil
	case frontierVerified, frontierLocalComplete, frontierPendingAbsent, frontierBothAbsent:
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(complete[progress], &doc); err != nil {
			t.Fatal(err)
		}
		value := string(frontierLocalComplete)
		if phase == frontierVerified {
			value = string(frontierVerified)
		}
		doc["phase"], err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		complete[progress], err = json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if phase != frontierVerified {
			// Covered request removal precedes the local-complete write.
			complete[request] = nil
		}
		if phase == frontierPendingAbsent || phase == frontierBothAbsent {
			complete[pending] = nil
		}
		if phase == frontierBothAbsent {
			complete[stage] = nil
		}
	default:
		t.Fatal("unknown frontier", phase)
	}
	restoreHistoricalReleaseFiles(t, f, complete)
	return f
}

func modifyHistoricalReleaseOwner(t *testing.T, f *historicalNativeStage, modifier releaseOwnerModifier) {
	t.Helper()
	if modifier == ownerDetachedOriginal || modifier == ownerDetachedLater {
		path := filepath.Join(f.store.Home(), "imports", f.reg.ImportBatch.Recorded()+".json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var control archive.NativeImportBatchControl
		if err := json.Unmarshal(raw, &control); err != nil {
			t.Fatal(err)
		}
		at := f.at.Add(time.Hour)
		control.UndoneAt = &at
		if err := local.Write(path, control); err != nil {
			t.Fatal(err)
		}
		changed, err := f.store.DetachNativeImportBatchMembership(t.Context(), control.ID, f.reg.ArchiveSessionID, f.reg.NativeImport.Membership.ReceiptID)
		if err != nil || !changed {
			t.Fatal("actual exact membership detach", changed, err)
		}
		f.reg, _, err = f.store.LoadRegistration(f.reg.ArchiveSessionID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if modifier == ownerAttachedLater || modifier == ownerDetachedLater {
		if err := f.store.SaveRequest(f.reg.ArchiveSessionID, "later-independent", f.at.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
}

// shortReleaseFrontierModifier is the one owner modifier short mode runs for
// each phase. Together they cover every modifier, a refused later request
// before release and an allowed one after it; the default suite runs every
// phase with every modifier.
var shortReleaseFrontierModifier = map[releaseFrontierPhase]releaseOwnerModifier{
	frontierAck:           ownerAttachedLater,
	frontierReceipt:       ownerDetachedOriginal,
	frontierVerified:      ownerAttachedOriginal,
	frontierLocalComplete: ownerDetachedLater,
	frontierPendingAbsent: ownerDetachedOriginal,
	frontierBothAbsent:    ownerDetachedLater,
	frontierReleased:      ownerAttachedLater,
}

func releaseFrontierModifiers(phase releaseFrontierPhase) []releaseOwnerModifier {
	if testing.Short() {
		return []releaseOwnerModifier{shortReleaseFrontierModifier[phase]}
	}
	return []releaseOwnerModifier{ownerAttachedOriginal, ownerDetachedOriginal, ownerDetachedLater, ownerAttachedLater}
}

func TestNativeImportPublicReleaseFrontiers(t *testing.T) {
	for _, phase := range []releaseFrontierPhase{frontierAck, frontierReceipt, frontierVerified, frontierLocalComplete, frontierPendingAbsent, frontierBothAbsent, frontierReleased} {
		t.Run(string(phase), func(t *testing.T) {
			for _, modifier := range releaseFrontierModifiers(phase) {
				t.Run(string(modifier), func(t *testing.T) {
					f := historicalReleaseFrontier(t, phase)
					modifyHistoricalReleaseOwner(t, &f, modifier)
					before := captureHistoricalReleaseFiles(t, f)
					if _, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at, "", f.opts.Sources); err != nil {
						t.Fatal("independent public local read before cleanup", err)
					}
					assertHistoricalReleaseFiles(t, f, before)
					refuse := modifier == ownerAttachedLater && phase != frontierReleased
					for _, route := range []releaseRoute{releaseRouteDirect, releaseRouteResume} {
						t.Run(string(route), func(t *testing.T) {
							restoreHistoricalReleaseFiles(t, f, before)
							reopened, err := state.Open(f.store.Home())
							if err != nil {
								t.Fatal(err)
							}
							if route == releaseRouteDirect {
								err = reopened.VerifyAndBeginStagedRelease(t.Context(), f.reg.ArchiveSessionID, f.remote, f.opts.CollectorGuard)
							} else {
								var holds map[string]error
								var closeResult func()
								holds, closeResult, err = reopened.ResumeStagedReleases(t.Context(), f.remote, f.opts.CollectorGuard)
								if closeResult != nil {
									defer closeResult()
								}
								if err == nil {
									err = holds[f.reg.ArchiveSessionID]
								}
							}
							if refuse {
								if err == nil {
									t.Fatal("attached later request extended frozen cleanup")
								}
								assertHistoricalReleaseFiles(t, f, before)
								return
							}
							if err != nil {
								t.Fatal("public release frontier", err)
							}
							if err := reopened.NativeImportRetainedAuthority(t.Context(), f.reg); err != nil {
								t.Fatal("cleanup did not finish exact release", err)
							}
							if modifier == ownerDetachedLater || modifier == ownerAttachedLater {
								raw, err := os.ReadFile(filepath.Join(f.store.Home(), "requests", f.reg.ArchiveSessionID+".json"))
								if err != nil || !bytes.Equal(raw, before[filepath.Join("requests", f.reg.ArchiveSessionID+".json")]) {
									t.Fatal("later request changed", err)
								}
							}
						})
					}
				})
			}
		})
	}
}

func TestNativeImportLocalCompleteAbsentStageRefusesChangedAuthority(t *testing.T) {
	for _, change := range []absentStageChange{absentStageOwner, absentStageForeignReceipt, absentStageProgressDigest, absentStageMissingReceipt, absentStageMissingAck, absentStageFutureProgress} {
		t.Run(string(change), func(t *testing.T) {
			f := historicalReleaseFrontier(t, frontierBothAbsent)
			modifyHistoricalReleaseOwner(t, &f, ownerDetachedLater)
			id := f.reg.ArchiveSessionID
			relative := filepath.Join("sessions", id, "import-stage", "release-progress.json")
			switch change {
			case absentStageOwner:
				relative = filepath.Join("registrations", id+".json")
				raw, err := os.ReadFile(filepath.Join(f.store.Home(), relative))
				if err != nil {
					t.Fatal(err)
				}
				var reg archive.SessionRegistration
				if err := json.Unmarshal(raw, &reg); err != nil {
					t.Fatal(err)
				}
				reg.AdmittedAt = reg.AdmittedAt.Add(time.Second)
				if err := local.Write(filepath.Join(f.store.Home(), relative), reg); err != nil {
					t.Fatal(err)
				}
			case absentStageForeignReceipt:
				other := historicalReleaseFrontier(t, frontierBothAbsent)
				for _, name := range []string{"release.json", "release-progress.json"} {
					raw, err := os.ReadFile(filepath.Join(other.store.Home(), "sessions", other.reg.ArchiveSessionID, "import-stage", name))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(f.store.Home(), "sessions", id, "import-stage", name), raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case absentStageMissingReceipt, absentStageMissingAck:
				if change == absentStageMissingReceipt {
					relative = filepath.Join("sessions", id, "import-stage", "release.json")
				} else {
					relative = filepath.Join("published", id+".json")
				}
				if err := os.Remove(filepath.Join(f.store.Home(), relative)); err != nil {
					t.Fatal(err)
				}
			case absentStageProgressDigest, absentStageFutureProgress:
				value := []byte(`{"version":999}`)
				if change == absentStageProgressDigest {
					raw, err := os.ReadFile(filepath.Join(f.store.Home(), relative))
					if err != nil {
						t.Fatal(err)
					}
					var doc map[string]json.RawMessage
					if err := json.Unmarshal(raw, &doc); err != nil {
						t.Fatal(err)
					}
					doc["receipt_sha256"] = json.RawMessage(`"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"`)
					value, err = json.Marshal(doc)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(f.store.Home(), relative), value, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := captureHistoricalReleaseFiles(t, f)
			if _, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at, "", f.opts.Sources); err == nil {
				t.Fatal("changed absent-stage authority allowed local read")
			}
			assertHistoricalReleaseFiles(t, f, before)
			reopened, err := state.Open(f.store.Home())
			if err != nil {
				t.Fatal(err)
			}
			if err := reopened.VerifyAndBeginStagedRelease(t.Context(), id, f.remote, f.opts.CollectorGuard); err == nil {
				t.Fatal("changed absent-stage authority allowed direct release")
			}
			assertHistoricalReleaseFiles(t, f, before)
			holds, closeResult, err := reopened.ResumeStagedReleases(t.Context(), f.remote, f.opts.CollectorGuard)
			if closeResult != nil {
				defer closeResult()
			}
			if err == nil && holds[id] == nil {
				t.Fatal("changed absent-stage authority allowed public resume")
			}
			assertHistoricalReleaseFiles(t, f, before)
		})
	}
}
