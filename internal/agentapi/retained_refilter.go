package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
)

// RefilterRetainedSource owns the existing retained transform and transfers all
// returned output leases. The caller keeps the original input lease alive.
func RefilterRetainedSource(ctx context.Context, reg archive.SessionRegistration, adapter TranscriptFilter, input archive.SourceBundle, budget *NativeReadBudget) (out archive.SourceBundle, release func(), err error) {
	reservation := budget.Reservation()
	if budget == nil {
		return out, nil, ErrReadBudget
	}
	if err = ctx.Err(); err != nil {
		return out, nil, err
	}
	if retainedRefilterIdentityChanged(input, reg) {
		return out, nil, errors.New("retained refilter identity mismatch")
	}
	if err = input.ValidateHistory(); err != nil {
		return out, nil, err
	}
	const sizingScratch = 32 << 10
	if !reservation.Reserve(sizingScratch) {
		return out, nil, ErrReadBudget
	}
	largest := int64(0)
	for _, record := range input.NativeRecords {
		available := reservation.Available()
		n, e := jsonwire.Bound(ctx, record, available)
		if errors.Is(e, jsonwire.ErrLimit) {
			reservation.Refuse(available + 1)
		}
		if e != nil {
			err = e
			break
		}
		largest = max(largest, n)
	}
	for _, text := range input.NativeText {
		largest = max(largest, int64(len(text.Content)))
	}
	reservation.Release(sizingScratch)
	if err != nil {
		return out, nil, errors.Join(ErrReadBudget, err)
	}
	scanner := int64(64 << 10)
	for scanner < largest+1 && scanner < int64(archive.MaxRecordBytes+1) {
		scanner = min(scanner*2, int64(archive.MaxRecordBytes+1))
	}
	if largest > (reservation.Available()-scanner)/3 {
		reservation.Refuse(3*largest + scanner)
		return out, nil, ErrReadBudget
	}
	scratch := 3*largest + scanner
	if !reservation.Reserve(scratch) {
		return out, nil, ErrReadBudget
	}
	defer reservation.Release(scratch)
	var leases []func()
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			for i := len(leases) - 1; i >= 0; i-- {
				leases[i]()
			}
		})
	}
	defer func() {
		if err != nil {
			closeAll()
		}
	}()
	filtered, closeFilter, err := refilterRetainedRows(ctx, reg, adapter, input, budget)
	if err != nil {
		return out, nil, err
	}
	leases = append(leases, closeFilter)
	if reg.StagedImportSHA256 != "" {
		filtered.Format = input.Capture.SourceFormat
	}
	var decoded int64
	for _, row := range filtered.Records {
		if int64(len(row)) > reservation.Available()-decoded {
			reservation.Refuse(decoded + int64(len(row)))
			return out, nil, ErrReadBudget
		}
		decoded += int64(len(row))
	}
	if !reservation.Reserve(decoded) {
		return out, nil, ErrReadBudget
	}
	leases = append(leases, func() { reservation.Release(decoded) })
	out, err = archive.NewSourceBundle(reg, adapter, filtered, input.Capture.CapturedAt, input.SupplementalEvidence)
	if err != nil {
		return out, nil, err
	}
	out.Capture.Harness = input.Capture.Harness
	mergeRetainedNativeGaps(&out, input, reg)
	if err = out.ValidateHistory(); err != nil {
		return out, nil, err
	}
	// Ordinary maps are independently decoded; detach their envelope before ending
	// encoded row ownership. Graph, ordinal and text aliases retain that owner.
	if ordinaryRefilterEnvelope(out, filtered) {
		var closeEnvelope func()
		out, closeEnvelope, err = detachRefilterEnvelope(ctx, out, budget)
		if err != nil {
			return out, nil, err
		}
		leases = append(leases, closeEnvelope)
		leases[0]()
		leases[0] = func() {}
	}
	if err = ctx.Err(); err != nil {
		return out, nil, err
	}
	return out, closeAll, nil
}

func refilterRetainedRows(ctx context.Context, reg archive.SessionRegistration, adapter TranscriptFilter, input archive.SourceBundle, budget *NativeReadBudget) (archive.FilteredTranscript, func(), error) {
	reservation := budget.Reservation()
	var filtered archive.FilteredTranscript
	var closeFilter func()
	var err error
	if leased, ok := adapter.(LeasedTranscriptRefilter); ok {
		filtered, closeFilter, err = leased.RefilterLeased(ctx, input, reg.SessionStartedAt, budget)
		if err != nil {
			return filtered, nil, err
		}

	} else {
		bounded, ok := adapter.(BoundedTranscriptRefilter)
		if !ok {
			return filtered, nil, ErrReadBudget
		}
		capBytes := min(int64(32<<20), max(int64(0), (reservation.Available()-(64<<10))/2))
		if capBytes <= 0 {
			reservation.Refuse((64 << 10) + 2)
			return filtered, nil, ErrReadBudget
		}
		if !reservation.Reserve(capBytes) {
			return filtered, nil, ErrReadBudget
		}
		filtered, err = bounded.RefilterBounded(ctx, input, reg.SessionStartedAt, ReadLimits{Records: archive.MaxHistoryRecords, FilteredBytes: capBytes, ReadBudget: budget})
		if err != nil {
			reservation.Release(capBytes)
			return filtered, nil, err
		}
		n := int64(filtered.Boundary.RetainedBytes)
		if n < 0 || n > capBytes {
			reservation.Release(capBytes)
			return filtered, nil, ErrReadBudget
		}
		reservation.Release(capBytes - n)
		closeFilter = func() { reservation.Release(n) }
	}
	return filtered, closeFilter, nil
}

func detachRefilterEnvelope(ctx context.Context, out archive.SourceBundle, budget *NativeReadBudget) (archive.SourceBundle, func(), error) {
	reservation := budget.Reservation()
	envelope := out
	envelope.NativeRecords = nil
	envelope.NativeText = nil
	envelope.Ordinals = nil
	if !reservation.Reserve((32 << 10)) {
		return out, nil, ErrReadBudget
	}
	n, e := jsonwire.Bound(ctx, envelope, reservation.Available()/2)
	reservation.Release((32 << 10))
	if e != nil {
		return out, nil, e
	}
	if !reservation.Reserve(2 * n) {
		return out, nil, ErrReadBudget
	}
	raw, e := json.Marshal(envelope)
	if e != nil {
		reservation.Release(2 * n)
		return out, nil, e
	}
	var detached archive.SourceBundle
	e = json.Unmarshal(raw, &detached)
	reservation.Release(n)
	if e != nil {
		reservation.Release(n)
		return out, nil, e
	}
	detached.NativeRecords = out.NativeRecords
	out = detached
	var once sync.Once
	return out, func() { once.Do(func() { reservation.Release(n) }) }, nil
}

func ordinaryRefilterEnvelope(out archive.SourceBundle, filtered archive.FilteredTranscript) bool {
	return out.History == nil && len(out.Ordinals) == 0 && filtered.History == nil && len(filtered.Ordinals) == 0 && len(filtered.Text) == 0
}

func retainedRefilterIdentityChanged(input archive.SourceBundle, reg archive.SessionRegistration) bool {
	return input.SchemaVersion != archive.SourceSchemaVersion && input.SchemaVersion != archive.HistorySourceSchemaVersion || input.ArchiveSessionID != reg.ArchiveSessionID || input.NativeSessionID != reg.NativeSessionID || input.ProjectID != reg.ProjectID || input.Capture.Harness.Name != reg.Harness.Name || input.ParentSessionID != reg.ParentSessionID && (!RetainedNativeChildOwned(reg, input) || input.ParentSessionID != "")
}

// RetainedNativeChildOwned recognizes only an already admitted child owner from
// its verified retained header or its exact supported persisted binding.
func RetainedNativeChildOwned(reg archive.SessionRegistration, input archive.SourceBundle) bool {
	if !reg.NativeChild || input.ArchiveSessionID != reg.ArchiveSessionID || input.NativeSessionID != reg.NativeSessionID || input.ProjectID != reg.ProjectID || input.Capture.Harness.Name != reg.Harness.Name {
		return false
	}
	if input.NativeChild {
		return true
	}
	binding := reg.CodexBinding
	return binding != nil && binding.Child && binding.Validate() == nil && reg.Harness.Name == archive.HarnessCodex && binding.NativeThreadID == reg.NativeSessionID && binding.Home != "" && (binding.ParentID == "" || reg.ParentNativeSessionID == binding.ParentID) && (binding.RootID == "" || reg.NativeRootSessionID == binding.RootID) && (binding.Home == "" || reg.NativeSourceHome == binding.Home)
}

func mergeRetainedNativeGaps(out *archive.SourceBundle, input archive.SourceBundle, reg archive.SessionRegistration) {
	gaps := out.Capture.Gaps
	out.Capture.Gaps = make([]archive.CaptureGap, 0, len(input.Capture.Gaps))
	for _, gap := range input.Capture.Gaps {
		if gap.Code != "native_parent_link_pending" || !RetainedNativeChildOwned(reg, input) || input.ParentSessionID != "" || reg.ParentSessionID == "" {
			out.Capture.Gaps = append(out.Capture.Gaps, gap)
		}
	}
	for _, gap := range gaps {
		if !slices.Contains(out.Capture.Gaps, gap) {
			out.Capture.Gaps = append(out.Capture.Gaps, gap)
		}
	}
}
