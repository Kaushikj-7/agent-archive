//go:build darwin || linux

package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Synthetic test-only owner; no durable production quota claim is made.
type syntheticAdmissionWorkspace struct {
	root         string
	reserved     int64
	reserveErr   error
	reserveCalls int
	fail         error
}

func (w *syntheticAdmissionWorkspace) Root() string                     { return w.root }
func (w *syntheticAdmissionWorkspace) OpenWorkspace() (*os.Root, error) { return os.OpenRoot(w.root) }
func (w *syntheticAdmissionWorkspace) Reserve(n int64) error {
	w.reserveCalls++
	if w.reserveErr != nil {
		return w.reserveErr
	}
	if w.fail != nil {
		return w.fail
	}
	if err := os.MkdirAll(w.root, 0700); err != nil {
		return err
	}
	w.reserved += n
	return nil
}
func (w *syntheticAdmissionWorkspace) Err() error   { return w.fail }
func (w *syntheticAdmissionWorkspace) Close() error { return os.RemoveAll(w.root) }
func admissionWorkspace(t *testing.T) *syntheticAdmissionWorkspace {
	t.Helper()
	return &syntheticAdmissionWorkspace{root: filepath.Join(t.TempDir(), "scratch")}
}

func TestAdmissionPinnedLiveCopyCoherentAcrossExternalGrowth(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	path := filepath.Join(t.TempDir(), "space ü", "state.vscdb")
	w := startWriter(t, path)
	before := `{"composerId":"c","createdAt":1,"conversation":[{"type":2,"text":"before"}]}`
	w.put(map[string]string{"composerData:c": before, "padding": strings.Repeat("p", 256<<10)})
	workspace := admissionWorkspace(t)
	var pinnedCheckpointWAL int64
	copied, stats, err := prepareAdmissionDatabase(t.Context(), path, workspace, admissionCopyOptions{afterStep: func(n int) {
		runtime.GC()
		if n == 1 {
			w.do(writerCommand{Op: writerDelete, Key: "composerData:c"})
			w.put(map[string]string{"composerData:c": `{"composerId":"c","createdAt":1,"conversation":[{"type":2,"text":"after"}]}`, "growth": strings.Repeat("g", 512<<10)})
			w.do(writerCommand{Op: writerCheckpoint})
			info, e := os.Stat(path + "-wal")
			if e != nil {
				t.Fatal(e)
			}
			pinnedCheckpointWAL = info.Size()
			if pinnedCheckpointWAL == 0 {
				t.Fatal("external truncate checkpoint ignored pinned reader")
			}

		}
	}})
	if err != nil {
		t.Fatal(stats, err)
	}
	db, err := sql.Open("sqlite", dsn(copied, false))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var got string
	if err = db.QueryRowContext(t.Context(), "SELECT value FROM cursorDiskKV WHERE key='composerData:c'").Scan(&got); err != nil || got != before {
		t.Fatal("unpinned snapshot", got, err)
	}
	if stats.Steps < 2 || stats.ExternalWALAfter <= stats.ExternalWALBefore {
		t.Fatal("missing actual growth observations", stats)
	}
	info, err := os.Stat(copied)
	if err != nil || info.Size() != stats.Pages*stats.PageBytes {
		t.Fatal(info, stats, err)
	}
	w.do(writerCommand{Op: writerCheckpoint})
	walAfterClose, e := os.Stat(path + "-wal")
	if e != nil || walAfterClose.Size() != 0 {
		t.Fatal("checkpoint remained pinned after copy close", walAfterClose, e)
	}
	t.Logf("external truncate checkpoint WAL bytes while pinned=%d after pinned transaction closes=%d", pinnedCheckpointWAL, walAfterClose.Size())
	if stats.DestinationWriteCalls <= 0 || stats.DestinationMaxWriteEnd > info.Size() || stats.ProhibitedOpens != 0 || stats.ProhibitedWrites != 0 || stats.DestinationOpenDescriptors != 0 {
		t.Fatal("missing before-allocation guard evidence", stats)
	}
	t.Logf("actual pinned copy steps=%d pages=%d page_bytes=%d scratch=%d external_wal_before=%d external_wal_after=%d destination_opens=%d writes=%d write_bytes=%d max_write_end=%d", stats.Steps, stats.Pages, stats.PageBytes, info.Size(), stats.ExternalWALBefore, stats.ExternalWALAfter, stats.DestinationOpens, stats.DestinationWriteCalls, stats.DestinationWriteBytes, stats.DestinationMaxWriteEnd)
}

func TestAdmissionLiveCopyCancellationLeavesCleanupOwned(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	w := startWriter(t, path)
	w.put(map[string]string{"composerData:c": chat("c", 1), "padding": strings.Repeat("p", 256<<10)})
	before := snapshotDir(t, filepath.Dir(path))
	workspace := admissionWorkspace(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	copied, stats, err := prepareAdmissionDatabase(ctx, path, workspace, admissionCopyOptions{afterStep: func(int) { cancel() }})
	if copied != "" || !errors.Is(err, context.Canceled) || stats.Steps != 1 {
		t.Fatal(copied, stats, err)
	}
	after := snapshotDir(t, filepath.Dir(path))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("cancel changed native database", before, after)
	}
	if err = workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(workspace.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned cleanup failed", err)
	}
}

func TestAdmissionReadOnlyLiveCopyLeavesAllNativeBytes(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	w := startWriter(t, path)
	w.put(map[string]string{"composerData:c": chat("c", 1), "padding": strings.Repeat("p", 256<<10)})
	before := snapshotDir(t, filepath.Dir(path))
	workspace := admissionWorkspace(t)
	_, stats, err := prepareReservedLiveImage(t.Context(), path, workspace)
	if err != nil {
		t.Fatal(stats, err)
	}
	assertUnchanged(t, filepath.Dir(path), before)
}

func TestAdmissionDestinationRejectsWritesBeforeAllocation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "native.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	vfs, err := newAdmissionDestinationVFS(t.Context(), path, f, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = vfs.Close() }()
	db, err := openAdmissionTestDatabase(t, admissionDestinationDSN(path, vfs.name, 512, 1000))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.ExecContext(t.Context(), "CREATE TABLE t(v BLOB); INSERT INTO t VALUES (zeroblob(65536))"); err == nil {
		t.Fatal("uncapped driver allocation succeeded")
	} else {
		t.Log("bounded actual driver refusal", err)
	}
	actual, err := os.Stat(path)
	if err != nil || actual.Size() > 4096 || vfs.deniedWrites.Load() == 0 {
		t.Fatal(actual, vfs.deniedWrites.Load(), vfs.deniedOpens.Load(), err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("implicit sidefile allocated", entries, err)
	}
}

func TestAdmissionSourceSidefileDisappearanceNeverCreatesNativeFiles(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.vscdb")
			w := startWriter(t, path)
			w.put(map[string]string{"composerData:c": chat("c", 1)})
			workspace := admissionWorkspace(t)
			copied, _, err := prepareAdmissionDatabase(t.Context(), path, workspace, admissionCopyOptions{beforeSourceOpen: func() {
				if e := os.Remove(path + suffix); e != nil {
					t.Fatal(e)
				}
			}})
			if copied != "" || err == nil {
				t.Fatal("disappearance admitted", copied, err)
			}
			if _, err = os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("reader recreated native sidefile", err)
			}
			if _, err = os.Stat(workspace.Root()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed pin allocated scratch", err)
			}
		})
	}
}

func TestAdmissionSourceRacedSymlinkCannotReadOutsideIdentity(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	w := startWriter(t, path)
	w.put(map[string]string{"composerData:c": chat("c", 1)})
	outside := filepath.Join(t.TempDir(), "outside.db")
	writeDB(t, outside, false, map[string]any{"composerData:outside": chat("outside", 1)})
	before := snapshotDir(t, filepath.Dir(outside))
	workspace := admissionWorkspace(t)
	copied, _, err := prepareAdmissionDatabase(t.Context(), path, workspace, admissionCopyOptions{beforeSourceOpen: func() {
		if e := os.Rename(path, path+".old"); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(outside, path); e != nil {
			t.Fatal(e)
		}
	}})
	if copied != "" || err == nil {
		t.Fatal("opened raced identity", copied, err)
	}
	assertUnchanged(t, filepath.Dir(outside), before)
	if _, err = os.Stat(workspace.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("raced source allocated scratch", err)
	}
}

func TestAdmissionGeometryAndExpiredCallerRefuseBeforeScratch(t *testing.T) {
	t.Parallel()
	for _, geometry := range [][2]int64{{0, 1}, {513, 1}, {65537, 1}, {4096, 0}, {4096, -1}, {4096, 1 << 62}, {65536, 2049}} {
		if _, err := admissionImageBytes(geometry[0], geometry[1]); err == nil {
			t.Fatal("unbounded geometry", geometry)
		}
	}
	if n, err := admissionImageBytes(65536, 2048); err != nil || n != admissionImageLimit {
		t.Fatal(n, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	workspace := admissionWorkspace(t)
	_, stats, err := prepareReservedLiveImage(ctx, filepath.Join(t.TempDir(), "missing.db"), workspace)
	if !errors.Is(err, context.Canceled) || stats.Steps != 0 {
		t.Fatal(stats, err)
	}
	if _, err = os.Stat(workspace.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled caller allocated scratch", err)
	}
}

func TestAdmissionDestinationGeometryCapabilityActualDriver(t *testing.T) {
	t.Parallel()
	for _, size := range []int64{512, 4096, 65536} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "native.db")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			path, err = filepath.EvalSymlinks(path)
			if err != nil {
				t.Fatal(err)
			}
			vfs, err := newAdmissionDestinationVFS(t.Context(), path, f, 128<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = vfs.Close() }()
			if err = verifyAdmissionDestination(t.Context(), admissionDestinationDSN(path, vfs.name, size, 100), size, 100); err != nil {
				t.Fatal(err)
			}
			if info, e := os.Stat(path); e != nil || info.Size() != 0 {
				t.Fatal("geometry probe allocated disk pages", info, e)
			}
		})
	}
}

func TestAdmissionDestinationAncestorSwapCannotWriteOutside(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("known Unix SHM ownership write capability is refused as root")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	w := startWriter(t, path)
	w.put(map[string]string{"composerData:c": `{"composerId":"c","createdAt":1,"conversation":[{"type":2,"text":"owned"}]}`, "padding": strings.Repeat("p", 256<<10)})
	workspace := admissionWorkspace(t)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "native.db")
	sentinel := []byte("outside untouched")
	if err := os.WriteFile(outsideFile, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(workspace.Root())
	retained := parent + "-retained"
	restored := false
	restore := func() {
		if !restored {
			if err := os.Remove(parent); err != nil {
				t.Error(err)
			}
			if err := os.Rename(retained, parent); err != nil {
				t.Error(err)
			}
			restored = true
		}
	}
	_, stats, err := prepareAdmissionDatabase(t.Context(), path, workspace, admissionCopyOptions{beforeDestinationOpen: func() {
		if e := os.Rename(parent, retained); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(outside, parent); e != nil {
			t.Fatal(e)
		}
		deferRestore := restore
		t.Cleanup(deferRestore)
	}})
	if err != nil {
		t.Fatal("descriptor-bound backup failed", stats, err)
	}
	actual, err := os.ReadFile(outsideFile)
	if err != nil || !reflect.DeepEqual(actual, sentinel) {
		t.Fatal("outside destination modified", string(actual), err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatal("outside allocation", entries, err)
	}

	restore()
	root, err := workspace.OpenWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err = verifyAdmissionImage(root, stats.Pages*stats.PageBytes); err != nil {
		t.Fatal(err)
	}
	if stats.DestinationOpenDescriptors != 0 {
		t.Fatal("native descriptors remain active", stats)
	}
}

func TestAdmissionDestinationShortReadZeroFillsNativeBuffer(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "native.db")
	if err := os.WriteFile(path, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	tls := libc.NewTLS()
	defer tls.Close()
	buffer := libc.Xmalloc(tls, 8)
	if buffer == 0 {
		t.Fatal("native buffer allocation failed")
	}
	defer libc.Xfree(tls, buffer)
	copy(libc.GoBytes(buffer, 8), []byte("xxxxxxxx"))
	vfs := &admissionVFS{context: t.Context(), heldFile: held}
	if rc := admissionReadDestination(vfs, buffer, 8, 0); rc != sqlite3.SQLITE_IOERR_SHORT_READ {
		t.Fatal(rc)
	}
	if got := string(libc.GoBytes(buffer, 8)); got != "abc\x00\x00\x00\x00\x00" {
		t.Fatal("short read left native garbage", []byte(got))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	vfs.context = ctx
	if rc := admissionReadDestination(vfs, buffer, 8, 0); rc != sqlite3.SQLITE_INTERRUPT {
		t.Fatal("cancelled native read", rc)
	}
}

func TestAdmissionRefusesWritableProcessSharedMemoryBeforeMapping(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root admission is refused before native open")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.ExecContext(t.Context(), `PRAGMA journal_mode=WAL; CREATE TABLE cursorDiskKV(key TEXT PRIMARY KEY,value BLOB); INSERT INTO cursorDiskKV VALUES ('composerData:c','{}')`); err != nil {
		t.Fatal(err)
	}
	before := snapshotDir(t, filepath.Dir(path))
	workspace := admissionWorkspace(t)
	copied, _, err := prepareReservedLiveImage(t.Context(), path, workspace)
	if err == nil || copied != "" {
		t.Fatal("reused writable native shared memory", copied, err)
	}
	assertUnchanged(t, filepath.Dir(path), before)
	if _, err = os.Stat(workspace.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsafe shared memory allocated scratch", err)
	}
}

func TestAdmissionRacedSourceRefusesBeforeNativeDelegation(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root admission is refused before native open")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	writeDB(t, path, false, map[string]any{"composerData:c": chat("c", 1)})
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	vfs, err := newAdmissionVFS(map[string]os.FileInfo{path: info}, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = vfs.Close() }()
	vfs.setSourceRoot(root, filepath.Dir(path))
	if err = os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.db")
	writeDB(t, outside, false, map[string]any{"composerData:outside": chat("outside", 1)})
	replacement, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", admissionDSN(dsn(path, false), vfs.name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var count int
	unlock := lockAdmissionRegistry()
	err = db.QueryRowContext(t.Context(), "SELECT count(*) FROM cursorDiskKV").Scan(&count)
	unlock()
	if err == nil {
		t.Fatal("raced identity admitted")
	}
	if vfs.opens.Load() != 0 {
		t.Fatal("native open ran before rooted identity refusal", vfs.opens.Load())
	}
}

func admissionWritableMapProbe(_ *libc.TLS, p uintptr, _, _, _ int32, _ uintptr) int32 {
	native := loadAdmissionC[sqlite3.TunixFile](p)
	inode := loadAdmissionC[sqlite3.TunixInodeInfo](native.FpInode)
	node := loadAdmissionC[sqlite3.TunixShmNode](inode.FpShmNode)
	node.FnRef++
	writeAdmissionC(inode.FpShmNode, node)
	return sqlite3.SQLITE_OK
}

func TestAdmissionWritableSharedMemoryRefusesBeforeCallback(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.vscdb")
	if err := os.WriteFile(path+"-shm", []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + "-shm")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	tls := libc.NewTLS()
	defer tls.Close()
	name, err := libc.CString(path)
	if err != nil {
		t.Fatal(err)
	}
	defer libc.Xfree(tls, name)
	file := libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(sqlite3.TunixFile{})))
	inode := libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(sqlite3.TunixInodeInfo{})))
	node := libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(sqlite3.TunixShmNode{})))
	if file == 0 || inode == 0 || node == 0 {
		t.Fatal("synthetic native allocation failed")
	}
	defer libc.Xfree(tls, file)
	defer libc.Xfree(tls, inode)
	defer libc.Xfree(tls, node)
	writeAdmissionC(file, sqlite3.TunixFile{FzPath: name, FpInode: inode})
	writeAdmissionC(inode, sqlite3.TunixInodeInfo{FpShmNode: node})
	writeAdmissionC(node, sqlite3.TunixShmNode{FisReadonly: 0, FhShm: -1})
	vfs := &admissionVFS{sourceRoot: root, sourceDirectory: filepath.Dir(path), allowed: map[string]os.FileInfo{path + "-shm": info}}
	defer func() {
		for _, held := range vfs.sourcePins {
			_ = held.Close()
		}
	}()
	f := &admissionFile{vfs: vfs, originalMethods: sqlite3.Tsqlite3_io_methods{FxShmMap: callbackPointer(admissionWritableMapProbe)}}
	admissionVFSState.Lock()
	admissionVFSState.files[file] = f
	admissionVFSState.Unlock()
	defer func() { admissionVFSState.Lock(); delete(admissionVFSState.files, file); admissionVFSState.Unlock() }()
	if rc := admissionShmMap(tls, file, 0, 32768, 0, 0); rc != sqlite3.SQLITE_READONLY {
		t.Fatal(rc)
	}
	if got := loadAdmissionC[sqlite3.TunixShmNode](node).FnRef; got != 0 {
		t.Fatal("writable SHM delegated before refusal", got)
	}
}

func TestAdmissionDescriptorVerificationPreservesPinnedReadLocks(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root admission is refused before native open")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	w := startWriter(t, path)
	w.put(map[string]string{"composerData:c": chat("c", 1), "padding": strings.Repeat("p", 256<<10)})
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	workspace := admissionWorkspace(t)
	var probeErr error
	_, _, err = prepareAdmissionDatabase(t.Context(), path, workspace, admissionCopyOptions{afterStep: func(n int) {
		if n != 1 {
			return
		}
		expected, e := os.Stat(path + "-shm")
		if e != nil {
			probeErr = e
			return
		}
		fd := int32(-1)
		admissionVFSState.Lock()
		for p, f := range admissionVFSState.files {
			if f.vfs.heldFile != nil || f.vfs.sourceDirectory != filepath.Dir(path) {
				continue
			}
			native := loadAdmissionC[sqlite3.TunixFile](p)
			if native.FpShm == 0 {
				continue
			}
			shm := loadAdmissionC[sqlite3.TunixShm](native.FpShm)
			node := loadAdmissionC[sqlite3.TunixShmNode](shm.FpShmNode)
			fd = node.FhShm
		}
		admissionVFSState.Unlock()
		if fd < 0 || !admissionDescriptorMatches(fd, expected) {
			probeErr = errors.New("source descriptor unavailable")
			return
		}
		w.do(writerCommand{Op: writerCheckpoint})
		info, e := os.Stat(path + "-wal")
		if e != nil || info.Size() == 0 {
			probeErr = errors.New("descriptor verification dropped pinned reader locks")
		}
	}})
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionDestinationRegistrationPreservesExistingVFS(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root source admission is refused")
	}
	path := filepath.Join(t.TempDir(), "native.db")
	held, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	info, err := held.Stat()
	if err != nil {
		t.Fatal(err)
	}
	source, err := newAdmissionVFS(map[string]os.FileInfo{path: info}, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	destination, err := newAdmissionDestinationVFS(t.Context(), path, held, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destination.Close() }()
	tls := libc.NewTLS()
	defer tls.Close()
	name, err := libc.CString(source.name)
	if err != nil {
		t.Fatal(err)
	}
	defer libc.Xfree(tls, name)
	unlock := lockAdmissionRegistry()
	actual := sqlite3.Xsqlite3_vfs_find(tls, name)
	unlock()
	if actual != source.ptr {
		t.Fatal("destination setup lost registered source VFS", actual, source.ptr)
	}
}

func openAdmissionTestDatabase(t *testing.T, dsn string) (*sql.DB, error) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	conn, err := admissionConnection(t.Context(), db)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if err = conn.Close(); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func TestAdmissionScratchRefusalPrecedesDestinationCreation(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root native SHM capability refused")
	}
	path := filepath.Join(t.TempDir(), "state.vscdb")
	w := startWriter(t, path)
	w.put(map[string]string{"composerData:c": chat("c", 1)})
	workspace := admissionWorkspace(t)
	refusal := errors.New("synthetic durable scratch capacity exhausted")
	workspace.reserveErr = refusal
	before := snapshotDir(t, filepath.Dir(path))
	copied, stats, err := prepareReservedLiveImage(t.Context(), path, workspace)
	if copied != "" || !errors.Is(err, refusal) || workspace.reserveCalls != 1 || stats.DestinationOpens != 0 {
		t.Fatal(copied, stats, workspace.reserveCalls, err)
	}
	if _, err := os.Stat(workspace.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused reservation created scratch", err)
	}
	assertUnchanged(t, filepath.Dir(path), before)
}

func TestAdmissionBackendDoesNotConvertSettledPathsIntoEvidence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.vscdb")
	writeDB(t, path, false, map[string]any{"composerData:c": chat("c", 1)})
	workspace := admissionWorkspace(t)
	copied, stats, err := prepareReservedLiveImage(t.Context(), path, workspace)
	if copied != "" || err == nil || stats.WorkspaceImage || workspace.reserveCalls != 0 {
		t.Fatal(copied, stats, workspace.reserveCalls, err)
	}
}

func TestAdmissionVFSRepeatedClosePreservesRegistry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "native.db")
	held, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	vfs, err := newAdmissionDestinationVFS(t.Context(), path, held, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err = vfs.Close(); err != nil {
		t.Fatal(err)
	}
	if err = vfs.Close(); err != nil {
		t.Fatal(err)
	}
}
