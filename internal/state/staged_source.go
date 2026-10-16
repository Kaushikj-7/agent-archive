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

// validatedStagedSource is successful same-call canonical evidence, never persisted.
type validatedStagedSource struct {
	bundleSHA  string
	stageSHA   string
	sourceSHA  string
	sourceSize int
	owner      string
	policy     readyPolicyContext
}

// The first typed slice accepts exact retained evidence only; transformations remain refused.
func (s *Store) validateExactStagedSource(ctx context.Context, stage ImportStage, p PendingPublication) error {
	_, err := s.validateStagedSourceForFactory(ctx, stage, p, nil)
	return err
}

func (s *Store) validateStagedSourceWithFact(ctx context.Context, stage ImportStage, p PendingPublication, fact *validatedStagedSource) error {
	if fact == nil {
		return s.validateExactStagedSource(ctx, stage, p)
	}
	_, err := s.validateStagedSourceForFactory(ctx, stage, p, fact)
	return err
}

// A fact is lexical to one factory invocation; external validation always recompresses.
func (s *Store) validateStagedSourceForFactory(ctx context.Context, stage ImportStage, p PendingPublication, prior *validatedStagedSource) (*validatedStagedSource, error) {
	original, err := s.readyDigest(ctx, stage.Bundle)
	if err != nil {
		return nil, err
	}
	selected, err := s.readyDigest(ctx, p.Bundle)
	if err != nil {
		return nil, err
	}
	if original != selected || stage.Bundle.Capture.FilterVersion != archive.FilterVersion || string(stage.SkillEvidence) != p.SkillEvidence {
		return nil, ErrReadySelectionFrozen
	}
	owner := s.readReservation()
	const compressorScratch int64 = 1 << 20
	const counterScratch int64 = 32 << 10
	if !owner.Reserve(counterScratch) {
		return nil, errStateBudget
	}
	largest, err := archive.SourceEncodingLineBound(ctx, stage.Bundle, owner.Available())
	owner.Release(counterScratch)
	if err != nil {
		return nil, errors.Join(errStateBudget, err)
	}
	if largest > (owner.Available()-compressorScratch)/2 {
		owner.Refuse(2*largest + compressorScratch)
		return nil, errStateBudget
	}
	scratch := 2*largest + compressorScratch
	if !owner.Reserve(scratch) {
		return nil, errStateBudget
	}
	defer owner.Release(scratch)
	if prior != nil {
		if prior.bundleSHA != original || prior.stageSHA != stage.SHA256 || prior.sourceSHA != p.SourceSHA256 || prior.sourceSize != len(p.SourceBytes) || prior.owner != p.Bundle.ArchiveSessionID || prior.policy != readyPolicy(p) || readySHA(p.SourceBytes) != prior.sourceSHA {
			return nil, ErrReadySelectionFrozen
		}
		return prior, ctx.Err()
	}
	sum := sha256.New()
	wire := &stagedCountWriter{writer: sum, remaining: int64(len(p.SourceBytes))}
	if err := archive.CompressSource(wire, stage.Bundle); err != nil {
		return nil, err
	}
	if wire.written != int64(len(p.SourceBytes)) || hex.EncodeToString(sum.Sum(nil)) != p.SourceSHA256 {
		return nil, ErrReadySelectionFrozen
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &validatedStagedSource{bundleSHA: original, stageSHA: stage.SHA256, sourceSHA: p.SourceSHA256, sourceSize: len(p.SourceBytes), owner: p.Bundle.ArchiveSessionID, policy: readyPolicy(p)}, nil
}

type stagedCountWriter struct {
	writer    io.Writer
	remaining int64
	written   int64
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

// Closed proof shape is checked before this contextless canonical codec.
// All strings are fixed digests or bounded owner/token fields; the output
// writer caps 16KiB including the excluded Encoder newline. No general value
// counter or synthetic context crosses JSON method dispatch.
func boundedStagedProofDigest(value StagedSelection) (string, error) {
	writer := &stagedJSONDigestWriter{sum: sha256.New()}
	bounded := &stagedCountWriter{writer: writer, remaining: 16 << 10}
	if err := json.NewEncoder(bounded).Encode(value); err != nil {
		return "", err
	}
	if !writer.haveLast || writer.last != '\n' {
		return "", ErrDurableStorageRecovery
	}
	return hex.EncodeToString(writer.sum.Sum(nil)), nil
}

func boundedStagedWireDigest(ctx context.Context, value any, limit int64) (string, error) {
	if _, err := agentmeta.JSONWireBound(ctx, value, limit); err != nil {
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
