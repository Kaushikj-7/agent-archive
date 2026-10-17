package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"path/filepath"
	"testing"
)

type accountingCancelContext struct {
	context.Context
	remaining int
}

func (c *accountingCancelContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestPublicationAccountingAdaptiveHashParityFallbackAndUnfundedRefusal(t *testing.T) {
	s := newTestStore(t)
	raw := bytes.Repeat([]byte("exact synthetic hash bytes"), 1<<17)
	path := "synthetic-hash-only"
	if err := os.WriteFile(filepath.Join(s.home, path), raw, 0600); err != nil {
		t.Fatal(err)
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := home.Close(); err != nil {
			t.Error(err)
		}
	}()
	info, err := home.Root.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, capacity := range []int64{512 << 10, 36 << 10, 1024} {
		budget := agentapi.NewNativeReadBudget(capacity)
		scoped, end := s.WithReadBudget(t.Context(), budget)
		closes := 0
		scoped.publicationFileClose = func(file *os.File) error { closes++; return file.Close() }
		sum, err := scoped.accountingHash(home, path, info)
		used, peak := budget.Charged()
		if capacity == 1024 {
			if !errors.Is(err, agentapi.ErrReadBudget) || closes != 0 || sum != ([32]byte{}) {
				t.Fatal("unfunded hash opened file/minted bytes", sum, err, closes)
			}
		} else {
			if err != nil || sum != sha256.Sum256(raw) || closes != 1 {
				t.Fatal("actual hash differs", sum, err, closes)
			}
			if capacity == 512<<10 && (peak < 128<<10 || peak > 132<<10) {
				t.Fatal("large charged batch", peak)
			}
			if capacity == 36<<10 && (peak < 32<<10 || peak > capacity) {
				t.Fatal("fallback charged batch", peak)
			}
		}
		if used != 0 {
			t.Fatal("hash loan leaked", used)
		}
		end()
		end()
	}
}

func TestPublicationAccountingAdaptiveHashCancelCloseAndEmpty(t *testing.T) {
	s := newTestStore(t)
	path := "synthetic-hash-only"
	raw := bytes.Repeat([]byte("hash"), 1<<20)
	if err := os.WriteFile(filepath.Join(s.home, path), raw, 0600); err != nil {
		t.Fatal(err)
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := home.Close(); err != nil {
			t.Error(err)
		}
	}()
	info, err := home.Root.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(512 << 10)
	ctx := &accountingCancelContext{Context: t.Context(), remaining: 6}
	scoped, end := s.WithReadBudget(ctx, budget)
	sum, err := scoped.accountingHash(home, path, info)
	if !errors.Is(err, context.Canceled) || sum != ([32]byte{}) {
		t.Fatal("canceled hash authority", sum, err)
	}
	end()
	scoped, end = s.WithReadBudget(t.Context(), budget)
	closeFailure := errors.New("synthetic actual hash descriptor Close failure")
	scoped.publicationFileClose = func(file *os.File) error { return errors.Join(file.Close(), closeFailure) }
	sum, err = scoped.accountingHash(home, path, info)
	if !errors.Is(err, closeFailure) || sum != ([32]byte{}) {
		t.Fatal("close failure returned hash", sum, err)
	}
	end()
	if err = os.WriteFile(filepath.Join(s.home, path), nil, 0600); err != nil {
		t.Fatal(err)
	}
	info, err = home.Root.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	scoped, end = s.WithReadBudget(t.Context(), budget)
	sum, err = scoped.accountingHash(home, path, info)
	if err != nil || sum != sha256.Sum256(nil) {
		t.Fatal("empty EOF hash", sum, err)
	}
	end()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("cancel/close/empty ownership leak", used)
	}
}
