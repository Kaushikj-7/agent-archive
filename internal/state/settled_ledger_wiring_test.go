package state

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func newSettledLedgerArm(t *testing.T) settledLedgerArm {
	t.Helper()
	store, reg, request, proof, a := settledStateCaptureFixture(t)
	b := agentapi.NewNativeReadBudget(128 << 20)
	right, closeRight := store.WithReadBudget(t.Context(), a)
	wrong, closeWrong := store.WithReadBudget(t.Context(), b)
	t.Cleanup(closeRight)
	t.Cleanup(closeWrong)
	return settledLedgerArm{store: store, reg: reg, a: a, b: b,
		wrongSave: func() error {
			_, err := wrong.SaveSettledCursorImportStage(t.Context(), reg, request, proof)
			return err
		},
		rightSave: func() error {
			_, err := right.SaveSettledCursorImportStage(t.Context(), reg, request, proof)
			return err
		},
		closeOwned: func() error { closeRight(); closeWrong(); err := proof.Close(); request.Close(); return err }}
}
