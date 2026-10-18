package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestSkillContentPassCachesKeepProjectAndOwnerScopes(t *testing.T) {
	t.Parallel()
	home, a, b := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(base, body string) {
		t.Helper()
		dir := filepath.Join(base, ".agents", "skills", "one")
		must(t, os.MkdirAll(dir, 0700))
		must(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600))
	}
	write(home, "ordinary user")
	write(a, "ordinary project A")
	write(b, "ordinary project B")
	env := testEnv(t, t.TempDir(), time.Now())
	env.UserHomeDir = func() (string, error) { return home, nil }
	lookups := map[string]int{}
	env.LookupEnv = func(key string) (string, bool) { lookups[key]++; return "", false }
	pass := newSkillObservationPass(env, config.SkillEvidenceBody)
	budget := agentapi.NewNativeReadBudget(8 << 20)
	get := func(project string) []agentapi.SkillContentDigest {
		t.Helper()
		out, releaseObservation, err := pass.ObserveWithContent(t.Context(), archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}, ProjectRoot: project}, time.Now(), budget)
		if err != nil {
			t.Fatal(err)
		}
		clonedDigests := slices.Clone(out.Digests)
		releaseObservation()
		releaseObservation()
		return clonedDigests
	}
	u := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary user")))
	pa := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary project A")))
	pb := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary project B")))
	regA := archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}, ProjectRoot: a}
	first := get(a)
	baseline, err := pass.Observe(regA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !slices.Contains(first, u) || !slices.Contains(first, pa) {
		t.Fatal("initial safe content domain")
	}
	write(home, "changed user")
	write(a, "changed project A")
	second := get(b)
	if len(second) != 1 || !slices.Contains(second, pb) || slices.Contains(second, u) || slices.Contains(second, pa) {
		t.Fatal("foreign project content joined")
	}
	if third := get(a); len(third) != 0 {
		t.Fatal("same pass cache reread skill bytes")
	}
	cached, err := pass.Observe(regA, time.Now())
	if err != nil || !reflect.DeepEqual(baseline, cached) {
		t.Fatal("cached evidence changed after file mutation", err)
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "CODEX_HOME", "XDG_DATA_HOME"} {
		if lookups[key] != 1 {
			t.Fatal("fixed environment was resnapshotted", lookups)
		}
	}
	other := agentapi.NewNativeReadBudget(8 << 20)
	out, releaseObservation, err := pass.ObserveWithContent(t.Context(), archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}, ProjectRoot: a}, time.Now(), other)
	releaseObservation()
	if err != nil || len(out.Digests) != 0 {
		t.Fatal("cache charge transferred to a different owner", err)
	}
	if used, _ := other.Charged(); used != 0 {
		t.Fatal("foreign budget charged cached content")
	}
	pass.Close()
	pass.Close()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("pass content caches leaked")
	}
	if _, releaseObservation, err := pass.ObserveWithContent(t.Context(), archive.SessionRegistration{}, time.Now(), budget); !errors.Is(err, agentapi.ErrClosed) {
		releaseObservation()
		t.Fatal("closed pass reused")
	}
}

func TestSkillContentPassNoneImportAndCancelSkipObservation(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.SkillEvidence{config.SkillEvidenceNone, config.SkillEvidenceMetadata} {
		env := testEnv(t, t.TempDir(), time.Now())
		env.UserHomeDir = func() (string, error) { t.Fatal("forbidden observer initialized"); return "", nil }
		env.LookupEnv = func(string) (string, bool) { t.Fatal("forbidden environment snapshot"); return "", false }
		pass := newSkillObservationPass(env, mode)
		origin := archive.SessionOrigin("")
		if mode == config.SkillEvidenceMetadata {
			origin = archive.SessionOriginImport
		}
		reg := archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}, Origin: origin}
		out, releaseObservation, err := pass.ObserveWithContent(t.Context(), reg, time.Now(), nil)
		releaseObservation()
		if err != nil || len(out.Evidence) != 0 || len(out.Digests) != 0 {
			t.Fatal("none/import produced current evidence", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, releaseObservation, err = pass.ObserveWithContent(ctx, reg, time.Now(), nil)
		releaseObservation()
		if !errors.Is(err, context.Canceled) {
			t.Fatal("canceled observer proceeded", err)
		}
		pass.Close()
	}
}

// The callback, rather than Close or the next session boundary, retires every
// private witness. This leaves existing cache owners and late headers their room.
func TestSkillContentOperationReleasesBetweenProjects(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	write := func(base, body string) {
		t.Helper()
		dir := filepath.Join(base, ".agents", "skills", "one")
		must(t, os.MkdirAll(dir, 0700))
		must(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600))
	}
	write(home, "ordinary user")
	env := testEnv(t, t.TempDir(), time.Now())
	env.UserHomeDir = func() (string, error) { return home, nil }
	env.LookupEnv = func(string) (string, bool) { return "", false }
	pass := newSkillObservationPass(env, config.SkillEvidenceBody)
	defer pass.Close()
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	const header = int64(128 << 10)
	background := agentapi.PassCacheReserve - header
	if !budget.ReserveUnscoped(background) {
		t.Fatal("existing cache setup")
	}
	for i := range 8 {
		project := t.TempDir()
		write(project, "ordinary project")
		end := budget.BeginScope(160 << 20)
		out, release, err := pass.ObserveWithContent(t.Context(), archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}, ProjectRoot: project}, time.Now(), budget)
		want := 1
		if i == 0 {
			want = 2
		}
		if err != nil || len(out.Digests) != want {
			t.Fatal("actual cache-miss witness", i, err)
		}
		borrowed := out.Digests
		release()
		release()
		if used, _ := budget.Charged(); used != background {
			t.Fatal("operation skill witness outlived callback", used-background)
		}
		for _, d := range borrowed {
			if d != (agentapi.SkillContentDigest{}) {
				t.Fatal("closed callback retained digest backing bytes")
			}
		}
		if budget.ScopeExceeded() || budget.ScopePressured() {
			t.Fatal("optional observation poisoned scope")
		}
		end()
	}
	end := budget.BeginScope(160 << 20)
	if !budget.ReserveUnscopedRequired(header) {
		t.Fatal("late existing header refused")
	}
	session := budget.Reservation()
	if !session.Reserve(160 << 20) {
		t.Fatal("operation witnesses consumed current session headroom")
	}
	session.Release(160 << 20)
	budget.ReleaseUnscoped(header)
	end()
	pass.Close()
	if used, _ := budget.Charged(); used != background {
		t.Fatal("Close released another cache owner")
	}
	budget.ReleaseUnscoped(background)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("test ledger leaked")
	}
}

func TestSkillContentOperationCloseAndCombinedCapacity(t *testing.T) {
	t.Parallel()
	for _, limit := range []int64{2368, 1 << 20} {
		home, project := t.TempDir(), t.TempDir()
		for _, base := range []string{home, project} {
			dir := filepath.Join(base, ".agents", "skills", "one")
			must(t, os.MkdirAll(dir, 0700))
			must(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("ordinary"), 0600))
		}
		env := testEnv(t, t.TempDir(), time.Now())
		env.UserHomeDir = func() (string, error) { return home, nil }
		env.LookupEnv = func(string) (string, bool) { return "", false }
		pass := newSkillObservationPass(env, config.SkillEvidenceBody)
		budget := agentapi.NewNativeReadBudget(limit)
		end := budget.BeginScope(limit)
		out, release, err := pass.ObserveWithContent(t.Context(), archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}, ProjectRoot: project}, time.Now(), budget)
		if err != nil || len(out.Evidence) == 0 {
			t.Fatal("ordinary evidence withheld", err)
		}
		pass.Close()
		if limit == 2368 {
			if len(out.Digests) != 0 {
				t.Fatal("combined capacity returned partial proof")
			}
			if used, _ := budget.Charged(); used != 0 {
				t.Fatal("combined refusal retained private companions")
			}
		} else {
			if len(out.Digests) != 2 || out.Digests[0] == (agentapi.SkillContentDigest{}) {
				t.Fatal("Close retired caller-owned view")
			}
			if used, _ := budget.Charged(); used == 0 {
				t.Fatal("live callback unfunded")
			}
		}
		if budget.ScopeExceeded() || budget.ScopePressured() {
			t.Fatal("optional capacity changed flags")
		}
		release()
		release()
		if used, _ := budget.Charged(); used != 0 {
			t.Fatal("callback close leaked")
		}
		end()
	}
}
