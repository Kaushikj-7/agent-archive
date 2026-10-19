package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestInactiveDeletionPreservesLegacyHomeRegistration(t *testing.T) {
	t.Parallel()
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-config", true: "legacy-config"}[configured], func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			if configured {
				if err := os.WriteFile(filepath.Join(s.home, "config.json"), []byte(`{"schema_version":1,"ignored":"`+strings.Repeat("a", 1<<20)+`"}`), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(filepath.Join(s.home, "config.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(s.home, 0755); err != nil {
				t.Fatal(err)
			}
			scoped, closeScope := s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(128<<10))
			t.Cleanup(closeScope)
			if err := scoped.SaveRegistration(registration(t)); err != nil {
				t.Fatal("inactive deletion imposed new private-home requirement", err)
			}
		})
	}
}

func TestDeletionGuardRetainsProtectedHomeRefusals(t *testing.T) {
	t.Parallel()
	for _, namespace := range []bool{false, true} {
		t.Run(map[bool]string{false: "sticky-config", true: "namespace"}[namespace], func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			if namespace {
				if err := os.Mkdir(filepath.Join(s.home, deletionDirectory), 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := config.WithDeletionProtection(s.home, func(config.DurableStorageGuard) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(s.home, 0755); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveRegistration(registration(t)); err == nil {
				t.Fatal("protected deletion accepted unsafe home")
			}
		})
	}
}

func TestDeletionCapabilityProjectionRefusesMalformedFutureAndPressure(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"schema_version":19}`, `{"schema_version":1,"schema_version":1}`, `{"Schema_Version":1}`, `{"schema_version":1,"ignored":fals}`, `{"schema_version":1,"deletion_protection":true}`} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			if err := os.WriteFile(filepath.Join(s.home, "config.json"), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if inactive, err := s.deletionInactive(); err == nil || inactive {
				t.Fatal("unreadable capability granted legacy bypass", inactive, err)
			}
		})
	}
	s := newTestStore(t)
	if err := os.WriteFile(filepath.Join(s.home, "config.json"), []byte(`{"schema_version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	scoped, closeScope := s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(1))
	t.Cleanup(closeScope)
	if inactive, err := scoped.deletionInactive(); inactive || !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("projection bypassed captured ledger", inactive, err)
	}
}

func TestInactiveDeletionAllowsFreshRegistrationHome(t *testing.T) {
	t.Parallel()
	s := OpenReadOnly(filepath.Join(t.TempDir(), "fresh"))
	if inactive, err := s.deletionInactive(); err != nil || !inactive {
		t.Fatal("fresh legacy home preflight refused", inactive, err)
	}
	s, err := Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRegistration(registration(t)); err != nil {
		t.Fatal("fresh legacy registration refused", err)
	}
}

func TestProtectedDeletionRefusesSymlinkHome(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := config.WithDeletionProtection(s.home, func(config.DurableStorageGuard) error { return nil }); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(s.home, alias); err != nil {
		t.Fatal(err)
	}
	s.home = alias
	if err := s.SaveRegistration(registration(t)); err == nil {
		t.Fatal("protected deletion accepted symlink home")
	}
}
