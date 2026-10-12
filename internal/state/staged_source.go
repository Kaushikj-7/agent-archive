package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// The first typed slice accepts exact retained evidence only. Transformations,
// including refilter/enrichment, require a later derivation authority.
func (s *Store) validateExactStagedSource(ctx context.Context, stage ImportStage, p PendingPublication) error {
	original, err := s.readyDigest(ctx, stage.Bundle)
	if err != nil {
		return err
	}
	selected, err := s.readyDigest(ctx, p.Bundle)
	if err != nil {
		return err
	}
	if original != selected || stage.Bundle.Capture.FilterVersion != archive.FilterVersion || string(stage.SkillEvidence) != p.SkillEvidence {
		return ErrReadySelectionFrozen
	}
	owner := s.resourceBudget.Reservation()
	const compressorScratch int64 = 1 << 20
	const counterScratch int64 = 32 << 10
	if !owner.Reserve(counterScratch) {
		return errStateBudget
	}
	largest, err := archive.SourceEncodingLineBound(ctx, stage.Bundle, owner.Available())
	owner.Release(counterScratch)
	if err != nil {
		return errors.Join(errStateBudget, err)
	}
	if largest > (owner.Available()-compressorScratch)/2 {
		owner.Refuse(2*largest + compressorScratch)
		return errStateBudget
	}
	scratch := 2*largest + compressorScratch
	if !owner.Reserve(scratch) {
		return errStateBudget
	}
	defer owner.Release(scratch)
	sum := sha256.New()
	wire := &stagedCountWriter{writer: sum, remaining: int64(len(p.SourceBytes))}
	if err := archive.CompressSource(wire, stage.Bundle); err != nil {
		return err
	}
	if wire.written != int64(len(p.SourceBytes)) || hex.EncodeToString(sum.Sum(nil)) != p.SourceSHA256 {
		return ErrReadySelectionFrozen
	}
	return ctx.Err()
}

type stagedCountWriter struct {
	writer             io.Writer
	remaining, written int64
}

func (w *stagedCountWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrDurableStorageCapacity
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	w.written += int64(n)
	return n, err
}

// A bounded proof encoder hashes the same canonical JSON as readyDigest,
// without constructing a second self-marshaled proof byte slice. The outer
// decoder's captured three-owner reservation includes this <=16KiB encoding.
func boundedStagedProofDigest(value any) (string, error) {
	if _, err := agentmeta.JSONWireBound(context.Background(), value, 16<<10); err != nil {
		return "", err
	}
	writer := &stagedJSONDigestWriter{sum: sha256.New()}
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		return "", err
	}
	if !writer.haveLast || writer.last != '\n' {
		return "", ErrDurableStorageRecovery
	}
	return hex.EncodeToString(writer.sum.Sum(nil)), nil
}

type stagedJSONDigestWriter struct {
	sum      hash.Hash
	last     byte
	haveLast bool
}

func (w *stagedJSONDigestWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if w.haveLast {
		if _, err := w.sum.Write([]byte{w.last}); err != nil {
			return 0, err
		}
	}
	if len(p) > 1 {
		if _, err := w.sum.Write(p[:len(p)-1]); err != nil {
			return 0, err
		}
	}
	w.last = p[len(p)-1]
	w.haveLast = true
	return len(p), nil
}
