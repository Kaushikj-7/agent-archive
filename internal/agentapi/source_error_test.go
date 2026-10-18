package agentapi

import (
	"errors"
	"testing"
)

// A cause words a failure after its kind, in fixed words with nothing of the
// native error (a path, say), and never changes the kind callers switch on.
func TestSourceErrorWordsItsCause(t *testing.T) {
	t.Parallel()
	native := errors.New("/Users/someone/.codex/sessions/rollout.jsonl: lineage incomplete")
	for _, tc := range []struct {
		err  error
		want string
		kind FailureKind
	}{
		{Wrap(Unavailable, native), "source unavailable", Unavailable},
		{WithCause(Wrap(Unavailable, native), CauseHistoryBaseUnavailable), "source unavailable: a Codex rollout this thread continues from was not found", Unavailable},
		{WithCause(Wrap(Unavailable, native), CauseLookupIOTimeout), "source unavailable: a native lookup I/O operation timed out; check local source access and retry", Unavailable},
		{WithCause(Wrap(Changed, native), CauseRolloutTipAmbiguous), "source changed while reading; try again: Codex thread has no single current rollout", Changed},
		//lint:ignore LV1001 deliberately a cause with no declared constant
		{WithCause(Wrap(Unsafe, native), "unknown_cause"), "unsafe source format", Unsafe},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error()=%q want %q", got, tc.want)
		}
		if got := Failure(tc.err); got != tc.kind {
			t.Errorf("%q: Failure=%s want %s", tc.want, got, tc.kind)
		}
		if !errors.Is(tc.err, native) {
			t.Errorf("%q no longer wraps its native error", tc.want)
		}
	}
}

func TestWithCauseLeavesTheOriginalAndOtherErrors(t *testing.T) {
	t.Parallel()
	original := Wrap(Unavailable, errors.New("refused"))
	named := WithCause(original, CauseHistoryBaseUnavailable)
	if original.Error() != "source unavailable" || named.Error() == original.Error() {
		t.Fatalf("original=%q named=%q", original, named)
	}
	joined := errors.Join(original, errors.New("other"))
	if got := WithCause(joined, CauseHistoryBaseUnavailable); got.Error() != joined.Error() || !errors.Is(got, joined) {
		t.Fatal("a join was given one failure's cause")
	}
	if WithCause(nil, CauseHistoryBaseUnavailable) != nil {
		t.Fatal("nil gained a cause")
	}
}
