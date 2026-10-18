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
		out, close, err := pass.ObserveWithContent(t.Context(), archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}, ProjectRoot: project}, time.Now(), budget)
		if err != nil {
			t.Fatal(err)
		}
		copy := slices.Clone(out.Digests)
		close()
		close()
		return copy
	}
	u := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary user")))
	pa := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary project A")))
	pb := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary project B")))
	first := get(a)
	if len(first) != 2 || !slices.Contains(first, u) || !slices.Contains(first, pa) {
		t.Fatal("initial safe content domain")
	}
	write(home, "changed user")
	write(a, "changed project A")
	second := get(b)
	if len(second) != 2 || !slices.Contains(second, u) || !slices.Contains(second, pb) || slices.Contains(second, pa) {
		t.Fatal("foreign project content joined")
	}
	if third := get(a); !reflect.DeepEqual(first, third) {
		t.Fatal("same pass cache reread skill bytes")
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "CODEX_HOME", "XDG_DATA_HOME"} {
		if lookups[key] != 1 {
			t.Fatal("fixed environment was resnapshotted", lookups)
		}
	}
	other := agentapi.NewNativeReadBudget(8 << 20)
	out, close, err := pass.ObserveWithContent(t.Context(), archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}, ProjectRoot: a}, time.Now(), other)
	close()
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
	if _, close, err := pass.ObserveWithContent(t.Context(), archive.SessionRegistration{}, time.Now(), budget); !errors.Is(err, agentapi.ErrClosed) {
		close()
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
		reg := archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}}
		if mode == config.SkillEvidenceMetadata {
			reg.Origin = archive.SessionOriginImport
		}
		out, close, err := pass.ObserveWithContent(t.Context(), reg, time.Now(), nil)
		close()
		if err != nil || len(out.Evidence) != 0 || len(out.Digests) != 0 {
			t.Fatal("none/import produced current evidence", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, close, err = pass.ObserveWithContent(ctx, reg, time.Now(), nil)
		close()
		if !errors.Is(err, context.Canceled) {
			t.Fatal("canceled observer proceeded", err)
		}
		pass.Close()
	}
}
