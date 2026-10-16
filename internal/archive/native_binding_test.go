package archive

import (
	"strings"
	"testing"
	"time"
)

func TestNativeAuthorityValidatesPathsSeparatelyFromOpaqueIDs(t *testing.T) {
	root := NativeRoot{Agent: "synthetic", Configured: "/approved", Canonical: "/canonical", Revision: "one"}
	if err := root.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []NativeRoot{{Agent: "Codex", Configured: "/a", Canonical: "/b", Revision: "one"}, {Agent: "synthetic", Configured: "relative", Canonical: "/b", Revision: "one"}, {Agent: "synthetic", Configured: "/a/../b", Canonical: "/b", Revision: "one"}, {Agent: "synthetic", Configured: "/a", Canonical: "/b", Revision: ""}} {
		if bad.Validate() == nil {
			t.Fatal("invalid root accepted")
		}
	}
	b := &NativeSourceBinding{Agent: "synthetic", Version: 1, NativeID: "opaque/λ", NativeCreatedAt: time.Now(), Cwd: "/workspace", Path: "/canonical/session", RootRevision: "one", StreamID: "stream"}
	if err := b.Validate(); err != nil {
		t.Fatal("opaque ID treated as a path component", err)
	}
	for _, mutate := range []func(*NativeSourceBinding){func(b *NativeSourceBinding) { b.NativeID = strings.Repeat("x", 4097) }, func(b *NativeSourceBinding) { b.Path = "../escape" }, func(b *NativeSourceBinding) { b.Version = 2 }, func(b *NativeSourceBinding) { b.RootRevision = "" }} {
		copyBinding := *b
		mutate(&copyBinding)
		if copyBinding.Validate() == nil {
			t.Fatal("invalid binding accepted")
		}
	}
	copyBinding := *b
	copyBinding.ParentID = "newly-observed"
	if !copyBinding.PreservesFacts(b) {
		t.Fatal("additional observed fact refused")
	}
	if b.PreservesFacts(&copyBinding) {
		t.Fatal("known parent disappeared")
	}
}
