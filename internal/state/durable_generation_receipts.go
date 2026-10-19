package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/local"
)

// The census needs only proof that these payloads have been cleared. Never
// materialize a nested publication merely to reject an uncleared receipt.
type clearedGenerationPayload struct{}

func (*clearedGenerationPayload) UnmarshalJSON(raw []byte) error {
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ErrDurableStorageRecovery
	}
	return nil
}

type generationReceiptJSON generationRecovery

// The census, replay and read eligibility share this bounded receipt proof.
// Directory observation caching never proves in-place rewritten contents.
func (s *Store) completedGenerationReceipt(home *local.RootedHome, id string) (complete bool, err error) {
	reservation := s.readReservation()
	if err := s.durableContext().Err(); err != nil {
		return false, err
	}
	path := filepath.Join(generationRecoveryDir, id+".json")
	before, err := home.Root.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	if before.Size() > durableControlBytes {
		return false, nil
	}
	if s.resourceBudget != nil {
		if !reservation.Reserve(4 * durableControlBytes) {
			return false, errStateBudget
		}
		defer reservation.Release(4 * durableControlBytes)
	}
	file, err := home.Root.Open(path)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			complete = false
		}
	}()
	opened, err := file.Stat()
	if err != nil || !sameDurableStamp(before, opened) {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	var receipt generationRecovery
	decoder := json.NewDecoder(io.LimitReader(file, durableControlBytes+1))
	decoder.DisallowUnknownFields()
	wire := struct {
		*generationReceiptJSON
		Registration clearedGenerationPayload `json:"registration"`
		Pending      clearedGenerationPayload `json:"pending"`
		Request      clearedGenerationPayload `json:"request"`
	}{generationReceiptJSON: (*generationReceiptJSON)(&receipt)}
	decodeErr := decoder.Decode(&wire)
	if decodeErr == nil {
		var extra any
		if !errors.Is(decoder.Decode(&extra), io.EOF) {
			decodeErr = ErrDurableStorageRecovery
		}
	}
	named, namedErr := home.Root.Lstat(path)
	if namedErr != nil || !sameDurableStamp(before, named) {
		return false, errors.Join(ErrDurableStorageRecovery, namedErr)
	}
	if err := errors.Join(home.Check(), s.durableContext().Err()); err != nil {
		return false, err
	}
	return decodeErr == nil && validGenerationRecoveryVersion(receipt) && receipt.Previous == id && safeFileComponent(receipt.Next) && receipt.Key.Validate() == nil && receipt.Complete && receipt.Registration == nil && receipt.Pending == nil && receipt.Request == nil, nil
}

// Replay consumes only the cleared receipt proof, not unrelated pending content.
// ResumeGenerationRecoveries has already checked the current rooted config.
func (s *Store) checkCompletedGenerationRecoveryFile(id string) (err error) {
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	defer func() {
		err = errors.Join(err, home.Check(), home.Close())
		if err != nil {
			err = errors.Join(ErrDurableStorageRecovery, err)
		}
	}()
	complete, err := s.completedGenerationReceipt(home, id)
	if err != nil || !complete {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}
