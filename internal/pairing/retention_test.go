package pairing

import "testing"

func TestForeverPayload(t *testing.T) {
	p := testPayload()
	p.RetentionDays = 0
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	// Published v1 receivers enforce this pinned finite range before installation.
	if !(p.RetentionDays < 1 || p.RetentionDays > 36500) {
		t.Fatal("old v1 validation would accept forever")
	}
	p.RetentionDays = -1
	if p.Validate() == nil {
		t.Fatal("negative retention accepted")
	}
}
