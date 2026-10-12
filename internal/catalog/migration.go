package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// MigrationKey is a durable private checkpoint inside the isolated destination.
const MigrationKey = "catalog-v4/migrations/current.json"

// CutoverProof identifies reviewed provider authority for credential revocation,
// source read-only policy and all participating writers' protocol version.
// This cannot be supplied by flags or by a local process/home inventory.
type CutoverProof struct {
	ID          string
	Source      string
	Destination string
	Protocol    uint64
}

// CutoverAuthority verifies actual destination policy and revoked old write
// grants. S3/R2 implement no authority until reviewed live evidence exists.
// A private synthetic fixture can implement it for migration protocol tests.
type CutoverAuthority interface {
	VerifyCatalogCutover(context.Context, destination.Config, destination.Config) (CutoverProof, error)
}

// MigrationPhase names a durable migration checkpoint phase.
type MigrationPhase string

// Supported migration checkpoint phases.
const (
	MigrationInitializing      MigrationPhase = "initializing"
	MigrationRollbackPreparing MigrationPhase = "rollback-preparing"
	MigrationCopying           MigrationPhase = "copying"
	MigrationVerifying         MigrationPhase = "verifying"
	MigrationVerified          MigrationPhase = "verified"
	MigrationActive            MigrationPhase = "active"
	MigrationRollback          MigrationPhase = "rollback"
)

// CatalogMigration resumes bounded source pages under exact global ownership.
// Original metadata and source/history bytes are preserved, including retention
// timestamps. Activation and rollback are separate durable checkpoint phases.
//
//revive:disable-next-line:exported -- Keep accepted protocol API name.
type CatalogMigration struct {
	Protocol     uint64             `json:"protocol"`
	Source       destination.Config `json:"source"`
	Destination  destination.Config `json:"destination"`
	Phase        MigrationPhase     `json:"phase"`
	Cursor       string             `json:"cursor"`
	ExpectedHead string             `json:"expected_head"`
	Owner        string             `json:"owner"`
	ID           string             `json:"id"`
	Proof        string             `json:"proof"`
	VerifiedRoot ObjectRef          `json:"verified_root"`
	Copied       uint64             `json:"copied"`
}

// Migration uses an isolated candidate destination and credential authority.
// It never changes configuration; the CLI activates only after Verify succeeds.
type Migration struct {
	Source         storage.ObjectStore
	Destination    storage.ObjectStore
	Authority      CutoverAuthority
	State          CatalogMigration
	writer         *Writer
	checkpointETag string
	intent         *migrationIntent
}

func destinationIdentity(cfg destination.Config) string { return config.DestinationID(cfg) }

func providerNamespace(cfg destination.Config) (string, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	endpoint := ""
	if provider == destination.ProviderR2 {
		var err error
		endpoint, err = destination.R2Endpoint(cfg.R2Endpoint, cfg.R2AccountID)
		if err != nil {
			return "", err
		}
	}
	return provider + "\x00" + strings.ToLower(endpoint) + "\x00" + cfg.Bucket, nil
}

func isolated(source, target destination.Config) bool {
	aNamespace, aErr := providerNamespace(source)
	bNamespace, bErr := providerNamespace(target)
	if aErr != nil || bErr != nil {
		return false
	}
	if aNamespace != bNamespace {
		return true
	}
	a, b := strings.Trim(source.Prefix, "/"), strings.Trim(target.Prefix, "/")
	if a == "" || b == "" || a == b {
		return false
	}
	return !strings.HasPrefix(a, b+"/") && !strings.HasPrefix(b, a+"/")
}

func (m *Migration) proof(ctx context.Context) (CutoverProof, error) {
	if m.Authority == nil {
		return CutoverProof{}, errors.New("reviewed credential cutover authority is required")
	}
	proof, err := m.Authority.VerifyCatalogCutover(ctx, m.State.Source, m.State.Destination)
	if err != nil {
		return proof, err
	}
	if proof.ID == "" || proof.Protocol != 10 || proof.Source != destinationIdentity(m.State.Source) || proof.Destination != destinationIdentity(m.State.Destination) {
		return proof, errors.New("credential cutover proof differs from migration destination or writer protocol")
	}
	return proof, nil
}

// OpenMigration validates qualification and isolation before any write. A
// checkpoint is resumed only for its original destination and credential proof.
func OpenMigration(ctx context.Context, source, target storage.ObjectStore, sourceConfig, targetConfig destination.Config, authority CutoverAuthority) (*Migration, error) {
	if !isolated(sourceConfig, targetConfig) || targetConfig.EffectiveArchiveFormat() != destination.FormatCatalogV4 || sourceConfig.EffectiveArchiveFormat() != destination.FormatLegacy {
		return nil, errors.New("migration requires isolated legacy source and catalog destination")
	}
	w, err := New(target)
	if err != nil {
		return nil, err
	}
	if _, ok := source.(storage.PageLister); !ok {
		return nil, errors.New("migration requires bounded source page listing")
	}
	if _, ok := source.(storage.LimitedVersionedGetter); !ok {
		return nil, errors.New("migration requires bounded exact source metadata revisions")
	}
	if _, ok := source.(storage.LimitedGetter); !ok {
		return nil, errors.New("migration requires bounded source reads")
	}
	m := &Migration{Source: source, Destination: target, Authority: authority, State: CatalogMigration{Protocol: 10, Source: sourceConfig, Destination: targetConfig}, writer: w}
	proof, err := m.proof(ctx)
	if err != nil {
		return nil, err
	}
	raw, checkpointVersion, err := w.versioned.GetCatalogVersion(ctx, MigrationKey, 64<<10)
	if err == nil {
		state, decodeErr := decodeMigration(raw)
		if decodeErr != nil || destinationIdentity(state.Source) != destinationIdentity(sourceConfig) || destinationIdentity(state.Destination) != destinationIdentity(targetConfig) || state.Proof != proof.ID {
			return nil, errors.New("migration checkpoint differs from cutover authority")
		}
		m.State = state
		m.checkpointETag = checkpointVersion.ETag
		if err = m.resumeIntent(ctx, proof); err != nil {
			return nil, err
		}
		if m.intent != nil && m.intent.Kind == migrationInitialize {
			if err = m.resumeInitialization(ctx); err != nil {
				return nil, err
			}
		}
		return m, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	if err = m.resumeIntent(ctx, proof); err != nil {
		return nil, err
	}
	if m.intent != nil {
		if err = m.resumeInitialization(ctx); err != nil {
			return nil, err
		}
		return m, nil
	}
	// Never claim mixed namespace completeness, even before sealing or copying.
	objects, err := target.List(ctx, "")
	if err != nil {
		return nil, err
	}
	if len(objects) != 0 {
		return nil, errors.New("catalog migration destination must be empty and isolated")
	}
	id, err := NewMutationID()
	if err != nil {
		return nil, err
	}
	owner, err := NewMutationID()
	if err != nil {
		return nil, err
	}
	m.State.Owner, m.State.ID, m.State.Proof, m.State.Phase = owner, id, proof.ID, MigrationInitializing
	if err = m.beginIntent(ctx, migrationInitialize, nil); err != nil {
		return nil, err
	}
	if err = m.resumeInitialization(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Migration) save(ctx context.Context) error {
	if err := m.State.validate(); err != nil {
		return err
	}
	ctx = m.authorityContext(ctx)
	if m.intent != nil {
		if err := m.checkIntentCheckpoint(ctx); err != nil {
			return err
		}
	} else if err := m.writer.checkWriteAuthority(ctx); err != nil {
		return err
	}
	raw, err := json.Marshal(m.State)
	if err != nil {
		return err
	}
	etag, err := m.writer.conditional.PutConditional(ctx, MigrationKey, raw, storage.PutCondition{MatchETag: m.checkpointETag, CreateOnly: m.checkpointETag == ""})
	if err != nil {
		actual, version, e := m.writer.versioned.GetCatalogVersion(ctx, MigrationKey, 64<<10)
		if e != nil || string(actual) != string(raw) {
			return errors.Join(ErrCommitUnknown, err, e)
		}
		etag = version.ETag
	}
	m.checkpointETag = etag
	return nil
}

func (m *Migration) held(ctx context.Context) error {
	state, _, err := m.writer.Coordinator().read(ctx)
	if err != nil {
		return err
	}
	if state.Seal != m.State.Owner || len(state.Owners) != 0 {
		return ErrAdmissionClosed
	}
	proof, err := m.proof(ctx)
	if err != nil {
		return err
	}
	if proof.ID != m.State.Proof {
		return errors.New("cutover authority changed; migration remains sealed")
	}
	return nil
}

func (m *Migration) copyMetadata(ctx context.Context, obj storage.Object) error {
	ctx = m.authorityContext(ctx)
	raw, etag, err := m.Source.(storage.LimitedVersionedGetter).GetLimitedVersioned(ctx, obj.Key, 32<<20)
	if err != nil {
		return err
	}
	if etag == "" || etag != obj.ETag {
		return errors.New("source inventory changed during migration")
	}
	var metadata archive.Metadata
	if err = json.Unmarshal(raw, &metadata); err != nil {
		return err
	}
	key, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil || key != obj.Key {
		return errors.New("source metadata identity mismatch")
	}
	refs, err := canonicalSourceReferences(metadata)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if ref.CompressedBytes <= 0 || ref.CompressedBytes > 64<<20 {
			return storage.ErrObjectTooLarge
		}
		bytes, err := m.Source.(storage.LimitedGetter).GetLimited(ctx, ref.Key, int64(ref.CompressedBytes))
		if err != nil {
			return err
		}
		if len(bytes) != ref.CompressedBytes || !storage.VerifySHA256(bytes, ref.SHA256) {
			return storage.ErrChecksumMismatch
		}
		if err = m.writer.checkWriteAuthority(ctx); err != nil {
			return err
		}
		_, err = m.writer.conditional.PutConditional(ctx, ref.Key, bytes, storage.PutCondition{CreateOnly: true})
		if err != nil {
			existing, e := m.writer.readRef(ctx, ObjectRef{ref.Key, ref.SHA256}, int64(ref.CompressedBytes))
			if e != nil {
				return errors.Join(err, e)
			}
			if string(existing) != string(bytes) {
				return storage.ErrChecksumMismatch
			}
		}
	}
	ctx = context.WithValue(ctx, writeAuthorityKey{}, writeAuthority{writer: m.writer, seal: m.State.Owner})
	ref, err := m.writer.putImmutableAdmitted(ctx, KindMetadata, raw)
	if err != nil {
		return err
	}
	mutation := CatalogMutation{ID: m.State.ID + "/" + storage.SHA256Hex([]byte(obj.Key)), SessionKey: obj.Key, Next: &CatalogEntry{Metadata: ref, Summary: metadata}}
	// The durable migration seal is exclusive authority; ordinary commit
	// admission is closed. Lost acknowledgements resolve via frozen receipts.
	_, err = m.writer.commitAdmitted(ctx, mutation)
	return err
}

// Step copies one bounded page and checkpoints only after every copied source,
// preserved history and immutable metadata has been verified and committed.
// Retrying a crashed page uses frozen receipts and cannot double-publish.
func (m *Migration) Step(ctx context.Context) (bool, error) {
	if m.State.Phase == MigrationVerifying {
		err := m.Verify(ctx)
		return err == nil, err
	}
	if m.State.Phase != MigrationCopying {
		return m.State.Phase == MigrationVerified || m.State.Phase == MigrationActive, nil
	}
	if err := m.held(ctx); err != nil {
		return false, err
	}
	page, err := m.Source.(storage.PageLister).ListPage(ctx, "sessions/", m.State.Cursor, 64)
	if err != nil {
		return false, err
	}
	prior := m.State
	checkpointed := false
	defer func() {
		if !checkpointed {
			m.State = prior
		}
	}()
	for _, obj := range page.Objects {
		if !metadataKey(obj.Key) {
			continue
		}
		if err = m.copyMetadata(ctx, obj); err != nil {
			return false, err
		}
		m.State.Copied++
	}
	_, etag, err := m.writer.gcHead(m.authorityContext(ctx), func(ctx context.Context, writer *Writer) error {
		if writer != m.writer {
			return ErrAdmissionClosed
		}
		return writer.checkWriteAuthority(ctx)
	})
	if err != nil {
		return false, err
	}
	m.State.ExpectedHead = etag
	m.State.Cursor = page.Next
	if page.Next == "" {
		m.State.Phase = MigrationVerifying
	}
	if err = m.save(ctx); err != nil {
		return false, err
	}
	checkpointed = true
	if page.Next == "" {
		err := m.Verify(ctx)
		return err == nil, err
	}
	return false, nil
}

// Verify compares the complete post-cutoff source inventory with the catalog
// and hashes every source/history object on both sides. No LIST coverage hint
// or successful batch checkpoint is treated as exhaustive completeness proof.
func (m *Migration) Verify(ctx context.Context) error {
	if m.State.Phase != MigrationVerifying && m.State.Phase != MigrationVerified {
		return errors.New("migration is not ready for exhaustive verification")
	}
	if err := m.held(ctx); err != nil {
		return err
	}
	snapshot, err := openSnapshot(ctx, m.Destination, nil, false)
	if err != nil {
		return err
	}
	rows, err := migrationRows(ctx, snapshot)
	if err != nil {
		return err
	}
	remaining := map[string]CatalogEntry{}
	for _, row := range rows {
		remaining[row.Key] = row.Entry
	}
	cursor := ""
	for {
		page, err := m.Source.(storage.PageLister).ListPage(ctx, "sessions/", cursor, 64)
		if err != nil {
			return err
		}
		for _, obj := range page.Objects {
			if !metadataKey(obj.Key) {
				continue
			}
			entry, ok := remaining[obj.Key]
			if !ok {
				return fmt.Errorf("source session missing from catalog: %s", obj.Key)
			}
			raw, etag, err := m.Source.(storage.LimitedVersionedGetter).GetLimitedVersioned(ctx, obj.Key, 32<<20)
			if err != nil {
				return err
			}
			if etag != obj.ETag || !storage.VerifySHA256(raw, entry.Metadata.SHA256) {
				return errors.New("source/catalog exhaustive oracle differs")
			}
			if err = m.writer.verifyEntry(ctx, obj.Key, &entry); err != nil {
				return err
			}
			refs, err := entry.Summary.SourceReferences()
			if err != nil {
				return err
			}
			for _, ref := range refs {
				bytes, e := m.Source.(storage.LimitedGetter).GetLimited(ctx, ref.Key, int64(ref.CompressedBytes))
				if e != nil {
					return e
				}
				if len(bytes) != ref.CompressedBytes || !storage.VerifySHA256(bytes, ref.SHA256) {
					return storage.ErrChecksumMismatch
				}
			}
			delete(remaining, obj.Key)
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if len(remaining) != 0 {
		return errors.New("catalog has entries outside exhaustive source inventory")
	}
	objects, err := m.Destination.List(ctx, "sessions/")
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if metadataKey(obj.Key) {
			return errors.New("mixed legacy/catalog namespace cannot be activated")
		}
	}
	if err = m.held(ctx); err != nil {
		return err
	}
	_, etag, err := m.writer.Head(ctx)
	if err != nil {
		return err
	}
	if etag != snapshot.etag {
		return ErrConflict
	}
	m.State.ExpectedHead = etag
	m.State.VerifiedRoot = snapshot.Root()
	m.State.Phase = MigrationVerified
	return m.save(ctx)
}

func migrationRows(ctx context.Context, snapshot *Snapshot) ([]Row, error) {
	var rows []Row
	cursor := ""
	for {
		page, err := snapshot.Query(ctx, Query{Index: IdentityIndex}, cursor, 1000)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page.Rows...)
		if page.Next == "" {
			return rows, nil
		}
		cursor = page.Next
	}
}

// Activate revalidates the exhaustive oracle and credential authority before
// making readers visible. Admission stays sealed until local configuration is
// durably switched and FinishActivation acknowledges that step.
func (m *Migration) Activate(ctx context.Context) error {
	if m.State.Phase == MigrationActive {
		state, _, err := m.writer.Coordinator().read(ctx)
		if err != nil {
			return err
		}
		proof, err := m.proof(ctx)
		if err != nil {
			return err
		}
		if proof.ID != m.State.Proof || state.Mode != admissionActive || state.Proof != m.State.Proof {
			return ErrAdmissionClosed
		}
		if state.Seal == "" {
			return nil
		}
		return m.held(ctx)
	}
	if err := m.Verify(ctx); err != nil {
		return err
	}
	if err := m.writer.Coordinator().Activate(ctx, m.State.Owner, m.State.Proof); err != nil {
		return err
	}
	m.State.Phase = MigrationActive
	return m.save(ctx)
}

// FinishActivation releases the global writer seal after configuration cutover.
func (m *Migration) FinishActivation(ctx context.Context) error {
	if m.State.Phase != MigrationActive {
		return errors.New("migration is not active")
	}
	state, _, err := m.writer.Coordinator().read(ctx)
	if err != nil {
		return err
	}
	if state.Seal == "" && state.Mode == admissionActive && state.Proof == m.State.Proof {
		return nil
	}
	return m.writer.Coordinator().Release(ctx, m.State.Owner)
}

// Rollback refuses any post-cutover catalog mutation. Original source remains
// read-only, so rollback cannot discard newer publications or reenable legacy
// writes. Configuration rollback is allowed only under this exact-root gate.
func (m *Migration) Rollback(ctx context.Context) error {
	proof, err := m.proof(ctx)
	if err != nil || proof.ID != m.State.Proof {
		return errors.Join(ErrAdmissionClosed, err)
	}
	if m.State.Phase == MigrationRollback {
		state, _, err := m.writer.Coordinator().read(ctx)
		if err != nil {
			return err
		}
		if state.Mode != admissionRollback || state.Proof != m.State.Proof || state.Seal != m.State.Owner || state.Hold != "" || state.GCLink != nil || len(state.Owners) != 0 || state.Intent != nil {
			return ErrAdmissionClosed
		}
		return m.unchangedHead(ctx)
	}
	if m.State.Phase != MigrationActive && m.State.Phase != MigrationRollbackPreparing {
		return errors.New("only active migration can roll back")
	}
	if m.intent == nil {
		predecessor := m.State
		owner, err := NewMutationID()
		if err != nil {
			return err
		}
		m.State.Owner, m.State.Phase = owner, MigrationRollbackPreparing
		if err = m.beginIntent(ctx, migrationRollbackIntent, &predecessor); err != nil {
			return err
		}
	}
	if m.intent.Kind != migrationRollbackIntent {
		return ErrAdmissionClosed
	}
	if err = m.sealIntent(ctx); err != nil {
		return err
	}
	if err = m.save(ctx); err != nil {
		return err
	}
	if err = m.unchangedHead(ctx); err != nil {
		return err
	}
	if err = m.writer.Coordinator().Deactivate(m.intentContext(ctx), m.State.Owner); err != nil {
		return err
	}
	m.State.Phase = MigrationRollback
	if err = m.save(ctx); err != nil {
		return err
	}
	return m.clearIntent(ctx)
}

// MigrationCheckpoint reads the original source/config identity when activation
// was interrupted after local configuration switched to the new destination.
func MigrationCheckpoint(ctx context.Context, target storage.ObjectStore) (CatalogMigration, error) {
	w, err := New(target)
	if err != nil {
		return CatalogMigration{}, err
	}
	raw, _, err := w.versioned.GetCatalogVersion(ctx, MigrationKey, 64<<10)
	if err != nil {
		return CatalogMigration{}, err
	}
	return decodeMigration(raw)
}

func (m *Migration) authorityContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, writeAuthorityKey{}, writeAuthority{writer: m.writer, seal: m.State.Owner})
}

func decodeMigration(raw []byte) (CatalogMigration, error) {
	var state CatalogMigration
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 64<<10 || decoder.Decode(&state) != nil || decoder.Decode(new(any)) != io.EOF {
		return state, errors.New("invalid migration checkpoint")
	}
	return state, state.validate()
}

func (state CatalogMigration) validate() error {
	invalid := errors.New("invalid migration checkpoint descriptor")
	if state.Protocol != 10 || !exactHex(state.ID, 24) || !exactHex(state.Owner, 24) || state.Proof == "" || len(state.Proof) > 4096 || len(state.Cursor) > 4096 || len(state.ExpectedHead) > 4096 || !isolated(state.Source, state.Destination) || state.Source.EffectiveArchiveFormat() != destination.FormatLegacy || state.Destination.EffectiveArchiveFormat() != destination.FormatCatalogV4 {
		return invalid
	}
	switch state.Phase {
	case MigrationInitializing, MigrationCopying:
	case MigrationVerifying, MigrationVerified, MigrationActive, MigrationRollbackPreparing, MigrationRollback:
		if state.Cursor != "" || state.ExpectedHead == "" {
			return invalid
		}
	default:
		return invalid
	}
	if state.VerifiedRoot != (ObjectRef{}) && state.VerifiedRoot.validate() != nil {
		return invalid
	}
	if (state.Phase == MigrationVerified || state.Phase == MigrationActive || state.Phase == MigrationRollback) && state.Copied > 0 && state.VerifiedRoot == (ObjectRef{}) {
		return invalid
	}
	return nil
}
