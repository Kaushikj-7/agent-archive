package pairing

import (
	"strings"
	"testing"
)

func TestHistoricalProjectWireRoundTripAndBoundedIdentity(t *testing.T) {
	// KDF tests stay sequential to bound simultaneous 64 MiB derivations.
	payload := testPayload()
	payload.HistoricalProjects = []HistoricalProject{{ID: strings.Repeat("a", 32), Label: "Retired service", Enabled: true}, {ID: strings.Repeat("b", 32), Label: "Retired service", Enabled: false}}
	const code = "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
	sealed, err := Seal(payload, code)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Open(sealed, code, payload.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.HistoricalProjects) != 2 || decoded.HistoricalProjects[0].ID != payload.HistoricalProjects[0].ID || decoded.HistoricalProjects[1].Enabled {
		t.Fatal("historical policy changed on wire")
	}
	payload.HistoricalProjects = append(payload.HistoricalProjects, payload.HistoricalProjects[0])
	if payload.Validate() == nil {
		t.Fatal("duplicate historical IDs accepted")
	}
	payload.HistoricalProjects = []HistoricalProject{{ID: strings.Repeat("a", 32), Label: "Private\nlabel", Enabled: true}}
	if payload.Validate() == nil {
		t.Fatal("control characters in public historical label accepted")
	}
}
