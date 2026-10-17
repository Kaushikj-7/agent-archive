package state

import (
	"encoding/json"
	"os"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Registration authority is classified before syntax failures can become
// quarantine or derived-index repair. All other state keeps its decoder.
func decodeRegistrationJSON(raw []byte, value any) error {
	if reg, ok := value.(*archive.SessionRegistration); ok {
		return archive.DecodeSessionRegistration(raw, reg)
	}
	return json.Unmarshal(raw, value)
}

func readRegistrationJSON(path string, value any) error {
	if _, ok := value.(*archive.SessionRegistration); !ok {
		return local.Read(path, value)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeRegistrationJSON(raw, value)
}
