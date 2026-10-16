package storage_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type publicationFaultStore struct {
	*storagetest.MemoryStore
	puts            int
	stats           int
	failKey         string
	uncertain       bool
	corruptMetadata bool
	metadataKey     string
}

func (s *publicationFaultStore) Put(ctx context.Context, key string, data []byte) error {
	s.puts++
	if key == s.failKey {
		return errors.New("synthetic interrupted write")
	}
	if key == s.metadataKey && s.corruptMetadata {
		data = []byte("corrupt committed metadata")
	}
	if err := s.MemoryStore.Put(ctx, key, data); err != nil {
		return err
	}
	if key == s.metadataKey && s.uncertain {
		s.uncertain = false
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (s *publicationFaultStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	s.stats++
	return s.MemoryStore.Stat(ctx, key)
}

func sourceSetFixture() []storage.SourcePublication {
	var out []storage.SourcePublication
	for _, body := range []string{"current filtered revision", "preserved filtered revision"} {
		bytes := []byte(body)
		digest := storage.SHA256Hex(bytes)
		out = append(out, storage.SourcePublication{Key: "sessions/codex/s/source." + digest + ".jsonl.gz", SHA256: digest, Size: len(bytes), Bytes: bytes})
	}
	return out
}

type sourceSetBoundary string

const (
	sourceSetCurrent   sourceSetBoundary = "current"
	sourceSetPreserved sourceSetBoundary = "preserved"
	sourceSetMetadata  sourceSetBoundary = "metadata"
	sourceSetUncertain sourceSetBoundary = "uncertain"
	sourceSetCompleted sourceSetBoundary = "completed"
)

func TestSourceSetInterruptionAndExactNextReplay(t *testing.T) {
	for _, boundary := range []sourceSetBoundary{sourceSetCurrent, sourceSetPreserved, sourceSetMetadata, sourceSetUncertain, sourceSetCompleted} {
		t.Run(string(boundary), func(t *testing.T) {
			sources := sourceSetFixture()
			key := "sessions/codex/s/metadata.json"
			remote := &publicationFaultStore{MemoryStore: storagetest.NewMemoryStore(), metadataKey: key}
			switch boundary {
			case sourceSetCurrent:
				remote.failKey = sources[0].Key
			case sourceSetPreserved:
				remote.failKey = sources[1].Key
			case sourceSetMetadata:
				remote.failKey = key
			case sourceSetUncertain:
				remote.uncertain = true
			case sourceSetCompleted:
				// The uninterrupted publication is the successful control.
			}
			next := []byte("exact frozen next metadata")
			prior := storage.MetadataPredecessor{Known: true}
			err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, next, prior, storage.RetryPolicy{MaxAttempts: 1})
			if boundary == sourceSetCompleted {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("interruption did not surface")
			}
			if boundary != sourceSetUncertain && boundary != sourceSetCompleted {
				if _, err := remote.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
					t.Fatal("metadata preceded complete source verification", err)
				}
			}
			remote.failKey = ""
			if err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, next, prior, storage.RetryPolicy{MaxAttempts: 1}); err != nil {
				t.Fatal(err)
			}
			puts := remote.puts
			stats := remote.stats
			if err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, next, storage.MetadataPredecessor{}, storage.RetryPolicy{MaxAttempts: 1}); err != nil {
				t.Fatal("exact next must finish even with unknown predecessor", err)
			}
			if remote.puts != puts || remote.stats-stats != len(sources) {
				t.Fatal("replay must verify each source exactly once without puts", remote.puts-puts, remote.stats-stats)
			}
		})
	}
}

func TestSourceSetPredecessorConflictNeverOverwritesWinner(t *testing.T) {
	for _, mode := range []string{"known-present", "unknown", "absent", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			sources := sourceSetFixture()
			key := "sessions/codex/s/metadata.json"
			remote := &publicationFaultStore{MemoryStore: storagetest.NewMemoryStore(), metadataKey: key}
			winner := []byte("different committed owner or mutation")
			if mode == "unreadable" {
				winner = []byte("{incomplete")
			}
			if err := remote.MemoryStore.Put(t.Context(), key, winner); err != nil {
				t.Fatal(err)
			}
			prior := storage.MetadataPredecessor{}
			if mode == "known-present" {
				prior = storage.MetadataPredecessor{Known: true, Exists: true, SHA256: storage.SHA256Hex([]byte("obsolete predecessor"))}
			}
			if mode == "absent" {
				prior.Known = true
			}
			err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, []byte("next"), prior, storage.RetryPolicy{MaxAttempts: 1})
			if !errors.Is(err, storage.ErrPublicationConflict) || remote.puts != 0 {
				t.Fatal("conflict mutated remote state", err, remote.puts)
			}
			got, err := remote.Get(t.Context(), key)
			if err != nil || string(got) != string(winner) {
				t.Fatal("winner changed", err)
			}
		})
	}
}

func TestSourceSetCorruptionAndRefOnlyMissingRemainUncommitted(t *testing.T) {
	for _, mode := range []string{"corrupt-existing", "missing-preserved", "metadata-readback"} {
		t.Run(mode, func(t *testing.T) {
			sources := sourceSetFixture()
			key := "sessions/codex/s/metadata.json"
			remote := &publicationFaultStore{MemoryStore: storagetest.NewMemoryStore(), metadataKey: key}
			if mode == "corrupt-existing" {
				if err := remote.MemoryStore.Put(t.Context(), sources[1].Key, []byte("wrong bytes")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-preserved" {
				sources[1].Bytes = nil
			}
			if mode == "metadata-readback" {
				remote.corruptMetadata = true
			}
			err := storage.PutSourceSetThenMetadata(t.Context(), remote, sources, key, []byte("next"), storage.MetadataPredecessor{Known: true}, storage.RetryPolicy{MaxAttempts: 1})
			want := storage.ErrChecksumMismatch
			if mode == "missing-preserved" {
				want = storage.ErrNotFound
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			if mode != "metadata-readback" {
				if _, err := remote.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
					t.Fatal("incomplete set became authoritative", err)
				}
			}
			if mode == "corrupt-existing" {
				got, err := remote.Get(t.Context(), sources[1].Key)
				if err != nil || string(got) != "wrong bytes" {
					t.Fatal("immutable mismatching key overwritten", err)
				}
			}
		})
	}
}

func TestSourceSetVerifiedPredecessorAndCancellation(t *testing.T) {
	remote := &publicationFaultStore{MemoryStore: storagetest.NewMemoryStore()}
	key := "sessions/codex/s/metadata.json"
	old := []byte("previous exact body")
	if err := remote.MemoryStore.Put(t.Context(), key, old); err != nil {
		t.Fatal(err)
	}
	prior := storage.MetadataPredecessor{Known: true, Exists: true, SHA256: storage.SHA256Hex(old)}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := storage.PutSourceSetThenMetadata(cancelled, remote, sourceSetFixture(), key, []byte("next"), prior, storage.RetryPolicy{}); !errors.Is(err, context.Canceled) || remote.puts != 0 {
		t.Fatal(err, remote.puts)
	}
	if err := storage.PutSourceSetThenMetadata(t.Context(), remote, sourceSetFixture(), key, []byte("next"), prior, storage.RetryPolicy{}); err != nil {
		t.Fatal(err)
	}
}

type publicationPositionChangeStore struct {
	*storagetest.MemoryStore
	metadataKey  string
	winner       []byte
	metadataPuts int
	changed      bool
}

func (s *publicationPositionChangeStore) Put(ctx context.Context, key string, body []byte) error {
	if key == s.metadataKey {
		s.metadataPuts++
	}
	if err := s.MemoryStore.Put(ctx, key, body); err != nil {
		return err
	}
	if key != s.metadataKey && !s.changed {
		s.changed = true
		return s.MemoryStore.Put(ctx, s.metadataKey, s.winner)
	}
	return nil
}

func TestSourceSetFreshPositionAfterSourceEffectsPreservesWinner(t *testing.T) {
	key := "sessions/codex/s/metadata.json"
	old := []byte("previous exact body")
	remote := &publicationPositionChangeStore{MemoryStore: storagetest.NewMemoryStore(), metadataKey: key, winner: []byte("foreign winner during source upload")}
	if err := remote.MemoryStore.Put(t.Context(), key, old); err != nil {
		t.Fatal(err)
	}
	prior := storage.MetadataPredecessor{Known: true, Exists: true, SHA256: storage.SHA256Hex(old)}
	err := storage.PutSourceSetThenMetadata(t.Context(), remote, sourceSetFixture(), key, []byte("next"), prior, storage.RetryPolicy{MaxAttempts: 1})
	if !errors.Is(err, storage.ErrPublicationConflict) || !remote.changed || remote.metadataPuts != 0 {
		t.Fatal("B reused stale A across source effects", err, remote.changed, remote.metadataPuts)
	}
	actual, err := remote.GetLimited(t.Context(), key, 32<<20)
	if err != nil || string(actual) != string(remote.winner) {
		t.Fatal("winner overwritten", err)
	}
}

func TestPublicationReadbackSingleConsumeBindsExactBodyAndValidator(t *testing.T) {
	remote := storagetest.NewMemoryStore()
	key, body := "sessions/codex/s/metadata.json", []byte("exact next body")
	receipt, err := storage.PutResolvedSourceSetThenMetadataReadback(t.Context(), remote, sourceSetFixture(), key, body, storage.MetadataPredecessor{Known: true}, storage.RetryPolicy{MaxAttempts: 1}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := receipt.Consume(key, []byte("different")); !errors.Is(err, storage.ErrPublicationConflict) {
		t.Fatal("wrong body consumed C", err)
	}
	copyReceipt := *receipt
	actual, validator, err := receipt.Consume(key, body)
	if err != nil || string(actual) != string(body) || validator == "" {
		t.Fatal("actual C response unavailable", err)
	}
	_, expectedValidator, err := remote.GetLimitedVersioned(t.Context(), key, int64(len(body)))
	if err != nil || validator != expectedValidator {
		t.Fatal("validator belongs to another response", err)
	}
	if _, _, err := receipt.Consume(key, body); !errors.Is(err, storage.ErrPublicationConflict) {
		t.Fatal("C consumed twice", err)
	}
	if _, _, err := copyReceipt.Consume(key, body); !errors.Is(err, storage.ErrPublicationConflict) {
		t.Fatal("copied C consumed twice", err)
	}
}

func TestSourceSetRefusesInvalidResolvedBytesBeforeUpload(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"wrong-size", "wrong-checksum", "resolver-error"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			remote := &publicationFaultStore{MemoryStore: storagetest.NewMemoryStore()}
			sources := sourceSetFixture()[:1]
			sources[0].Bytes = nil
			released := 0
			resolverErr := errors.New("synthetic resolver failure")
			resolve := func(context.Context, int) ([]byte, func(), error) {
				body := []byte("wrong")
				if mode == "wrong-checksum" {
					body = make([]byte, sources[0].Size)
				}
				var err error
				if mode == "resolver-error" {
					err = resolverErr
				}
				return body, func() { released++ }, err
			}
			err := storage.PutResolvedSourceSetThenMetadata(t.Context(), remote, sources, "metadata", []byte("next"), storage.MetadataPredecessor{Known: true}, storage.RetryPolicy{MaxAttempts: 1}, resolve, nil)
			want := storage.ErrChecksumMismatch
			if mode == "resolver-error" {
				want = resolverErr
			}
			if !errors.Is(err, want) || remote.puts != 0 || released != 1 {
				t.Fatalf("invalid replay payload: err=%v puts=%d releases=%d", err, remote.puts, released)
			}
		})
	}
}

type unboundedPublicationStore struct {
	storage.ObjectStore
	gets int
	puts int
}

func (s *unboundedPublicationStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.ObjectStore.Get(ctx, key)
}

func (s *unboundedPublicationStore) Put(ctx context.Context, key string, body []byte) error {
	s.puts++
	return s.ObjectStore.Put(ctx, key, body)
}

func TestSourceSetRefusesUnboundedMetadataReadBeforeSideEffects(t *testing.T) {
	t.Parallel()
	remote := &unboundedPublicationStore{ObjectStore: storagetest.NewMemoryStore()}
	err := storage.PutSourceSetThenMetadata(t.Context(), remote, sourceSetFixture(), "metadata", []byte("next"), storage.MetadataPredecessor{Known: true}, storage.RetryPolicy{MaxAttempts: 1})
	if !errors.Is(err, storage.ErrVersionedReadUnavailable) || remote.gets != 0 || remote.puts != 0 {
		t.Fatalf("unbounded publication: err=%v gets=%d puts=%d", err, remote.gets, remote.puts)
	}
}
