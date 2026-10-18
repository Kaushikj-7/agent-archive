package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestNativeImportReservationMalformedRawPreservesEveryBoundary(t *testing.T) {
	t.Parallel()
	for _, scheme := range []string{nativeReservationPrefix, nativeImportReservationPrefix, "muse-import-reservation-v2:"} {
		for _, damage := range []string{"after", "before-escaped", "fully-escaped"} {
			t.Run(scheme+damage, func(t *testing.T) {
				t.Parallel()
				s := newTestStore(t)
				key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: "protected-root"}
				entry := indexEntry(key, strings.Repeat("a", 32))
				entry.Reservation = strings.Repeat("b", 32)
				entry.NativeReservation = scheme + strings.Repeat("c", 64)
				raw, err := json.Marshal(entry)
				if err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "after":
					raw = append(raw[:len(raw)-1], []byte(`,"damaged":`)...)
				case "before-escaped":
					raw = append([]byte(`{"ignored":"unterminated,`), bytes.Replace(raw[1:], []byte(`"native_reservation"`), []byte(`"native_\u0072eservation"`), 1)...)
				case "fully-escaped":
					raw = append([]byte(`{"ignored":"unterminated,`), bytes.Replace(raw[1:], []byte(`"native_reservation"`), []byte(`"\u006e\u0061\u0074\u0069\u0076\u0065\u005f\u0072\u0065\u0073\u0065\u0072\u0076\u0061\u0074\u0069\u006f\u006e"`), 1)...)
				}
				path := qualifiedSessionIndexPath(s.home, key)
				if err := os.MkdirAll(filepath.Join(s.home, "sessions-v1"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				for _, operation := range []func() error{
					func() error { _, _, err := s.readQualifiedIndex(key); return err },
					func() error { return s.finishSessionIndex(key, entry.ArchiveSessionID) },
					func() error { return s.recoverRegistrationOwners(key, []string{entry.ArchiveSessionID}) },
					func() error { return s.RequestSessionIndexRecovery(key) },
					func() error {
						return s.packedShardOwners(t.Context(), map[agentmeta.SessionKey][]string{key: {entry.ArchiveSessionID}}, map[string]qualifiedSessionIndexEntry{}, time.Time{})
					},
					func() error { return s.removeSessionIndex(key, entry.ArchiveSessionID) },
					func() error {
						_, err := s.PrepareSessionDeletion(archive.SessionRegistration{ArchiveSessionID: entry.ArchiveSessionID, NativeSessionID: key.NativeID, Harness: archive.Harness{Name: "muse"}}, RemovalReasonRetention, nil, time.Now(), "")
						return err
					},
				} {
					if err := operation(); !errors.Is(err, errUnsupportedNativeReservation) {
						t.Fatal("private raw marker not refused", err)
					}
					after, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(raw, after) {
						t.Fatal("refusal changed protected index", err)
					}
				}
			})
		}
	}
}

func TestNativeImportReservationPossibleKeyKeepsOrdinaryValues(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"version":1,"agent":"muse","native_id":"ordinary","archive_session_id":"` + strings.Repeat("a", 32) + `","note":"native_reservation"}`,
		`{"note":"native_\u0072eservation"}`,
	} {
		var entry qualifiedSessionIndexEntry
		if err := decodeNativeReservationIndex([]byte(raw), &entry); err != nil {
			t.Fatal("ordinary string classified as authority", err)
		}
	}
	var entry qualifiedSessionIndexEntry
	if err := decodeNativeReservationIndex([]byte(`{"version":`), &entry); !IsUndecodable(err) || errors.Is(err, errUnsupportedNativeReservation) {
		t.Fatal("ordinary legacy error changed", err)
	}
	raw := []byte(`{"ignored":"unterminated,"native_\u0072eservation":{}}` + strings.Repeat(" ", 64<<10))
	if err := decodeNativeReservationIndex(raw, &entry); !errors.Is(err, errUnsupportedNativeReservation) {
		t.Fatal("escaped selector hidden by malformed prefix/whitespace", err)
	}
}

func TestNativeImportReservationGenericClearingRefusesEvenMatchingOwner(t *testing.T) {
	t.Parallel()
	f := newNativeImportPrepareFixture(t)
	p, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	}()
	injected := errors.New("owner before exact index completion")
	f.native.store.onIndexStep = func(step string) error {
		if step == "import-registration" {
			return injected
		}
		return nil
	}
	if _, err := f.native.store.CommitNativeImport(t.Context(), p); !errors.Is(err, injected) {
		t.Fatal("post-owner checkpoint not reached", err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: p.reg.NativeSessionID}
	path := qualifiedSessionIndexPath(f.native.store.home, key)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.native.store.finishSessionIndex(key, p.reg.ArchiveSessionID); !errors.Is(err, errUnsupportedNativeReservation) {
		t.Fatal("generic clearing accepted private import marker", err)
	}
	if err := f.native.store.RequestSessionIndexRecovery(key); !errors.Is(err, errUnsupportedNativeReservation) {
		t.Fatal("generic recovery replaced marker", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("generic path changed pending marker", err)
	}
	entry, found, err := f.native.store.readQualifiedIndex(key)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if err := f.native.store.nativeImportReservationOwner(entry); err != nil {
		t.Fatal("matching actual owner/stage/control did not qualify", err)
	}
	future := entry
	future.NativeReservation = "muse-import-reservation-v2:" + strings.Repeat("0", 64)
	if err := local.Write(path, future); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.native.store.readQualifiedIndex(key); !errors.Is(err, errUnsupportedNativeReservation) {
		t.Fatal("future marker accepted", err)
	}
}
