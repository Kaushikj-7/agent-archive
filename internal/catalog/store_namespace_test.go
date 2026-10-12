package catalog

import (
	"context"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

type namespaceWriteStore struct {
	*qualifiedStore
	writes int
}

func (s *namespaceWriteStore) Put(ctx context.Context, key string, raw []byte) error {
	s.writes++
	return s.qualifiedStore.Put(ctx, key, raw)
}

func (s *namespaceWriteStore) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	s.writes++
	return s.qualifiedStore.PutConditional(ctx, key, raw, condition)
}

func TestPublicPutRefusesCatalogNamespaceBeforeProviderWrite(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsealed", true: "sealed"}[sealed], func(t *testing.T) {
			_, raw := fixture(t)
			provider := &namespaceWriteStore{qualifiedStore: raw}
			store, err := Wrap(provider)
			if err != nil {
				t.Fatal(err)
			}
			if sealed {
				if _, err = store.Writer.Coordinator().Seal(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			provider.writes = 0
			for _, key := range []string{HeadKey, CoordinatorKey, MigrationKey, "catalog-v4/nodes/immutable.json", "catalog-v4/unknown"} {
				if err = store.Put(t.Context(), key, []byte("overwrite")); !errors.Is(err, ErrAdmissionClosed) {
					t.Fatalf("namespace write was accepted: %v", err)
				}
			}
			if provider.writes != 0 {
				t.Fatal("refused namespace write reached provider")
			}
		})
	}
}
