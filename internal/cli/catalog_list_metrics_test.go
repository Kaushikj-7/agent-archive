package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type catalogListReads struct {
	*storagetest.MeasuredStore
	mu    sync.Mutex
	paths map[string]int
}

func (s *catalogListReads) record(key string) {
	kind := "other"
	switch {
	case key == catalog.HeadKey:
		kind = "head"
	case key == catalog.CoordinatorKey:
		kind = "coordinator"
	case strings.Contains(key, "/nodes/"):
		kind = "tree"
	case strings.Contains(key, "/metadata/"):
		kind = "body"
	}
	s.mu.Lock()
	s.paths[kind]++
	s.mu.Unlock()
}

func (s *catalogListReads) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	s.record(key)
	return s.MeasuredStore.GetLimited(ctx, key, limit)
}

func (s *catalogListReads) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	s.record(key)
	return s.MeasuredStore.GetCatalogVersion(ctx, key, limit)
}

func TestRealCLICatalogDefaultList50Bounded(t *testing.T) {
	for _, size := range []int{128, 512} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			a := newScopedArchive(t)
			for i := range size {
				m := a.base
				m.SessionID = fmt.Sprintf("list-%04d", i)
				m.Title = m.SessionID
				m.CapturedAt = a.base.CapturedAt.Add(-time.Duration(i+1) * time.Minute)
				if i%4 == 0 {
					m.ParentSessionID = "list-0001"
				}
				key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err = a.mem.Put(t.Context(), key, body); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := reader.RebuildIndex(t.Context(), a.mem, archiveSessionsPrefix); err != nil {
				t.Fatal(err)
			}
			remote := privateCatalogFromLegacy(t, a.mem)
			counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
			measured, err := catalog.Wrap(counts)
			if err != nil {
				t.Fatal(err)
			}
			oracle := a.env
			home, err := a.env.Home()
			if err != nil {
				t.Fatal(err)
			}
			cfg, found, err := config.Load(home)
			if err != nil || !found {
				t.Fatal("private configuration", err)
			}
			oracleHome := t.TempDir()
			if err = config.Save(oracleHome, cfg); err != nil {
				t.Fatal(err)
			}
			oracle.Home = func() (string, error) { return oracleHome, nil }
			oracle.OpenStore = func(config.Config) (storage.ObjectStore, error) { return a.mem, nil }
			env := a.env
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
			args := []string{"list", "--all-projects", "--limit", "50"}
			var want, stderr bytes.Buffer
			if code := Run(args, bytes.NewReader(nil), &want, &stderr, oracle); code != 0 {
				t.Fatal(code, stderr.String())
			}
			for _, phase := range []string{"cold", "warm"} {
				counts.Reset()
				counts.paths = map[string]int{}
				var got, errs bytes.Buffer
				if code := Run(args, bytes.NewReader(nil), &got, &errs, env); code != 0 {
					t.Fatal(code, errs.String())
				}
				if got.String() != want.String() || errs.String() != stderr.String() {
					t.Fatalf("%s output differs from exhaustive oracle", phase)
				}
				metrics := counts.Metrics()
				t.Logf("%s sessions=%d GET=%d bytes=%d LIST=%d paths=%v", phase, size, metrics.Gets, metrics.Bytes, metrics.Lists, counts.paths)
				bodyReads := 50
				if phase == "warm" {
					bodyReads = 0
				}
				if metrics.Lists != 0 || counts.paths["tree"] > 16 || counts.paths["body"] != bodyReads || counts.paths["head"] != 1 || counts.paths["coordinator"] != 1 || counts.paths["other"] != 0 {
					t.Fatalf("unbounded list-50: %+v paths=%v", metrics, counts.paths)
				}
			}
		})
	}
}

func TestRealCLICatalogListNonMultiplePageLimit(t *testing.T) {
	for _, size := range []int{1100} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			a := newScopedArchive(t)
			for i := range size {
				m := a.base
				m.SessionID = fmt.Sprintf("list-%04d", i)
				m.Title = m.SessionID
				m.CapturedAt = a.base.CapturedAt.Add(-time.Duration(i+1) * time.Minute)

				key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err = a.mem.Put(t.Context(), key, body); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := reader.RebuildIndex(t.Context(), a.mem, archiveSessionsPrefix); err != nil {
				t.Fatal(err)
			}
			remote := privateCatalogFromLegacy(t, a.mem)
			counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
			measured, err := catalog.Wrap(counts)
			if err != nil {
				t.Fatal(err)
			}
			oracle := a.env
			home, err := a.env.Home()
			if err != nil {
				t.Fatal(err)
			}
			cfg, found, err := config.Load(home)
			if err != nil || !found {
				t.Fatal("private configuration", err)
			}
			oracleHome := t.TempDir()
			if err = config.Save(oracleHome, cfg); err != nil {
				t.Fatal(err)
			}
			oracle.Home = func() (string, error) { return oracleHome, nil }
			oracle.OpenStore = func(config.Config) (storage.ObjectStore, error) { return a.mem, nil }
			env := a.env
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
			args := []string{"list", "--all-projects", "--limit", "1050"}
			var want, stderr bytes.Buffer
			if code := Run(args, bytes.NewReader(nil), &want, &stderr, oracle); code != 0 {
				t.Fatal(code, stderr.String())
			}
			for _, phase := range []string{"cold", "warm"} {
				counts.Reset()
				counts.paths = map[string]int{}
				var got, errs bytes.Buffer
				if code := Run(args, bytes.NewReader(nil), &got, &errs, env); code != 0 {
					t.Fatal(code, errs.String())
				}
				if got.String() != want.String() || errs.String() != stderr.String() {
					t.Fatalf("%s output differs from exhaustive oracle", phase)
				}
				metrics := counts.Metrics()
				t.Logf("%s sessions=%d GET=%d bytes=%d LIST=%d paths=%v", phase, size, metrics.Gets, metrics.Bytes, metrics.Lists, counts.paths)
				bodyReads := 1050
				if phase == "warm" {
					bodyReads = 0
				}
				if metrics.Lists != 0 || counts.paths["body"] != bodyReads || counts.paths["head"] != 2 || counts.paths["coordinator"] != 1 || counts.paths["other"] != 0 {
					t.Fatalf("nonmultiple page selection: %+v paths=%v", metrics, counts.paths)
				}
			}
		})
	}
}
