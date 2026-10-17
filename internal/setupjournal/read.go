package setupjournal

import (
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
	return journal.UnmarshalJSON(raw)
}
