package builtin

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

func TestBuiltinsExposeAntigravity(t *testing.T) {
	r := NewBuiltins()
	b, ok := r.Lookup("antigravity")
	if !ok || b.Descriptor.ID != agentmeta.Agy {
		t.Fatalf("Lookup(antigravity) = %#v, %v", b.Descriptor, ok)
	}
	if _, ok := r.Launcher("agy"); !ok {
		t.Fatal("AGY launcher is not registered")
	}
	if _, _, ok := r.LookupSources("agy"); !ok {
		t.Fatal("AGY source/filter is not registered")
	}
}
