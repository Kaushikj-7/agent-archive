package collector

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type syntheticNativeProvider struct{}

func (syntheticNativeProvider) Describe(agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return agentapi.SourceSemantics{}, nil
}

func (syntheticNativeProvider) OpenPass(_ context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	return &syntheticNativePass{env: env}, nil
}

type syntheticNativePass struct{ env agentapi.SourceEnvironment }

func (*syntheticNativePass) Close() error { return nil }

func (*syntheticNativePass) Signature(context.Context, agentapi.SourceRef) (agentapi.SourceObservation, error) {
	return agentapi.SourceObservation{}, nil
}

func (p *syntheticNativePass) Read(_ context.Context, ref agentapi.SourceRef, _ agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	file, err := transcriptio.Open(p.env.Files, ref.Path, p.env.Policy)
	if err != nil {
		return nil, err
	}
	return &syntheticNativeSnapshot{Snapshot: file, root: *p.env.NativeRoot, ref: ref}, nil
}

type syntheticNativeSnapshot struct {
	*transcriptio.Snapshot
	root archive.NativeRoot
	ref  agentapi.SourceRef
}

func (s *syntheticNativeSnapshot) Observation() agentapi.SourceObservation {
	return agentapi.SourceObservation{Present: true, Size: s.Length()}
}

func (s *syntheticNativeSnapshot) Input() agentapi.NativeInput {
	return agentapi.NativeInput{File: s.Snapshot}
}

func (s *syntheticNativeSnapshot) ValidateAdmission(context.Context, agentapi.SourceAdmission) error {
	return s.Check()
}

func (s *syntheticNativeSnapshot) AdmissionFacts(context.Context) (agentapi.SourceBinding, error) {
	if err := s.Check(); err != nil {
		return agentapi.SourceBinding{}, err
	}
	return agentapi.SourceBinding{Native: &archive.NativeSourceBinding{Agent: s.root.Agent, Version: 1, NativeID: s.ref.Key, NativeCreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Cwd: "/project", Path: s.ref.Path, StreamID: "stream", RootRevision: s.root.Revision}}, nil
}

func TestNativeRootConfinesFirstUnboundReadAndOpenedAuthority(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session.jsonl")
	if err = os.WriteFile(path, []byte("synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	authority := archive.NativeRoot{Agent: "synthetic", Configured: root, Canonical: root, Revision: "one"}
	for _, pooled := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "shared-pass"}[pooled], func(t *testing.T) {
			var passes *sourcePassSet
			if pooled {
				passes = &sourcePassSet{passes: map[sourcePassKey]agentapi.SourcePass{}}
			}
			r := providerReader{harness: "synthetic", nativeRoot: &authority, ref: agentapi.SourceRef{Path: path, Key: "native"}, admission: agentapi.SourceAdmission{NativeID: "native"}, passes: passes}
			pass, closePass, err := r.pass(t.Context(), syntheticNativeProvider{}, "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = closePass() }()
			snapshot, err := pass.Read(t.Context(), r.ref, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = snapshot.Close() }()
			binding, err := r.snapshotBinding(t.Context(), snapshot)
			if err != nil || binding.Native.RootRevision != "one" {
				t.Fatal("same-handle bootstrap failed", err)
			}
			data, err := io.ReadAll(io.NewSectionReader(snapshot.Input().File, 0, snapshot.Input().File.Length()))
			if err != nil || string(data) != "synthetic\n" {
				t.Fatal("opened snapshot changed", err)
			}
			changed := authority
			changed.Revision = "two"
			r.nativeRoot = &changed
			if _, err = r.snapshotBinding(t.Context(), snapshot); err == nil {
				t.Fatal("changed root revision accepted")
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err = os.WriteFile(outside, []byte("outside\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, escape := range []string{outside, filepath.Join(root, "link")} {
				if escape != outside {
					if err = os.Symlink(outside, escape); err != nil {
						t.Fatal(err)
					}
					defer func() { _ = os.Remove(escape) }()
				}
				if unexpected, err := pass.Read(t.Context(), agentapi.SourceRef{Path: escape}, agentapi.ReadLimits{}); err == nil {
					_ = unexpected.Close()
					t.Fatal("unbound native read escaped committed root")
				}
			}
		})
	}
	reg := archive.SessionRegistration{Harness: archive.Harness{Name: "synthetic"}, NativeSessionID: "native", TranscriptPath: path}
	if _, ok := newSourceReader(reg, Options{NativeRoots: []archive.NativeRoot{authority}}); ok {
		t.Fatal("surviving locator reauthorized without original binding")
	}
}
