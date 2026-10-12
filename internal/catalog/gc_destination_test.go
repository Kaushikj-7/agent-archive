package catalog

import (
	"bytes"
	"testing"
)

func TestCollectForeignHeldBarrierLeavesCoordinatorUnchanged(t *testing.T) {
	writerA, storeA := fixture(t)
	writerB, _ := fixture(t)
	coordinatorA := writerA.Coordinator()
	seal, err := coordinatorA.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before, version, err := storeA.GetVersioned(t.Context(), CoordinatorKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := writerB.Collect(t.Context(), coordinatorA.HeldBarrier(seal)); err == nil {
		t.Fatal("foreign held barrier accepted")
	}
	after, renewed, err := storeA.GetVersioned(t.Context(), CoordinatorKey)
	if err != nil || renewed != version || !bytes.Equal(before, after) {
		t.Fatal("foreign collection changed originating coordinator", err, version, renewed)
	}
	current, _, err := coordinatorA.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinatorA.RecoverUnleasedSeal(t.Context(), seal, current.Generation); err != nil {
		t.Fatal("foreign collection stranded originating seal", err)
	}
}
