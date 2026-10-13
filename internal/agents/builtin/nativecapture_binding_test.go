package builtin

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

func TestCaptureBindingCannotBeInstalledThroughArbitraryRegistry(t *testing.T) {
	registry, err := New(agentmeta.Builtins(), NewBuiltins().ordered)
	if err != nil {
		t.Fatal(err)
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
