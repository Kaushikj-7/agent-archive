package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type migrationReadFault struct {
	*storagetest.MemoryStore
	reads  int
	failAt int
	before func()
}

func (s *migrationReadFault) GetVersioned(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("unbounded migration metadata read")
}

func (s *migrationReadFault) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	s.reads++
	if s.before != nil {
		fn := s.before
		s.before = nil
		fn()
	}
	if s.failAt != 0 && s.reads == s.failAt {
		s.failAt = 0
		return nil, "", errors.New("interrupted source page")
	}
	if limit != 32<<20 {
		return nil, "", errors.New("wrong migration metadata bound")
	}
	return s.MemoryStore.GetLimitedVersioned(ctx, key, limit)
}

type migrationWriteFault struct {
	*qualifiedStore
	writes         map[string]int
	failCheckpoint bool
}

func (s *migrationWriteFault) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	s.writes[key]++
	if key == MigrationKey && s.failCheckpoint {
		s.failCheckpoint = false
		return "", errors.New("interrupted checkpoint")
	}
	return s.qualifiedStore.PutConditional(ctx, key, raw, condition)
}

func TestMigrationSameInstanceRetryDoesNotInflateCheckpoint(t *testing.T) {
	for _, fault := range []string{"source", "checkpoint"} {
		t.Run(fault, func(t *testing.T) {
			source, target, src, dst, authority := migrationFixture(t, 80)
			read := &migrationReadFault{MemoryStore: source}
			write := &migrationWriteFault{qualifiedStore: target, writes: map[string]int{}}
			m, err := OpenMigration(t.Context(), read, write, src, dst, authority)
			if err != nil {
				t.Fatal(err)
			}
			prior := m.State
			if fault == "source" {
				read.failAt = 2
			} else {
				write.failCheckpoint = true
			}
			if _, err = m.Step(t.Context()); err == nil {
				t.Fatal("fault did not interrupt page")
			}
			if m.State != prior {
				t.Fatal("failed page changed local checkpoint", m.State, prior)
			}
			for {
				done, err := m.Step(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if done {
					break
				}
			}
			if m.State.Copied != 80 {
				t.Fatal("receipt retry inflated count", m.State.Copied)
			}
			if err = m.Activate(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = m.FinishActivation(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrationLostSealRefusesSourceAndCheckpointWrites(t *testing.T) {
	source, target, src, dst, authority := migrationFixture(t, 2)
	read := &migrationReadFault{MemoryStore: source}
	write := &migrationWriteFault{qualifiedStore: target, writes: map[string]int{}}
	m, err := OpenMigration(t.Context(), read, write, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	write.writes = map[string]int{}
	read.before = func() {
		if err := m.writer.Coordinator().Release(t.Context(), m.State.Owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = m.Step(t.Context()); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("lost migration seal accepted", err)
	}
	if err = m.save(t.Context()); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("checkpoint saved without seal", err)
	}
	for key, count := range write.writes {
		if key != CoordinatorKey && count != 0 {
			t.Fatal("write after lost seal", key, count)
		}
	}
}

func TestMigrationCheckpointStrictDescriptors(t *testing.T) {
	source, target, src, dst, authority := migrationFixture(t, 2)
	m, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m.State)
	if err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range []string{strings.TrimSuffix(string(raw), "}") + `,"unknown":1}`, string(raw) + ` {}`, strings.Replace(string(raw), `"phase":"copying"`, `"phase":"complete"`, 1), strings.Replace(string(raw), `"cursor":""`, `"cursor":"`+strings.Repeat("x", 4097)+`"`, 1)} {
		if _, err = target.PutConditional(t.Context(), MigrationKey, []byte(corrupt), storage.PutCondition{MatchETag: m.checkpointETag}); err != nil {
			t.Fatal(err)
		}
		if _, err = OpenMigration(t.Context(), source, target, src, dst, authority); err == nil {
			t.Fatal("corrupt checkpoint resumed")
		}
		if _, err = MigrationCheckpoint(t.Context(), target); err == nil {
			t.Fatal("corrupt checkpoint exposed configuration")
		}
		_, etag, err := target.GetVersioned(t.Context(), MigrationKey)
		if err != nil {
			t.Fatal(err)
		}
		m.checkpointETag, err = target.PutConditional(t.Context(), MigrationKey, raw, storage.PutCondition{MatchETag: etag})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestEmptyMigrationAndGCBootstrapRequireExactAuthority(t *testing.T) {
	source, target, src, dst, authority := migrationFixture(t, 0)
	m, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	if done, err := m.Step(t.Context()); err != nil || !done {
		t.Fatal(done, err)
	}
	if err = m.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = m.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}

	raw := &migrationWriteFault{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}, writes: map[string]int{}}
	w, err := New(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = w.gcHead(t.Context(), nil); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("nil bootstrap authority", err)
	}
	if raw.writes[HeadKey] != 0 {
		t.Fatal("nil authority created head")
	}
	c := w.Coordinator()
	owner, err := c.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := c.HeldBarrier(owner).Hold(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	authorize, err := c.bootstrapAuthority(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := New(&qualifiedStore{storagetest.NewMemoryStore()})
	if _, _, err = foreign.gcHead(t.Context(), authorize); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("foreign bootstrap authority", err)
	}
	release()
	if _, _, err = w.gcHead(t.Context(), authorize); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("changed bootstrap authority", err)
	}
	if raw.writes[HeadKey] != 0 {
		t.Fatal("changed authority created head")
	}
	state, _, err := c.read(t.Context())
	if err != nil || state.Seal != "" || state.Hold != "" {
		t.Fatal("owned release did not clear exact durable hold/seal", state, err)
	}
	if err = w.Collect(t.Context(), heldBarrier{}); err != nil {
		t.Fatal("empty GC", err)
	}
	if raw.writes[HeadKey] == 0 {
		t.Fatal("empty GC never created qualified head")
	}
}

func TestPublicationBoundsAndCoordinatorMutationRefuseBeforeWrite(t *testing.T) {
	raw := &migrationWriteFault{qualifiedStore: &qualifiedStore{storagetest.NewMemoryStore()}, writes: map[string]int{}}
	store, err := Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	mutation := mutation(t, store.Writer, "bounded-pending")
	metadata, err := json.Marshal(mutation.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	raw.writes = map[string]int{}
	for _, id := range []string{"", strings.Repeat("x", 456)} {
		if _, err = store.BeginPublication(t.Context(), id, metadata); err == nil {
			t.Fatal("invalid identifier admitted")
		}
	}
	if _, err = store.BeginPublication(t.Context(), "oversized", make([]byte, 32<<20+1)); !errors.Is(err, storage.ErrObjectTooLarge) {
		t.Fatal(err)
	}
	if err = store.Writer.Coordinator().change(t.Context(), func(state *admissions) error { state.Mode = invalidAdmissionMode; return nil }); err == nil {
		t.Fatal("invalid custom descriptor persisted")
	}
	if raw.writes[CoordinatorKey] != 0 {
		t.Fatal("invalid descriptor wrote coordinator")
	}
}

const invalidAdmissionMode admissionMode = "invalid"
