package nativecodec

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestCursorBoundedRecordsRetainExactWireAndCapturedOwner(t *testing.T) {
	c := oneMessageChat(1, `{"_v":3,"bubbleId":"b1","type":1,"text":"`+strings.Repeat("selected ", 4096)+`"}`)
	stream := func() func(context.Context) (CursorBubble, bool, error) {
		i := 0
		return func(ctx context.Context) (CursorBubble, bool, error) {
			if err := ctx.Err(); err != nil {
				return CursorBubble{}, false, err
			}
			if i >= len(c.Bubbles) {
				return CursorBubble{}, false, nil
			}
			row := c.Bubbles[i]
			i++
			return row, true, nil
		}
	}
	ordinary, err := FilterComposerRecords(t.Context(), c.Composer, stream())
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(2 << 20)
	owner := budget.Reservation()
	bounded, closeOutput, err := FilterComposerRecordsBounded(t.Context(), c.Composer, stream(), 1<<20, owner)
	if err != nil || !reflect.DeepEqual(bounded, ordinary) {
		t.Fatalf("bounded wire changed: %v", err)
	}
	if budget.Available() == 2<<20 {
		t.Fatal("borrowed output escaped its captured owner")
	}
	closeOutput()
	closeOutput()
	if budget.Available() != 2<<20 {
		t.Fatal("bounded output owner did not close exactly")
	}
	refused, closeRefused, err := FilterComposerRecordsBounded(t.Context(), c.Composer, stream(), 64, owner)
	defer closeRefused()
	if !errors.Is(err, agentapi.ErrReadBudget) || len(refused.Records) != 0 {
		t.Fatalf("prospective output refusal retained partial rows: %v", err)
	}
	if budget.Available() != 2<<20 {
		t.Fatal("output refusal lost captured charges")
	}
}
