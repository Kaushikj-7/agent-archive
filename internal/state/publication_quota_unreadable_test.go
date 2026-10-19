package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
)

func withUnreadablePublicationPeer(t *testing.T, check func(*Store, *durableQuota, string, os.FileInfo)) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	s := newTestStore(t)
	p := publicationFixture(t, publicationThread, time.Now())
	raw, err := json.Marshal(publishedState{Bundle: p.Bundle, MetadataBytes: p.MetadataBytes})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("published", p.Bundle.ArchiveSessionID+".json")
	if err := os.MkdirAll(filepath.Join(s.home, "published"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, path), raw, 0); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(s.home, path))
	if err != nil {
		t.Fatal(err)
	}
	err = config.WithPublicationComposition(s.home, func(c config.PublicationCompositionGuard) error {
		g, err := c.Storage(s.home)
		if err != nil {
			return err
		}
		q, err := s.openDurableQuota(g)
		if err != nil {
			return err
		}
		defer func() {
			if err := q.Close(); err != nil {
				t.Error(err)
			}
		}()
		check(s, q, path, info)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnreadablePublicationPeerPaysFullQuotaWithoutReplacementCredit(t *testing.T) {
	t.Parallel()
	withUnreadablePublicationPeer(t, func(s *Store, q *durableQuota, path string, info os.FileInfo) {
		usage, err := q.usageContext(t.Context())
		if err != nil || usage.physical != info.Size() || usage.charged != 2*info.Size() {
			t.Fatal("unreadable peer was not fully charged", usage, err)
		}
		if _, err := q.publishedProtocol(t.Context(), path, info); !errors.Is(err, os.ErrPermission) {
			t.Fatal("accounting fabricated readable classification", err)
		}
		if _, err := q.publishedAdditional(t.Context(), path, info, 1); !errors.Is(err, os.ErrPermission) {
			t.Fatal("unreadable peer gained replacement credit", err)
		}
		if err := os.Chmod(filepath.Join(s.home, path), 0600); err != nil {
			t.Fatal(err)
		}
		for _, raw := range [][]byte{[]byte(`{"publication_version":999}`), []byte(`{"broken":`)} {
			if err := os.WriteFile(filepath.Join(s.home, path), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := q.usageContext(t.Context()); !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatal("unknown or malformed readable protocol gained accounting admission", err)
			}
		}
	})
}

func TestUnreadablePublicationPeerCannotExceedQuotaOrReadBudget(t *testing.T) {
	t.Parallel()
	withUnreadablePublicationPeer(t, func(s *Store, q *durableQuota, path string, _ os.FileInfo) {
		scoped, closeScope := s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(1))
		q.store = scoped
		if _, err := q.usageContext(t.Context()); !errors.Is(err, agentapi.ErrReadBudget) {
			t.Fatal("unreadable peer bypassed read budget", err)
		}
		closeScope()
		q.store = s
		absolute := filepath.Join(s.home, path)
		if err := os.Chmod(absolute, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(absolute, durableStorageQuota/2+1); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(absolute, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := q.usageContext(t.Context()); !errors.Is(err, ErrDurableStorageCapacity) {
			t.Fatal("unreadable peer bypassed full protected quota charge", err)
		}
	})
}

func TestUnreadablePublicationChargeRefusesJoinedErrorsAndChangedIdentity(t *testing.T) {
	t.Parallel()
	withUnreadablePublicationPeer(t, func(s *Store, q *durableQuota, path string, info os.FileInfo) {
		denied := &os.PathError{Op: "openat", Path: path, Err: syscall.EACCES}
		closeFailure := errors.New("checked descriptor close failed")
		for _, err := range []error{errors.Join(denied, context.Canceled), errors.Join(denied, closeFailure), errors.Join(denied, ErrDurableStorageRecovery), &os.PathError{Op: "read", Path: path, Err: syscall.EACCES}, &os.PathError{Op: "openat", Path: path + "-foreign", Err: syscall.EACCES}} {
			if mode, got := q.unreadablePublishedCharge(t.Context(), path, info, err); mode != 0 || !errors.Is(got, err) {
				t.Fatal("non-open or joined refusal was converted to accounting authority", err, got)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := q.unreadablePublishedCharge(ctx, path, info, denied); !errors.Is(err, context.Canceled) {
			t.Fatal("original caller cancellation lost", err)
		}
		absolute := filepath.Join(s.home, path)
		if err := os.Chmod(absolute, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := q.unreadablePublishedCharge(t.Context(), path, info, denied); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("changed file mode gained permission accounting", err)
		}
		if err := os.Chmod(absolute, 0); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(absolute, filepath.Join(s.home, "saved-quota-peer")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte("replacement remains evidence"), 0); err != nil {
			t.Fatal(err)
		}
		if _, err := q.unreadablePublishedCharge(t.Context(), path, info, denied); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("replacement identity gained permission accounting", err)
		}
		if err := os.Remove(absolute); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "saved-quota-peer"), absolute); err != nil {
			t.Fatal(err)
		}
		if _, err := q.usageContext(t.Context()); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("symlink gained accounting admission", err)
		}
	})
}
