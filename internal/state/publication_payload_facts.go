package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// payloadDigestFacts borrows exact bytes only within one closed validation call.
// It has no persistence, authority, effects or lifetime across guards.
type payloadDigestFact struct {
	reference archive.SourceReference
	selection PublicationSelection
	body      []byte
	sha       string
}

type payloadDigestFacts struct {
	ctx         context.Context
	reservation stateReservation
	facts       []payloadDigestFact
	err         error
	charge      int64
}

func newPayloadDigestFacts(ctx context.Context, budget *agentapi.NativeReadBudget, count int, owners ...stateReservation) (*payloadDigestFacts, func(), error) {
	var owner stateReservation = budget.Reservation()
	if len(owners) > 0 {
		owner = owners[0]
	}
	if budget == nil || count == 0 {
		return nil, func() {}, nil
	}
	if count < 0 || count > 130 {
		return nil, func() {}, nil
	} // Existing shape validators retain their own caps.
	if err := ctx.Err(); err != nil {
		return nil, func() {}, err
	}
	charge := int64(count)*256 + 1024
	if !owner.Reserve(charge) {
		return nil, func() {}, errStateBudget
	}
	f := &payloadDigestFacts{ctx: ctx, reservation: owner, facts: make([]payloadDigestFact, 0, count), charge: charge}
	var once sync.Once
	return f, func() { once.Do(func() { f.facts = nil; owner.Release(f.charge); f.charge = 0 }) }, nil
}

func (f *payloadDigestFacts) result(err error) error {
	if f == nil {
		return err
	}
	return errors.Join(err, f.err, f.ctx.Err())
}

func payloadInlineSHA(source PublicationSource, f *payloadDigestFacts) string {
	if f == nil {
		return publicationSHA256(source.Payload.Inline)
	}
	if f.err != nil {
		return ""
	}
	if f.err = f.ctx.Err(); f.err != nil {
		return ""
	}
	for _, previous := range f.facts {
		if previous.reference != source.Reference || previous.selection != source.Selection || len(previous.body) != len(source.Payload.Inline) {
			continue
		}
		equal := true
		for offset := 0; offset < len(previous.body); offset += 64 << 10 {
			if f.err = f.ctx.Err(); f.err != nil {
				return ""
			}
			end := min(offset+(64<<10), len(previous.body))
			if !bytes.Equal(previous.body[offset:end], source.Payload.Inline[offset:end]) {
				equal = false
				break
			}
		}
		if equal {
			return previous.sha
		}
	}
	if len(f.facts) == cap(f.facts) {
		// Unexpected extra valid shapes compute afresh, never invent a wire refusal.
		if !f.reservation.Reserve(64) {
			f.err = errStateBudget
			return ""
		}
		f.charge += 64
	}
	hash := sha256.New()
	for offset := 0; offset < len(source.Payload.Inline); offset += 64 << 10 {
		if f.err = f.ctx.Err(); f.err != nil {
			return ""
		}
		_, _ = hash.Write(source.Payload.Inline[offset:min(offset+(64<<10), len(source.Payload.Inline))])
	}
	sha := hex.EncodeToString(hash.Sum(nil))
	if len(f.facts) < cap(f.facts) {
		f.facts = append(f.facts, payloadDigestFact{source.Reference, source.Selection, source.Payload.Inline, sha})
	}
	return sha
}
