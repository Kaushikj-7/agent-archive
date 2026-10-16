package archive

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNativeCheckpointRequiresRecognizedFormatAndExactMonotonicProof(t *testing.T) {
	t.Parallel()
	cp := NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 10, SHA256: strings.Repeat("a", 64)}
	b := NativeSourceBinding{Agent: "muse", Version: 2, NativeID: "native", NativeCreatedAt: time.Unix(1, 0), Cwd: "/work", Path: "/native/session.jsonl", RootRevision: "one", Checkpoint: &cp}
	if e := b.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*NativeCheckpoint){func(c *NativeCheckpoint) { c.Version = 2 }, func(c *NativeCheckpoint) { c.Domain = "foreign" }, func(c *NativeCheckpoint) { c.Algorithm = "unknown" }, func(c *NativeCheckpoint) { c.Boundary = 0 }, func(c *NativeCheckpoint) { c.SHA256 = strings.Repeat("A", 64) }, func(c *NativeCheckpoint) { c.SHA256 = "bad" }} {
		c := cp
		mutate(&c)
		if c.Validate() == nil {
			t.Fatal("unknown proof accepted")
		}
	}
	next := b
	nextCP := cp
	next.Checkpoint = &nextCP
	if !next.AdvancesCheckpoint(&b) {
		t.Fatal("same proof refused")
	}
	nextCP.SHA256 = strings.Repeat("b", 64)
	if next.AdvancesCheckpoint(&b) {
		t.Fatal("equal-boundary conflicting digest accepted")
	}
	nextCP.Boundary++
	if !next.AdvancesCheckpoint(&b) {
		t.Fatal("shape rejected; caller still owes prior prefix proof")
	}
	if !next.PreservesFacts(&b) {
		t.Fatal("checkpoint relaxed or contaminated original facts")
	}
	next.Version = 1
	if next.Validate() == nil || next.PreservesFacts(&b) {
		t.Fatal("checkpoint downgrade accepted")
	}
	raw, _ := json.Marshal(cp)
	var decoded NativeCheckpoint
	if json.Unmarshal([]byte(strings.TrimSuffix(string(raw), "}")+`,"future":true}`), &decoded) == nil {
		t.Fatal("unknown proof field dropped")
	}
}

func FuzzNativeCheckpoint(f *testing.F) {
	f.Add([]byte(`{"version":1,"domain":"muse-ordinary","algorithm":"sha256","boundary":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
	f.Add([]byte(`{"version":1,"version":1}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var cp NativeCheckpoint
		if json.Unmarshal(raw, &cp) != nil {
			return
		}
		if cp.Validate() != nil {
			t.Fatal("decoded invalid checkpoint")
		}
		encoded, e := json.Marshal(cp)
		if e != nil {
			t.Fatal(e)
		}
		var round NativeCheckpoint
		if e = json.Unmarshal(encoded, &round); e != nil || cp != round {
			t.Fatal("proof changed", e)
		}
	})
}
