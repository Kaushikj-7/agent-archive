package agentmeta

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

func TestSizingImportBoundary(t *testing.T) {
	direct, all := importgraph.Imports(t, "github.com/wangjohn/agent-archive/internal/agentmeta")
	importgraph.Forbid(t, "internal/agentmeta", direct, "os", "os/exec", "io/fs", "path/filepath", "net", "net/http", "math/rand", "math/rand/v2")
	for _, path := range all {
		if strings.HasPrefix(path, "github.com/wangjohn/agent-archive/") {
			t.Errorf("pure JSON sizing reaches operational dependency %s", path)
		}
	}
}

type arbitraryMarshaler struct{}

func (arbitraryMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"unbounded"`), nil }

func TestBoundSupportsActualWireTypes(t *testing.T) {
	at := time.Date(2026, 1, 1, 1, 2, 3, 999999999, time.FixedZone("offset", 3600))
	values := []any{nil, true, false, math.MaxFloat64, math.SmallestNonzeroFloat64, int64(math.MinInt64), uint64(math.MaxUint64), json.Number("1.234e+100"), "<> &\u2028\u2029\x00\xff", []byte{0, 1, 2, 3}, []byte{}, []byte(nil), [3]byte{1, 2, 3}, at, &at, json.RawMessage(`{"text":"<&>  "}`), map[string]any{"<&>": []any{1.0, nil, "\"\\\n"}}, struct {
		A string `json:"a,omitempty"`
		B []byte `json:"b"`
		C int    `json:"c,string"`
	}{B: []byte{1}, C: 3}}
	for _, v := range values {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		n, err := JSONWireBound(t.Context(), v, 1<<20)
		if err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		if n < int64(len(raw)) {
			t.Fatalf("%T bound %d smaller than wire %d: %s", v, n, len(raw), raw)
		}
		if _, err := JSONWireBound(t.Context(), v, n-1); !errors.Is(err, ErrJSONWireLimit) {
			t.Fatalf("%T undersized bound: %v", v, err)
		}
	}
}

func TestBoundRefusesUnboundedValuesAndCancellation(t *testing.T) {
	if _, err := JSONWireBound(t.Context(), arbitraryMarshaler{}, 1024); !errors.Is(err, ErrJSONWireUnsupported) {
		t.Fatal(err)
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	if _, err := JSONWireBound(t.Context(), cycle, 1<<20); !errors.Is(err, ErrJSONWireUnsupported) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := JSONWireBound(ctx, "data", 100); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := JSONWireBound(t.Context(), "data", -1); !errors.Is(err, ErrJSONWireLimit) {
		t.Fatal(err)
	}
}

func FuzzStringBound(f *testing.F) {
	f.Add("<&>\u2028\xff\x00")
	f.Fuzz(func(t *testing.T, s string) {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		n, err := JSONWireBound(t.Context(), s, 6*int64(len(s))+2)
		if err != nil || n < int64(len(raw)) {
			t.Fatalf("bound %d wire %d error %v", n, len(raw), err)
		}
	})
}
