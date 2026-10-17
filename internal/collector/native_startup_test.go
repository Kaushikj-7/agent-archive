package collector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestStartupCandidateAcknowledgementHonorsCancellationAndExactEpoch(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	intent := agentapi.AdmissionIntent{Version: 2, Harness: "muse", NativeMode: agentapi.NativeStartupCandidate, NativeSessionID: "opaque-root", ProjectRoot: "/synthetic/project", ObservedCwd: "/synthetic/project", DestinationID: strings.Repeat("a", 64), ObservedAt: time.Now().UTC(), Event: "SessionStart", NativeAdmission: &archive.NativeAdmission{Contract: "muse-root-admission", HookContract: "muse-root-hooks-v1", RootFingerprint: strings.Repeat("b", 64), PolicyFingerprint: strings.Repeat("c", 64), WindowGeneration: strings.Repeat("d", 32)}}
	candidate := startupCandidate{path: filepath.Join(home, "admission-intents", "startup.json"), intent: intent}
	if err := local.Write(candidate.path, intent); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(candidate.path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := pass{ctx: ctx, local: store}
	if err := p.ackStartupCandidate(candidate); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(candidate.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled startup acknowledgement changed queue", err)
	}
	changed := intent
	proof := *intent.NativeAdmission
	proof.WindowGeneration = strings.Repeat("e", 32)
	changed.NativeAdmission = &proof
	if err := local.Write(candidate.path, changed); err != nil {
		t.Fatal(err)
	}
	before, err = os.ReadFile(candidate.path)
	if err != nil {
		t.Fatal(err)
	}
	p.ctx = t.Context()
	if err := p.ackStartupCandidate(candidate); err == nil {
		t.Fatal("replaced startup epoch acknowledged")
	}
	after, err = os.ReadFile(candidate.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("replaced startup epoch removed", err)
	}
	candidate.intent = changed
	if err := p.ackStartupCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(candidate.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exact startup retained", err)
	}
}

type startupOwnerChange string

const (
	startupOwnerSame         startupOwnerChange = "same"
	startupOwnerLater        startupOwnerChange = "later_same_project"
	startupOwnerOtherProject startupOwnerChange = "other_project"
	startupOwnerOtherCwd     startupOwnerChange = "other_cwd"
	startupOwnerConfiguredID startupOwnerChange = "configured_id"
	startupOwnerRootRevision startupOwnerChange = "root_revision"
	startupOwnerSameCwd      startupOwnerChange = "same_project_cwd"
	startupOwnerExcludedCwd  startupOwnerChange = "excluded_cwd"
)

func TestStartupCandidateExistingOwnerPreservesConflictingProjectAuthority(t *testing.T) {
	t.Parallel()
	for _, change := range []startupOwnerChange{startupOwnerSame, startupOwnerLater, startupOwnerOtherProject, startupOwnerOtherCwd, startupOwnerConfiguredID, startupOwnerRootRevision, startupOwnerSameCwd, startupOwnerExcludedCwd} {
		t.Run(string(change), func(t *testing.T) {
			t.Parallel()
			home, project, other, rootPath := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			// Configured project matching uses physical paths, including macOS /var aliases.
			var err error
			project, err = filepath.EvalSymlinks(project)
			if err != nil {
				t.Fatal(err)
			}
			other, err = filepath.EvalSymlinks(other)
			if err != nil {
				t.Fatal(err)
			}
			rootPath, err = filepath.EvalSymlinks(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			subdir, excluded := filepath.Join(project, "subdir"), filepath.Join(project, "excluded")
			for _, path := range []string{subdir, excluded} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			const nativeID = "opaque-root"
			id, _, err := store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: nativeID})
			if err != nil {
				t.Fatal(err)
			}
			birth := time.Now().UTC().Add(-time.Hour)
			root := archive.NativeRoot{Agent: "muse", Configured: rootPath, Canonical: rootPath, Revision: "one"}
			cfg := config.Config{MachineID: "synthetic", RetentionDays: 90, Harnesses: []string{"muse"}, Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic", Prefix: "fixture"}, DestinationSince: birth.Add(-time.Hour), NativeRoots: []archive.NativeRoot{root}, Archive: archive.Config{Enabled: true, SchemaVersion: 1, Projects: []archive.ProjectActivation{{Root: project, ProjectID: "configured-project-a", Included: true, ActivatedAt: birth.Add(-time.Hour)}, {Root: other, ProjectID: "configured-project-b", Included: true, ActivatedAt: birth.Add(-time.Hour)}}}}
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: excluded, ProjectID: "excluded-project", Included: false})
			cfg, err = config.ReconcileMuseCapture(cfg, config.Config{}, birth.Add(-time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			fingerprint, err := archive.NativeRootFingerprint(root)
			if err != nil {
				t.Fatal(err)
			}
			proof := &archive.NativeAdmission{Contract: "muse-root-admission", HookContract: "muse-root-hooks-v1", RootFingerprint: fingerprint, PolicyFingerprint: cfg.MuseCapture.PolicyFingerprint, WindowGeneration: cfg.MuseCapture.Generation}
			path := filepath.Join(rootPath, "session.jsonl")
			reg := archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: nativeID, Harness: archive.Harness{Name: "muse"}, ProjectRoot: project, ProjectID: "configured-project-a", DestinationID: cfg.DestinationID(), TranscriptPath: path, SessionStartedAt: birth, RegisteredAt: birth.Add(time.Second), AdmittedAt: birth.Add(time.Second), HookObservedAt: birth.Add(time.Second), NativeAdmission: proof, NativeBinding: &archive.NativeSourceBinding{Agent: "muse", Version: 2, NativeID: nativeID, NativeCreatedAt: birth, Cwd: project, Path: path, RootRevision: "one", Checkpoint: &archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 1, SHA256: strings.Repeat("a", 64)}}}
			if err := reg.Validate(); err != nil {
				t.Fatal(err)
			}
			// Seed an already complete durable owner; this test never creates one via startup.
			regPath := filepath.Join(home, "registrations", id+".json")
			if err := local.Write(regPath, reg); err != nil {
				t.Fatal(err)
			}
			ownerBefore, err := os.ReadFile(regPath)
			if err != nil {
				t.Fatal(err)
			}
			intent := agentapi.AdmissionIntent{Version: 2, Harness: "muse", NativeMode: agentapi.NativeStartupCandidate, NativeSessionID: nativeID, ProjectRoot: project, ObservedCwd: project, DestinationID: cfg.DestinationID(), ObservedAt: reg.HookObservedAt, Event: "SessionStart", NativeAdmission: proof}
			candidate := startupCandidate{path: filepath.Join(home, "admission-intents", "duplicate.json"), intent: intent, root: root, projectID: reg.ProjectID}
			switch change {
			case startupOwnerSame:
				// The original observation is already compatible.
			case startupOwnerLater:
				candidate.intent.ObservedAt = candidate.intent.ObservedAt.Add(time.Minute)
			case startupOwnerOtherProject:
				candidate.intent.ProjectRoot, candidate.intent.ObservedCwd, candidate.projectID = other, other, "configured-project-b"
			case startupOwnerOtherCwd:
				candidate.intent.ObservedCwd = other
			case startupOwnerConfiguredID:
				candidate.projectID = "changed-configured-id"
			case startupOwnerRootRevision:
				candidate.root.Revision = "two"
			case startupOwnerSameCwd:
				candidate.intent.ObservedCwd = subdir
			case startupOwnerExcludedCwd:
				candidate.intent.ObservedCwd = excluded
			}
			if err := local.Write(candidate.path, candidate.intent); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(candidate.path)
			if err != nil {
				t.Fatal(err)
			}
			p := pass{ctx: t.Context(), local: store, now: time.Now().UTC()}
			err = p.admitStartupCandidate(candidate)
			accepted := change == startupOwnerSame || change == startupOwnerLater || change == startupOwnerSameCwd
			if accepted {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(candidate.path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("compatible duplicate retained", err)
				}
			} else {
				if err == nil {
					t.Fatal("incompatible duplicate acknowledged")
				}
				after, err := os.ReadFile(candidate.path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("conflicting candidate changed", err)
				}
			}
			ownerAfter, err := os.ReadFile(regPath)
			if err != nil || !bytes.Equal(ownerBefore, ownerAfter) {
				t.Fatal("existing owner changed", err)
			}
		})
	}
}

func TestAdmissionReplayDiagnosticsKeepJoinedLocalFailures(t *testing.T) {
	t.Parallel()
	for _, interrupted := range []error{context.Canceled, context.DeadlineExceeded} {
		p := pass{result: Result{Errors: map[string]error{}}}
		p.recordInitialErrors(nil, nil, interrupted)
		if len(p.result.Errors) != 0 {
			t.Fatal("ordinary interruption reported as failure")
		}
		localFailure := errors.New("synthetic queue read failure")
		joined := errors.Join(interrupted, localFailure)
		p.recordInitialErrors(nil, nil, joined)
		if !errors.Is(p.result.Errors["admission-intents"], localFailure) {
			t.Fatal("joined local failure was hidden")
		}
	}
}
