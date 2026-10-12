package backfill

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeletedWorktreeRecoveryEvidenceCache(t *testing.T) {
	tr := newTree(t)
	a, b := tr.repo("home/a"), tr.repo("home/b")
	gone := tr.path("home/.codex/worktrees/ab12/repo")
	ka, kb := archive.RepoKey("git@example.test:acme/a.git"), archive.RepoKey("https://example.test/acme/b")
	env := tr.env()
	calls := 0
	env.RepositoryIdentity = func(_ context.Context, root string) sourcefacts.RepositoryIdentity {
		calls++
		key := ka
		if root == b {
			key = kb
		}
		return sourcefacts.RepositoryIdentity{Root: root, Key: key, Known: true}
	}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(a, true), project(b, true)}}}
	r := newResolver(env, cfg, Filters{})
	first, second := r.resolveEvidence(context.Background(), gone, ka), r.resolveEvidence(context.Background(), gone, kb)
	if first.root != a || second.root != b || first.proof == nil || second.proof == nil || calls != 2 {
		t.Fatalf("%+v %+v calls%d", first, second, calls)
	}
	if r.resolveEvidence(context.Background(), gone, ka).proof.OriginalCwd != gone {
		t.Fatal("lost cwd")
	}
	// A trustworthy configured live owner wins over recorded remote evidence.
	if live := r.resolveEvidence(context.Background(), a, kb); live.root != a {
		t.Fatal(live)
	}
}

func TestProjectMappingValidationAndBatchDigest(t *testing.T) {
	for _, bad := range []string{"relative=/a", "/a=relative", "/a=", "=/a"} {
		if _, err := ParseProjectMappings([]string{bad}); err == nil {
			t.Fatal(bad)
		}
	}
	if _, err := ParseProjectMappings([]string{"/a=/b", "/a=/c"}); err == nil {
		t.Fatal("conflicting mapping accepted")
	}
	maps, err := ParseProjectMappings([]string{"/old=/target=with=equals"})
	if err != nil || maps["/old"] != "/target=with=equals" {
		t.Fatal(maps, err)
	}
	tr := newTree(t)
	target := tr.repo("home/a")
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(target, true)}}}
	mappings := map[string]string{filepath.Join(tr.home, "gone"): target}
	if err := validateProjectMappings(tr.env(), cfg, mappings); err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Projects[0].Included = false
	if err := validateProjectMappings(tr.env(), cfg, mappings); err == nil {
		t.Fatal("excluded target accepted")
	}
	a := Plan{Filters: Filters{ProjectMappings: mappings}}.BatchFilters()
	b := Plan{Filters: Filters{ProjectMappings: map[string]string{filepath.Join(tr.home, "gone"): "/other"}}}.BatchFilters()
	if a.equal(b) || a.MappingDigest == "" {
		t.Fatal("mapping change continued batch")
	}
}

func TestDeletedWorktreeBackfillPersistsProof(t *testing.T) {
	tr := newTree(t)
	root := tr.repo("home/repo")
	gone := tr.path("home/.codex/worktrees/old/repo")
	id := "00000000-0000-0000-0000-000000000077"
	body := codexTranscript(id, id, gone, fixedNow.Add(-time.Hour))
	body = strings.Replace(body, `"source":"cli"`, `"git":{"repository_url":"git@example.test:acme/repo.git"},"source":"cli"`, 1)
	tr.write(filepath.Join("home", codexFile(id)), body)
	env := tr.env()
	key := archive.RepoKey("https://example.test/acme/repo")
	env.RepositoryIdentity = func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
		return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true}
	}
	cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{project(root, true)}}}
	p := plan(t, env, nil, cfg, Filters{})
	items := p.Imported()
	if len(items) != 1 || items[0].ProjectResolution == nil {
		t.Fatalf("%+v", p.Imported())
	}
	home := t.TempDir()
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Registration{Home: home, Store: store, AdmittedAt: fixedNow, Batch: "synthetic-import"}).Run(items)
	if err != nil || len(result.Sessions) != 1 {
		t.Fatal(result, err)
	}
	reg, found, err := store.LoadRegistration(result.Sessions[0])
	if err != nil || !found || reg.ProjectResolution == nil || reg.ProjectResolution.OriginalCwd != gone || reg.ProjectResolution.RecordedRepoKey != key || reg.Origin != archive.SessionOriginImport {
		t.Fatal(reg, err)
	}
	cfg.Archive.Projects[0].Included = false
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AcceptSession(reg) {
		t.Fatal("exclusion lost after recovery")
	}
}

func TestRecordedRecoveryCannotOverrideLiveUnconfiguredAncestor(t *testing.T) {
	tr := newTree(t)
	live, target := tr.repo("home/live"), tr.repo("home/target")
	key := archive.RepoKey("https://example.test/acme/target")
	env := tr.env()
	calls := 0
	env.RepositoryIdentity = func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
		calls++
		return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true}
	}
	r := newResolver(env, config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(target, true)}}}, Filters{})
	res := r.resolveEvidence(context.Background(), filepath.Join(live, "deleted-subtree"), key)
	if res.root != live || calls != 0 {
		t.Fatal(res, calls)
	}
}

func TestRecoveredImportRechecksEvidenceBeforeRegistration(t *testing.T) {
	for _, changeSource := range []bool{false, true} {
		tr := newTree(t)
		root := tr.repo("home/repo")
		gone := tr.path("home/.codex/worktrees/gone/repo")
		id := "00000000-0000-0000-0000-000000000099"
		body := strings.Replace(codexTranscript(id, id, gone, fixedNow.Add(-time.Hour)), `"source":"cli"`, `"git":{"repository_url":"https://example.test/acme/repo"},"source":"cli"`, 1)
		tr.write(filepath.Join("home", codexFile(id)), body)
		key := archive.RepoKey("https://example.test/acme/repo")
		env := tr.env()
		env.RepositoryIdentity = func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
			return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true}
		}
		current := true
		env.RepositoryIdentityCurrent = func(sourcefacts.RepositoryIdentity) bool { return current }
		cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{project(root, true)}}}
		p := plan(t, env, nil, cfg, Filters{})
		if len(p.Imported()) != 1 {
			t.Fatal(p.Imported())
		}
		if changeSource {
			// A shorter rewrite: an append alone would still be current.
			source := tr.path(filepath.Join("home", codexFile(id)))
			if err := os.Truncate(source, 1); err != nil {
				t.Fatal(err)
			}
		} else {
			current = false
		}
		home := t.TempDir()
		if err := config.Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		store, err := state.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		result, err := (Registration{Home: home, Store: store, AdmittedAt: fixedNow, Batch: "synthetic"}).Run(p.Imported())
		if err != nil || len(result.Sessions) != 0 || result.NotAdmitted != 1 {
			t.Fatal(result, err)
		}
	}
}

func TestRecordedRecoveryKeepsBrokenWorktreePending(t *testing.T) {
	tr := newTree(t)
	target := tr.repo("home/target")
	broken := tr.path("home/.codex/worktrees/broken/repo")
	tr.write("home/.codex/worktrees/broken/repo/.git", "gitdir: /synthetic/missing/gitdir\n")
	key := archive.RepoKey("https://example.test/acme/target")
	env := tr.env()
	calls := 0
	env.RepositoryIdentity = func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
		calls++
		return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true}
	}
	r := newResolver(env, config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(target, true)}}}, Filters{})
	for _, cwd := range []string{broken, filepath.Join(broken, "deleted-subtree")} {
		result := r.resolveEvidence(t.Context(), cwd, key)
		if result.skip != SkipWorktreeUnresolved || result.root == target || calls != 0 {
			t.Fatal(result, calls)
		}
	}
}

func TestDeletedWorktreePlanningContinuesPastRecoveryCache(t *testing.T) {
	tr := newTree(t)
	root := tr.repo("home/repo")
	key := archive.RepoKey("https://example.test/acme/repo")
	env := tr.env()
	calls := 0
	env.RepositoryIdentity = func(context.Context, string) sourcefacts.RepositoryIdentity {
		calls++
		return sourcefacts.RepositoryIdentity{Root: root, Key: key, Known: true}
	}
	env.RepositoryIdentityCurrent = func(sourcefacts.RepositoryIdentity) bool { return true }
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(root, true)}}}
	for range 2 {
		r := newResolver(env, cfg, Filters{})
		var last resolution
		for i := range 4097 {
			cwd := tr.path(fmt.Sprintf("home/.codex/worktrees/deleted-%d/repo", i))
			last = r.resolveEvidence(t.Context(), cwd, key)
			if last.skip != "" || last.root != root || last.proof == nil {
				t.Fatalf("candidate %d: %+v", i, last)
			}
		}
		last.current.reset(t.Context())
		if !last.current.valid() {
			t.Fatal("last candidate lost freshness proof")
		}
	}
	if calls != 2 {
		t.Fatalf("inventory re-read per cwd: %d", calls)
	}
}

// mutationReader runs a deterministic external write after a borrowed read or close.
type mutationReader struct {
	io.ReadCloser
	afterRead  func()
	afterClose func()
}

func (r *mutationReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if r.afterRead != nil {
		r.afterRead()
	}
	return n, err
}

func (r *mutationReader) Close() error {
	err := r.ReadCloser.Close()
	if r.afterClose != nil {
		r.afterClose()
	}
	return err
}

type sourceMutation string

const (
	mutationExcludedCwd   sourceMutation = "excluded_cwd"
	mutationRepositoryKey sourceMutation = "repository_key"
	mutationProducer      sourceMutation = "producer"
	mutationCreation      sourceMutation = "creation"
	mutationReplacement   sourceMutation = "replacement"
)

func TestImportRejectsSourceChangesAcrossHeaderAndRecovery(t *testing.T) {
	for _, phase := range []string{"header_read", "after_header", "repository_lookup"} {
		for _, change := range []sourceMutation{mutationExcludedCwd, mutationRepositoryKey, mutationProducer, mutationCreation, mutationReplacement} {
			t.Run(phase+"/"+string(change), func(t *testing.T) {
				tr := newTree(t)
				root := tr.repo("home/repo")
				excluded := tr.repo("home/excluded")
				gone := tr.path("home/.codex/worktrees/gone/repo")
				id := "00000000-0000-0000-0000-000000000098"
				body := strings.Replace(codexTranscript(id, id, gone, fixedNow.Add(-time.Hour)), `"source":"cli"`, `"git":{"repository_url":"https://example.test/acme/repo"},"source":"cli"`, 1)
				relative := filepath.Join("home", codexFile(id))
				source := tr.path(relative)
				tr.write(relative, body)
				// Unrelated settled work must still progress through the same plan/import.
				other := "00000000-0000-0000-0000-000000000099"
				tr.write(filepath.Join("home", codexFile(other)), codexTranscript(other, other, root, fixedNow.Add(-time.Hour)))
				changed := false
				mutate := func() {
					if changed {
						return
					}
					changed = true
					replacement := body
					switch change {
					case mutationExcludedCwd:
						replacement = strings.Replace(body, gone, excluded, 1)
					case mutationRepositoryKey:
						replacement = strings.Replace(body, "acme/repo", "acme/other", 1)
					case mutationProducer:
						replacement = strings.Replace(body, `"source":"cli"`, `"source":"exec"`, 1)
					case mutationCreation:
						replacement = strings.ReplaceAll(body, fixedNow.Add(-time.Hour).UTC().Format(time.RFC3339), fixedNow.Add(-2*time.Hour).UTC().Format(time.RFC3339))
					case mutationReplacement:
						// Preserve size and mtime: path replacement must compare file identity.
						info, err := os.Stat(source)
						if err != nil {
							t.Fatal(err)
						}
						tmp := source + ".replacement"
						if err := os.WriteFile(tmp, []byte(body), 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
							t.Fatal(err)
						}
						if err := os.Rename(tmp, source); err != nil {
							t.Fatal(err)
						}
						return
					}
					if replacement == body {
						t.Fatal("mutation did not change fixture")
					}
					tr.write(relative, replacement)
				}
				env := tr.env()
				if phase != "repository_lookup" {
					env.Open = func(path string) (io.ReadCloser, error) {
						f, err := os.Open(path)
						if err != nil {
							return nil, err
						}
						if path != source {
							return f, nil
						}
						if phase == "header_read" {
							return &mutationReader{ReadCloser: f, afterRead: mutate}, nil
						}
						return &mutationReader{ReadCloser: f, afterClose: mutate}, nil
					}
				}
				key := archive.RepoKey("https://example.test/acme/repo")
				env.RepositoryIdentity = func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
					if phase == "repository_lookup" {
						mutate()
					}
					k := key
					if path == excluded {
						k = archive.RepoKey("https://example.test/acme/excluded")
					}
					return sourcefacts.RepositoryIdentity{Root: path, Key: k, Known: true}
				}
				env.RepositoryIdentityCurrent = func(sourcefacts.RepositoryIdentity) bool { return true }
				cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{project(root, true), project(excluded, false)}}}
				p := plan(t, env, nil, cfg, Filters{})
				if !changed {
					t.Fatal("mutation hook was not exercised")
				}
				var affected Candidate
				for _, c := range p.Candidates {
					if c.NativeSessionID == id {
						affected = c
					}
				}
				if affected.Skip != SkipSourceChanged {
					t.Fatalf("changed candidate: %+v", affected)
				}
				home := t.TempDir()
				if err := config.Save(home, cfg); err != nil {
					t.Fatal(err)
				}
				store, err := state.Open(home)
				if err != nil {
					t.Fatal(err)
				}
				result, err := (Registration{Home: home, Store: store, AdmittedAt: fixedNow, Batch: "synthetic-source-observation"}).Run(p.Imported())
				if err != nil || len(result.Sessions) != 1 || len(p.Imported()) != 1 || p.Imported()[0].NativeSessionID != other {
					t.Fatalf("unrelated progress: %+v, %v", result, err)
				}
				// A later scan considers settled fresh facts rather than permanently
				// poisoning the native ID as an unsafe format.
				fresh := tr.env()
				fresh.RepositoryIdentity = env.RepositoryIdentity
				fresh.RepositoryIdentityCurrent = env.RepositoryIdentityCurrent
				settled := plan(t, fresh, nil, cfg, Filters{})
				for _, c := range settled.Candidates {
					if c.NativeSessionID == id && (c.Skip == SkipSourceChanged || c.Skip == SkipUnsafeFormat) {
						t.Fatalf("settled retry: %+v", c)
					}
					if c.NativeSessionID == id && change == mutationExcludedCwd && c.Skip != SkipExcludedProject {
						t.Fatalf("excluded retry: %+v", c)
					}
				}
			})
		}
	}
}

func TestRecoveredImportRenewsSemanticContextAfterPlanning(t *testing.T) {
	for _, mode := range []string{"caller-context", "cancelled", "nil-context"} {
		t.Run(mode, func(t *testing.T) {
			tr := newTree(t)
			root := tr.repo("home/repo")
			gone := tr.path("home/.codex/worktrees/gone/repo")
			id := "00000000-0000-0000-0000-000000000099"
			body := strings.Replace(codexTranscript(id, id, gone, fixedNow.Add(-time.Hour)), `"source":"cli"`, `"git":{"repository_url":"https://example.test/acme/repo"},"source":"cli"`, 1)
			tr.write(filepath.Join("home", codexFile(id)), body)
			key := archive.RepoKey("https://example.test/acme/repo")
			env := tr.env()
			observations := 0
			admitting := false
			priorValidation := struct{ observed context.Context }{}
			env.RepositoryIdentity = func(ctx context.Context, path string) sourcefacts.RepositoryIdentity {
				if ctx.Err() != nil {
					t.Fatal("Git lookup used expired planning context")
				}
				if admitting && mode == "nil-context" {
					deadline, bounded := ctx.Deadline()
					if remaining := time.Until(deadline); !bounded || remaining <= 0 || remaining > 30*time.Second {
						t.Fatal("nil-context admission did not bound semantic work per slice", remaining, bounded)
					}
					if ctx == priorValidation.observed {
						t.Fatal("nil-context admission reused the previous slice's deadline")
					}
					priorValidation = struct{ observed context.Context }{observed: ctx}
				}
				observations++
				return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true, Validation: "semantic", ObservedRoot: path}
			}
			env.RepositoryIdentityCurrent = func(sourcefacts.RepositoryIdentity) bool { return true }
			cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{project(root, true)}}}
			planning, cancelPlanning := context.WithCancel(t.Context())
			p, err := BuildPlan(planning, env, states{}, cfg, Filters{})
			if err != nil || len(p.Imported()) != 1 {
				t.Fatal(p.Imported(), err)
			}
			cancelPlanning()
			plannedObservations := observations
			home := t.TempDir()
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			admission, cancelAdmission := context.WithCancel(t.Context())
			defer cancelAdmission()
			if mode == "cancelled" {
				cancelAdmission()
			}
			if mode == "nil-context" {
				admission = nil
			}
			admitting = true
			// Force separate reservation and admission holds instead of depending
			// on whether both fit the wall-clock limit on a loaded runner.
			result, err := (Registration{Context: admission, Home: home, Store: store, AdmittedAt: fixedNow, Batch: "synthetic", MaxHoldSteps: 1}).Run(p.Imported())
			if mode == "cancelled" {
				if !errors.Is(err, ErrStopped) || len(result.Sessions) != 0 {
					t.Fatal(result, err)
				}
			} else if err != nil || len(result.Sessions) != 1 || observations != plannedObservations+2 {
				t.Fatal("renewed admission failed", result, err, observations)
			} else {
				reg, found, err := store.LoadRegistration(result.Sessions[0])
				if err != nil || !found || reg.ProjectResolution == nil || reg.ProjectResolution.ValidationMethod != "semantic" {
					t.Fatal("semantic ownership provenance was not retained", reg, err)
				}
			}
		})
	}
}
