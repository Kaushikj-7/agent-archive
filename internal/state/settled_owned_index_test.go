package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

// The actual public stage factory must charge the whole physical index view.
func TestSettledPublicSaveOwnsPhysicalIndexExtent(t *testing.T) {
	for _, pressure := range []bool{false, true} {
		t.Run(map[bool]string{false: "adequate", true: "pressure"}[pressure], func(t *testing.T) {
			s, reg, request, proof, budget := settledStateCaptureFixture(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: reg.NativeSessionID}
			path := qualifiedSessionIndexPath(s.home, key)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw = append(bytes.Repeat([]byte(" "), 1<<20), raw...)
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before := settledProtocolInventory(t, s.home)
			blocker := budget.Reservation()
			held := int64(0)
			if pressure {
				held = budget.Available() - (512 << 10)
				if held < 0 || !blocker.Reserve(held) {
					t.Fatal("pressure setup")
				}
			}
			scoped, closeScoped := s.WithReadBudget(t.Context(), budget)
			_, err = scoped.SaveSettledCursorImportStage(t.Context(), reg, request, proof)
			closeScoped()
			if pressure {
				if !errors.Is(err, agentapi.ErrReadBudget) {
					t.Fatal("physical index escaped captured pressure", err)
				}
				assertSettledIndexInventory(t, before, settledProtocolInventory(t, s.home))
			} else if err != nil {
				t.Fatal("adequate public physical save refused", err)
			}
			blocker.Release(held)
			if err = proof.Close(); err != nil {
				t.Fatal(err)
			}
			request.Close()
			if budget.Available() != 128<<20 {
				t.Fatal("physical index owner leaked")
			}
		})
	}
}

// A genuine SQL proof lends its original owner to actual packed lookup. This
// is a resource-route control, not a Cursor packed first-save authority claim.
func TestSettledProofLoanOwnsPackedIndexExtent(t *testing.T) {
	for _, pressure := range []bool{false, true} {
		t.Run(map[bool]string{false: "adequate", true: "pressure"}[pressure], func(t *testing.T) {
			_, reg, request, proof, budget := settledStateCaptureFixture(t)
			packed, key, _ := packedOwnerFixture(t)
			expandSettledPackedRow(t, packedIndexPath(packed.home, packedIndexHash(key)[:2]))
			before := settledProtocolInventory(t, packed.home)
			blocker := budget.Reservation()
			held := int64(0)
			if pressure {
				held = budget.Available() - (512 << 10)
				if held < 0 || !blocker.Reserve(held) {
					t.Fatal("pressure setup")
				}
			}
			err := proof.WithVerifiedCapture(t.Context(), reg, request, budget, func(ctx context.Context, _ archive.SourceBundle, _ nativefacts.SettledImportFacts) error {
				lease, err := request.Borrow()
				if err != nil {
					return err
				}
				defer lease.Close()
				scoped, closeScoped := packed.WithReadBudget(ctx, budget)
				defer closeScoped()
				scoped.resourceReservation = func() stateReservation { return lease.Reservation() }
				entry, found, err := scoped.readQualifiedIndex(key)
				if err != nil {
					return err
				}
				if !found || entry.ArchiveSessionID != "packed-owner" {
					return errors.New("actual packed owner missing")
				}
				return nil
			})
			if pressure {
				if !errors.Is(err, agentapi.ErrReadBudget) {
					t.Fatal("packed row escaped captured pressure", err)
				}
			} else if err != nil {
				t.Fatal("adequate packed lookup refused", err)
			}
			assertSettledIndexInventory(t, before, settledProtocolInventory(t, packed.home))
			blocker.Release(held)
			if err = proof.Close(); err != nil {
				t.Fatal(err)
			}
			request.Close()
			if budget.Available() != 128<<20 {
				t.Fatal("packed owner leaked")
			}
		})
	}
}

func TestSettledProofLoanOwnsPackedMissAnchorExtent(t *testing.T) {
	for _, pressure := range []bool{false, true} {
		t.Run(map[bool]string{false: "adequate", true: "pressure"}[pressure], func(t *testing.T) {
			_, reg, request, proof, budget := settledStateCaptureFixture(t)
			packed, owner, marker := packedOwnerFixture(t)
			key := packedFreshKeyInShard(packedIndexHash(owner)[:2], "owned-miss")
			anchor, err := json.Marshal(indexEntry(owner, "packed-owner"))
			if err != nil {
				t.Fatal(err)
			}
			anchor = append(bytes.Repeat([]byte(" "), 1<<20), anchor...)
			if err = os.WriteFile(qualifiedSessionIndexPath(packed.home, owner), anchor, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(packed.home, "sessions-v1", packedOverlaySentinel), []byte(marker.PackedEpoch+":overrides:"+packedIndexHash(owner)), 0600); err != nil {
				t.Fatal(err)
			}
			before := settledProtocolInventory(t, packed.home)
			blocker := budget.Reservation()
			held := int64(0)
			if pressure {
				held = budget.Available() - (512 << 10)
				if held < 0 || !blocker.Reserve(held) {
					t.Fatal("pressure setup")
				}
			}
			err = proof.WithVerifiedCapture(t.Context(), reg, request, budget, func(ctx context.Context, _ archive.SourceBundle, _ nativefacts.SettledImportFacts) error {
				lease, err := request.Borrow()
				if err != nil {
					return err
				}
				defer lease.Close()
				scoped, closeScoped := packed.WithReadBudget(ctx, budget)
				defer closeScoped()
				scoped.resourceReservation = func() stateReservation { return lease.Reservation() }
				_, found, err := scoped.readQualifiedIndex(key)
				if err != nil {
					return err
				}
				if found {
					return errors.New("actual packed miss unexpectedly found an owner")
				}
				return nil
			})
			if pressure {
				if !errors.Is(err, agentapi.ErrReadBudget) {
					t.Fatal("packed miss anchor escaped captured pressure", err)
				}
			} else if err != nil {
				t.Fatal("adequate packed lookup refused", err)
			}
			assertSettledIndexInventory(t, before, settledProtocolInventory(t, packed.home))
			blocker.Release(held)
			if err = proof.Close(); err != nil {
				t.Fatal(err)
			}
			request.Close()
			if budget.Available() != 128<<20 {
				t.Fatal("packed owner leaked")
			}
		})
	}
}

func assertSettledIndexInventory(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatal("index refusal changed inventory")
	}
	for path, raw := range before {
		if !bytes.Equal(raw, after[path]) {
			t.Fatalf("index refusal changed %s", path)
		}
	}
}

// Preserve the actual packed layout/checksums while widening valid JSON wire.
func expandSettledPackedRow(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := int(binary.BigEndian.Uint32(raw[8:12]))
	var header packedIndexHeader
	if err = json.Unmarshal(raw[12:12+n], &header); err != nil {
		t.Fatal(err)
	}
	if header.Count != 1 {
		t.Fatal("expected one actual packed owner")
	}
	table := append([]byte(nil), raw[12+n:12+n+packedRecordIndexBytes]...)
	row := append(bytes.Repeat([]byte(" "), 1<<20), raw[12+n+packedRecordIndexBytes:]...)
	binary.BigEndian.PutUint32(table[40:44], uint32(len(row)))
	sum := sha256.Sum256(row)
	copy(table[44:76], sum[:])
	sum = sha256.Sum256(table)
	header.IndexChecksum = hex.EncodeToString(sum[:])
	header.Bytes = len(row)
	header.Checksum = header.checksum()
	meta, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]byte, 12)
	copy(result, packedIndexMagic)
	binary.BigEndian.PutUint32(result[8:12], uint32(len(meta)))
	result = append(result, meta...)
	result = append(result, table...)
	result = append(result, row...)
	if len(result) > packedSessionIndexMaxBytes {
		t.Fatal("fixture exceeded existing packed maximum")
	}
	if err = os.WriteFile(path, result, 0600); err != nil {
		t.Fatal(err)
	}
}
