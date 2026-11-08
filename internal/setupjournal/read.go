package setupjournal

import (
	"encoding/json"
	"errors"
	"os"
)

// Read loads the recovery record through its raw authority boundary. Calling
// UnmarshalJSON directly is required: encoding/json rejects malformed syntax
// before a custom decoder can preserve a possibly protected recovery record.
func Read(home string, journal *Journal) (err error) {
	if journal == nil {
		return errors.New("setup recovery destination is required")
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	raw, err := root.ReadFile("setup-transaction.json")
	if err != nil {
		return err
	}
	decodeErr := journal.UnmarshalJSON(raw)
	if decodeErr == nil || errors.Is(decodeErr, ErrUnsupportedRecoveryAuthority) {
		return decodeErr
	}
	// The raw boundary must run first to preserve protected malformed records.
	// For unprotected invalid JSON, retain the generic reader's SyntaxError so
	// legacy recovery can still offer its existing move-aside way out. The empty
	// destination cannot retain snapshot payloads or assign to the receiver.
	var discarded struct{}
	var syntaxErr *json.SyntaxError
	if fallbackErr := json.Unmarshal(raw, &discarded); errors.As(fallbackErr, &syntaxErr) {
		return fallbackErr
	}
	return decodeErr
}
