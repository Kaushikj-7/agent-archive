package state

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestNativeImportAtomicInspectionNeverLeaksWitness(t *testing.T) {
	s, _, _ := releasedNativeImportFixture(t)
	held, err := local.OpenRootedHome(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Error(err)
		}
	}()
	var escaped *Store
	err = local.RootedAtomicWriteWitnessed(t.Context(), held.Root, "pending/transient.json", func(w io.Writer) error { _, err := w.Write([]byte(`{}`)); return err }, make([]byte, 1024), func(witness *local.AtomicWriteWitness, rename func() error) error {
		// Seed the shared anonymous cache with the real temporary present.
		if s.CheckDurableReadRoots() == nil {
			t.Fatal("ordinary reader ignored live temporary")
		}
		scoped := *s
		scoped.atomicWitness = witness
		escaped = &scoped
		for range 2 {
			read, closeRead := scoped.WithReadBudget(t.Context(), nil)
			err := read.CheckDurableReadRoots()
			closeRead()
			if err != nil {
				t.Fatal("exact writer temporary refused", err)
			}
		}
		if s.CheckDurableReadRoots() == nil {
			t.Fatal("scoped result contaminated shared cache")
		}
		sibling := filepath.Join(s.Home(), "pending", ".pending-unrelated")
		if err := os.WriteFile(sibling, []byte("unrelated"), 0600); err != nil {
			t.Fatal(err)
		}
		if scoped.CheckDurableReadRoots() == nil {
			t.Fatal("unrelated anonymous sibling excused")
		}
		if err := os.Remove(sibling); err != nil {
			t.Fatal(err)
		}
		if err := scoped.CheckDurableReadRoots(); err != nil {
			t.Fatal(err)
		}
		return rename()
	})
	if err != nil {
		t.Fatal(err)
	}
	if escaped == nil || escaped.CheckDurableReadRoots() == nil {
		t.Fatal("revoked witness remained usable")
	}
	if err := os.Remove(filepath.Join(s.Home(), "pending", "transient.json")); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckDurableReadRoots(); err != nil {
		t.Fatal("ordinary reader retained scoped exception", err)
	}
}

func TestNativeImportAtomicCommitRechecksCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"valid", "owner", "receipt", "progress", "predecessor", "config", "sibling", "request"} {
		t.Run(mode, func(t *testing.T) {
			s, reg, next := releasedNativeImportFixture(t)
			check, err := s.nativeImportPublicationCommit(t.Context(), reg.ArchiveSessionID, nativeImportPendingTarget(next))
			if err != nil || check == nil {
				t.Fatal("initial exact state", err)
			}
			held, err := local.OpenRootedHome(s.Home())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := held.Close(); err != nil {
					t.Error(err)
				}
			}()
			var requestToken string
			var preservedPath string
			err = local.RootedAtomicWriteWitnessed(t.Context(), held.Root, "pending/selected.json", func(w io.Writer) error { _, e := w.Write([]byte(`{"selected":true}`)); return e }, make([]byte, 1024), func(witness *local.AtomicWriteWitness, rename func() error) error {
				switch mode {
				case "owner", "receipt":
					if mode == "owner" {
						reg.RegisteredAt = reg.RegisteredAt.Add(time.Second)
					} else {
						reg.NativeImport.Membership.ReceiptID = "dddddddddddddddddddddddddddddddd"
					}
					preservedPath = s.registrationPath(reg.ArchiveSessionID)
					if err := local.Write(preservedPath, reg); err != nil {
						t.Fatal(err)
					}
				case "progress":
					preservedPath = filepath.Join(s.Home(), stagedControl(reg.ArchiveSessionID, stagedReleaseProgressName))
					if err := os.WriteFile(preservedPath, []byte(`{"future":true}`), 0600); err != nil {
						t.Fatal(err)
					}
				case "predecessor":
					preservedPath = s.publishedPath(reg.ArchiveSessionID)
					raw, err := os.ReadFile(preservedPath)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(preservedPath, append(raw, 'x'), 0600); err != nil {
						t.Fatal(err)
					}
				case "config":
					cfg, _, err := config.Load(s.Home())
					if err != nil {
						t.Fatal(err)
					}
					previous := cfg
					cfg.Paused = true
					cfg, err = config.ReconcileMuseCapture(cfg, previous, reg.AdmittedAt.Add(time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					if err := config.Save(s.Home(), cfg); err != nil {
						t.Fatal(err)
					}
					preservedPath = filepath.Join(s.Home(), "config.json")
				case "sibling":
					preservedPath = filepath.Join(s.Home(), "pending", ".pending-unrelated")
					if err := os.WriteFile(preservedPath, []byte("unrelated"), 0600); err != nil {
						t.Fatal(err)
					}
				case "request":
					if err := s.SaveRequest(reg.ArchiveSessionID, "independent", reg.AdmittedAt.Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
					req, found, err := s.LoadRequest(reg.ArchiveSessionID)
					if err != nil || !found {
						t.Fatal(err)
					}
					requestToken = req.Token
				}
				var before []byte
				if preservedPath != "" {
					var e error
					before, e = os.ReadFile(preservedPath)
					if e != nil {
						t.Fatal(e)
					}
				}
				err := check(witness, rename)
				if preservedPath != "" {
					after, e := os.ReadFile(preservedPath)
					if e != nil || !bytes.Equal(before, after) {
						t.Fatal("refusal changed racing evidence", e)
					}
				}
				return err
			})
			if mode == "valid" || mode == "request" {
				if err != nil {
					t.Fatal("unchanged authority refused", err)
				}
				if mode == "request" {
					req, found, e := s.LoadRequest(reg.ArchiveSessionID)
					if e != nil || !found || req.Token != requestToken {
						t.Fatal("later independent request lost", e)
					}
				}
				return
			}
			if err == nil {
				t.Fatal("changed authority committed")
			}
			if _, e := os.Stat(filepath.Join(s.Home(), "pending", "selected.json")); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("failed CAS selected output", e)
			}
		})
	}
}
