package builtin

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

func TestCaptureBindingCannotBeInstalledThroughArbitraryRegistry(t *testing.T) {
	bindings := append([]Integration(nil), NewBuiltins().ordered...)
	for i := range bindings {
		// New accepts canonical identity declarations and derives their metadata.
		bindings[i].Descriptor = agentmeta.Descriptor{ID: bindings[i].Descriptor.ID}
	}
	registry, err := New(agentmeta.Builtins(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	if registry.fixedCapture != nil {
		t.Fatal("arbitrary valid bindings installed capture provider")
	}
	if _, err = registry.CaptureSettledCursor(nil); err == nil {
		t.Fatal("arbitrary valid descriptors installed capture")
	}
	actual := NewBuiltins()
	if actual.fixedCapture == nil {
		t.Fatal("production projection absent")
	}
	if _, err = actual.CaptureSettledCursor(nil); err == nil {
		t.Fatal("nil request minted proof")
	}
}
