package discovery

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestOverCapCheckpointCannotReportDurableSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	prior := catalog{Version: catalogVersion, Roots: []string{"/synthetic"}}
	if err := local.WriteCompact(path, prior); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pending := catalog{Version: catalogVersion, Roots: prior.Roots,
		Queue:  []directory{{Root: "/synthetic", Path: "sessions", Offset: 7}},
		Health: Health{Errors: []string{strings.Repeat("x", maxCatalogBytes+1)}}}
	lookup := &CodexRolloutLookup{readBudget: agentapi.NewNativeReadBudget(128 << 20)}
	health := Health{Pending: true}
	err = checkpointCatalog(t.Context(), lookup, path, &pending, &health)
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal replaced durable checkpoint", readErr)
	}
	if used, _ := lookup.readBudget.Charged(); used != 0 {
		t.Fatalf("refusal leaked %d bytes", used)
	}
	if !errors.Is(err, errCatalogTooLarge) {
		t.Fatalf("unwritten pending checkpoint reported success: %v", err)
	}
}
