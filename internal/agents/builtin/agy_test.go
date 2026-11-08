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

func TestAgyPortsRegisteredInBuiltins(t *testing.T) {
	r := NewBuiltins()
	if _, ok := r.LookupNativeHeaders("agy"); !ok {
		t.Fatal("Agy NativeHeaders not registered")
	}
	if _, ok := r.LookupImport("agy"); !ok {
		t.Fatal("Agy Imports not registered")
	}
	if _, ok := r.LookupCapabilityEvidence("agy"); !ok {
		t.Fatal("Agy CapabilityEvidence not registered")
	}
	if _, ok := r.LookupVersionInspector("agy"); !ok {
		t.Fatal("Agy VersionInspector not registered")
	}
	if _, ok := r.LookupRuntime("agy"); !ok {
		t.Fatal("Agy Runtime not registered")
	}
	if _, ok := r.LookupDiscovery("agy"); !ok {
		t.Fatal("Agy Discovery not registered")
	}
}
