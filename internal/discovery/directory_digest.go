package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// directoryBatch keeps one bounded getdents batch's fingerprints for the
// metadata inventory's directory digests. These include irrelevant regular
// files, directories and symlinks, so an unrelated header rewrite cannot hide
// behind an unchanged directory mtime during validation.
type directoryBatch struct {
	Stamp       string
	Entries     []string
	Unavailable bool
}

func entryFingerprint(name string, info os.FileInfo) string {
	identity := ""
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity = fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	}
	raw := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", name, identity, info.Mode(), info.Size(), info.ModTime().UnixNano())
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// memberFingerprint identifies a rollout file's directory membership: its
// name, file identity and type. Size and modification time are left out so an
// append by a running Codex session does not change a directory's digest.
func memberFingerprint(name string, info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	raw := fmt.Sprintf("member\x00%s\x00%d:%d\x00%d", name, stat.Dev, stat.Ino, info.Mode())
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func appendDirectoryDigest(prior string, entries []string) string {
	digest, err := hex.DecodeString(prior)
	if err != nil || len(digest) != sha256.Size {
		digest = make([]byte, sha256.Size)
	}
	for _, entry := range entries {
		sum := sha256.Sum256(append(digest, []byte(entry)...))
		digest = sum[:]
	}
	return hex.EncodeToString(digest)
}

func directoryStamp(root, path string) string {
	info, err := os.Lstat(filepath.Join(root, path))
	if errors.Is(err, os.ErrNotExist) {
		sum := sha256.Sum256([]byte("absent"))
		return hex.EncodeToString(sum[:])
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	return entryFingerprint("", info)
}
