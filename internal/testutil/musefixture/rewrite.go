// Package musefixture contains the existing synthetic permission-frame encoder.
// Only test files import it; production Muse decoding and authority are separate.
package musefixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

const syntheticID = "00000000-0000-7000-8000-000000000001"

type frameKind string

const permissionFrame frameKind = "session_permission_transaction"

// Rewrite derives synthetic fixture identities using the original CLI encoder.
// Its output grants no ownership or source authority; production validates it.
func Rewrite(tb testing.TB, raw []byte, id, workspace string) []byte {
	tb.Helper()
	workspaceJSON, _ := json.Marshal(workspace)
	raw = bytes.ReplaceAll(raw, []byte(syntheticID), []byte(id))
	raw = bytes.ReplaceAll(raw, []byte(`"/work/synthetic-muse"`), workspaceJSON)
	lines := bytes.Split(raw, []byte("\n"))
	var frame struct {
		Kind        frameKind `json:"retained_frame"`
		Schema      int       `json:"frame_schema_version"`
		Ordinal     int       `json:"outer_log_ordinal"`
		Transaction string    `json:"transaction_id"`
		Children    []struct {
			Index  int    `json:"child_index"`
			Record string `json:"record_json"`
		} `json:"children"`
		Hash string `json:"content_sha256"`
	}
	if err := json.Unmarshal(lines[0], &frame); err != nil {
		tb.Fatal(err)
	}
	if frame.Kind != permissionFrame {
		tb.Fatal("unexpected synthetic frame")
	}
	h := sha256.New()
	h.Write([]byte("muse.session_permission_transaction.v1\x00"))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], 1)
	h.Write(buf[:])
	transaction, err := hex.DecodeString(strings.ReplaceAll(frame.Transaction, "-", ""))
	if err != nil {
		tb.Fatal(err)
	}
	h.Write(transaction)
	binary.BigEndian.PutUint32(buf[:4], 2)
	h.Write(buf[:4])
	for i, child := range frame.Children {
		binary.BigEndian.PutUint32(buf[:4], uint32(i))
		h.Write(buf[:4])
		binary.BigEndian.PutUint64(buf[:], uint64(len(child.Record)))
		h.Write(buf[:])
		h.Write([]byte(child.Record))
	}
	frame.Hash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	lines[0], err = json.Marshal(frame)
	if err != nil {
		tb.Fatal(err)
	}
	return bytes.Join(lines, []byte("\n"))
}
