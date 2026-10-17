package archive

import "encoding/json"

// ImportBatch is the ID of the backfill import that registered a session,
// as SessionRegistration.ImportBatch records it. It is not a string and
// cannot be compared with ==, so the compiler holds every caller to
// SessionRegistration.InBatch, which also requires the registration to be
// an import and the ID to be set: a batch with an empty ID, or a hook
// registration that carries one, never matches. It is stored as the plain
// JSON string it always was, and left out when empty.
type ImportBatch struct {
	id string
	// A func field makes the type, and so SessionRegistration, not
	// comparable: an import ID copied out of a registration cannot be
	// matched with == either.
	_ [0]func()
}

// NewImportBatch is the ImportBatch of the import with this ID, for the
// registrations backfill writes.
func NewImportBatch(id string) ImportBatch { return ImportBatch{id: id} }

// IsZero reports that no import ID is recorded. encoding/json's omitzero
// uses it, so an empty one is left out as before.
func (b ImportBatch) IsZero() bool { return b.id == "" }

// Recorded is the ID as recorded, "" when there is none, for listing the
// IDs registrations carry (so a new import picks an unused one, or a
// project is attributed to the import that kept it). It never decides
// whether a registration belongs to an import: that is InBatch, and a guard
// test in internal/backfill holds callers to it.
func (b ImportBatch) Recorded() string { return b.id }

// matchesNativeImportAttribution checks immutable creating provenance, including
// detached receipts. It does not authorize operational batch membership.
func (b ImportBatch) matchesNativeImportAttribution(id string) bool { return id != "" && b.id == id }

// MarshalJSON writes the ID as a JSON string.
func (b ImportBatch) MarshalJSON() ([]byte, error) { return json.Marshal(b.id) }

// UnmarshalJSON reads the ID from a JSON string; null leaves it empty.
func (b *ImportBatch) UnmarshalJSON(data []byte) error {
	var id *string
	if err := json.Unmarshal(data, &id); err != nil {
		return err
	}
	b.id = ""
	if id != nil {
		b.id = *id
	}
	return nil
}

// InBatch reports whether the registration was registered by the import id:
// backfill registered it (Imported), and it carries that import's ID, which
// is not empty. It is the only test of whether a registration belongs to an
// import, so a batch with a missing or empty ID, or a hook registration that
// carries an ID, never pulls a hook-captured session into an import's undo,
// upload, or history.
func (r SessionRegistration) InBatch(id string) bool {
	if r.NativeImport != nil {
		return id != "" && r.ValidateNativeImport() == nil && r.NativeImport.Membership.State == "attached" && r.NativeImport.Confirmation.BatchID == id
	}
	return id != "" && r.Imported() && r.ImportBatch.id == id
}
