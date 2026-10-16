package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRealCLITextAndBrowserMaintainBoundedCacheAndEvictDeletion(t *testing.T) {
	env, legacy, id := publishedFixture(t)
	remote := privateCatalogFromLegacy(t, legacy)
	measured := storagetest.NewMeasuredStore(remote, 0)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
	run := func(args []string, input *strings.Reader, terminal bool) int {
		var out, errs bytes.Buffer
		local := env
		if terminal {
			local.IsTerminal = func(stream any) bool { return stream == any(input) || stream == any(&out) }
		}
		code := Run(args, input, &out, &errs, local)
		if code != 0 {
			t.Logf("private command failure: %s", errs.String())
		}
		return code
	}
	if code := run([]string{"show", id[:8], "--json"}, strings.NewReader(""), false); code != 0 {
		t.Fatal("warm text routing", code)
	}
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, "cache", "metadata")
	var temps, dirs []string
	old := time.Now().Add(-90 * 24 * time.Hour)
	for i := range 200 {
		key := fmt.Sprintf("other/private/%04d/metadata.json", i)
		dir := filepath.Join(base, hex.EncodeToString([]byte(key)))
		if err = os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		tmp := filepath.Join(dir, ".pending-private-stale")
		if err = os.WriteFile(tmp, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.Chtimes(tmp, old, old); err != nil {
			t.Fatal(err)
		}
		temps = append(temps, tmp)
		dirs = append(dirs, dir)
	}
	removed := func() int {
		n := 0
		for _, path := range temps {
			if _, e := os.Stat(path); errors.Is(e, os.ErrNotExist) {
				n++
			} else if e != nil {
				t.Fatal(e)
			}
		}
		return n
	}
	if code := run([]string{"show", id[:8], "--json"}, strings.NewReader(""), false); code != 0 {
		t.Fatal("text command", code)
	}
	first := removed()
	if first <= 0 || first > 64 {
		t.Fatal("text maintenance directory budget", first)
	}
	if code := run([]string{"show", "--json"}, strings.NewReader("q\n"), true); code != 0 {
		t.Fatal("browser command", code)
	}
	second := removed() - first
	if second <= 0 || second > 64 {
		t.Fatal("browser maintenance directory budget", second)
	}
	for _, dir := range dirs {
		if _, err = os.Stat(dir); err != nil {
			t.Fatal("other-prefix directory removed", err)
		}
	}
	if code := run([]string{"list", "--all-projects", "--limit", "50"}, strings.NewReader(""), false); code != 0 {
		t.Fatal("selected list body hydration", code)
	}
	key := "sessions/codex/" + id + "/metadata.json"
	bodyDir := filepath.Join(base, hex.EncodeToString([]byte(key)))
	if _, err = os.Stat(bodyDir); err != nil {
		t.Fatal("selected body cache not created", err)
	}
	if err = remote.DeleteSession(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	measured.Reset()
	if code := run([]string{"show", id[:8], "--json"}, strings.NewReader(""), false); code == 0 {
		t.Fatal("deleted session returned")
	}
	if _, err = os.Stat(bodyDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("text route retained proven deleted body", err)
	}
	t.Logf("text maintained=%d browser maintained=%d canonical LIST=%d", first, second, measured.Metrics().Lists)
	if measured.Metrics().Lists != 0 {
		t.Fatal("remote deletion used canonical listing", measured.Metrics())
	}
}

func TestBrowseDiscoveryPreservesCanceledRequest(t *testing.T) {
	env, legacy, _ := publishedFixture(t)
	remote := privateCatalogFromLegacy(t, legacy)
	for _, store := range []storage.ObjectStore{legacy, remote} {
		measured := storagetest.NewMeasuredStore(store, 0)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var stderr bytes.Buffer
		_, err := loadSessionsForBrowseContext(ctx, env, measured, listOptions{noCache: true}, &stderr, "show")
		if !errors.Is(err, context.Canceled) {
			t.Fatal("browse lost request cancellation", err)
		}
		if counts := measured.Metrics(); counts.Gets != 0 || counts.Lists != 0 {
			t.Fatal("canceled browse performed provider reads", counts)
		}
	}
}
