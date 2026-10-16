//go:build darwin || linux

package cursorstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Admission uses a private Unix VFS wrapper. SQLite's ordinary VFS stays
// untouched. Native descriptors are verified before reading; destination
// writes and truncation are checked before delegation, including final flush.
type admissionVFS struct {
	name              string
	ptr               uintptr
	namePtr           uintptr
	base              uintptr
	native            sqlite3.Tsqlite3_vfs
	clone             sqlite3.Tsqlite3_vfs
	pin               runtime.Pinner
	allowed           map[string]os.FileInfo
	limit             int64
	writable          bool
	sourceRoot        *os.Root
	sourceDirectory   string
	sourcePins        map[string]*os.File
	heldFile          *os.File
	context           context.Context
	writeCalls        atomic.Int64
	writeBytes        atomic.Int64
	maxWriteEnd       atomic.Int64
	deniedWrites      atomic.Int64
	opens             atomic.Int64
	deniedOpens       atomic.Int64
	activeDescriptors atomic.Int64
}

type admissionFile struct {
	vfs             *admissionVFS
	original        uintptr
	originalMethods sqlite3.Tsqlite3_io_methods
	table           sqlite3.Tsqlite3_io_methods
	methods         uintptr
	pin             runtime.Pinner
}

var admissionVFSState = struct {
	sync.Mutex
	vfs   map[uintptr]*admissionVFS
	files map[uintptr]*admissionFile
}{vfs: map[uintptr]*admissionVFS{}, files: map[uintptr]*admissionFile{}}

var admissionVFSSequence atomic.Uint64

// Registry links are native mutable state. Serialize admission registration,
// lookup/open and unregister/free, even on SQLite builds with no-op mutexes.
var admissionRegistryMutex sync.Mutex

func lockAdmissionRegistry() func() {
	admissionRegistryMutex.Lock()
	return admissionRegistryMutex.Unlock
}

func callbackPointer[T any](fn T) uintptr { return *(*uintptr)(unsafe.Pointer(&struct{ f T }{fn})) }

func loadAdmissionC[T any](ptr uintptr) T {
	var value T
	target := unsafe.Slice((*byte)(unsafe.Pointer(&value)), int(unsafe.Sizeof(value)))
	copy(target, libc.GoBytes(ptr, len(target)))
	return value
}

func setAdmissionMethods(file, methods uintptr) {
	writeAdmissionC(file, sqlite3.Tsqlite3_file{FpMethods: methods})
}

func newAdmissionVFS(allowed map[string]os.FileInfo, writable bool, limit int64) (*admissionVFS, error) {
	if !writable && os.Geteuid() == 0 {
		return nil, errors.New("read-only cursor admission cannot delegate Unix SHM ownership changes as root")
	}
	tls := libc.NewTLS()
	defer tls.Close()
	unlock := lockAdmissionRegistry()
	base := sqlite3.Xsqlite3_vfs_find(tls, 0)
	var native sqlite3.Tsqlite3_vfs
	if base != 0 {
		native = loadAdmissionC[sqlite3.Tsqlite3_vfs](base)
	}
	unlock()
	if base == 0 {
		return nil, errors.New("bounded admission VFS unavailable")
	}
	if libc.GoString(native.FzName) != "unix" || native.FszOsFile != int32(unsafe.Sizeof(sqlite3.TunixFile{})) || native.FxOpen == 0 {
		return nil, errors.New("bounded admission requires the known Unix VFS ABI")
	}
	name := "archive-admission-" + itoa(admissionVFSSequence.Add(1))
	namePtr, err := libc.CString(name)
	if err != nil {
		return nil, err
	}
	clone := native
	clone.FzName = namePtr
	clone.FpNext = 0
	clone.FxOpen = callbackPointer(admissionOpen)
	clone.FxDelete = callbackPointer(admissionDelete)
	if writable {
		clone.FxFullPathname = callbackPointer(admissionFullPathname)
	}
	v := &admissionVFS{name: name, namePtr: namePtr, base: base, allowed: allowed, writable: writable, limit: limit, native: native, clone: clone}
	v.pin.Pin(&v.clone)
	v.ptr = libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(v.clone)))
	if v.ptr == 0 {
		v.pin.Unpin()
		libc.Xfree(tls, namePtr)
		return nil, errors.New("bounded admission VFS allocation failed")
	}
	writeAdmissionC(v.ptr, v.clone)
	admissionVFSState.Lock()
	admissionVFSState.vfs[v.ptr] = v
	admissionVFSState.Unlock()
	unlock = lockAdmissionRegistry()
	rc := sqlite3.Xsqlite3_vfs_register(tls, v.ptr, 0)
	unlock()
	if rc != sqlite3.SQLITE_OK {
		return nil, errors.Join(errors.New("bounded admission VFS registration failed"), v.Close())
	}
	return v, nil
}

func newAdmissionDestinationVFS(ctx context.Context, path string, held *os.File, limit int64) (*admissionVFS, error) {
	info, err := held.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("bounded destination descriptor unavailable")
	}
	v, err := newAdmissionVFS(map[string]os.FileInfo{path: info}, true, limit)
	if err != nil {
		return nil, err
	}
	v.heldFile = held
	v.context = ctx
	return v, nil
}

func itoa(n uint64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}

func (v *admissionVFS) Close() error {
	unlock := lockAdmissionRegistry()
	defer unlock()
	if v.ptr == 0 {
		return nil
	}
	if v.activeDescriptors.Load() != 0 {
		return errors.New("cursor admission native handles remain open")
	}
	tls := libc.NewTLS()
	defer tls.Close()
	if sqlite3.Xsqlite3_vfs_unregister(tls, v.ptr) != sqlite3.SQLITE_OK {
		return errors.New("cursor admission VFS cleanup failed")
	}
	admissionVFSState.Lock()
	delete(admissionVFSState.vfs, v.ptr)
	admissionVFSState.Unlock()
	v.pin.Unpin()
	libc.Xfree(tls, v.ptr)
	libc.Xfree(tls, v.namePtr)
	v.ptr, v.namePtr = 0, 0
	var err error
	for _, held := range v.sourcePins {
		err = errors.Join(err, held.Close())
	}
	return err
}

func admissionFullPathname(tls *libc.TLS, pVFS, name uintptr, capacity int32, out uintptr) int32 {
	admissionVFSState.Lock()
	v := admissionVFSState.vfs[pVFS]
	admissionVFSState.Unlock()
	if v == nil || name == 0 {
		return sqlite3.SQLITE_CANTOPEN
	}
	path := libc.GoString(name)
	if _, ok := v.allowed[path]; !ok || len(path)+1 > int(capacity) {
		return sqlite3.SQLITE_CANTOPEN
	}
	libc.Xmemcpy(tls, out, name, libc.Tsize_t(len(path)+1))
	return sqlite3.SQLITE_OK
}

func admissionDelete(*libc.TLS, uintptr, uintptr, int32) int32 { return sqlite3.SQLITE_READONLY }

func admissionOpen(tls *libc.TLS, pVFS, zName, pFile uintptr, flags int32, out uintptr) int32 {
	admissionVFSState.Lock()
	v := admissionVFSState.vfs[pVFS]
	admissionVFSState.Unlock()
	if v == nil || zName == 0 {
		return sqlite3.SQLITE_CANTOPEN
	}
	path := libc.GoString(zName)
	approved, ok := v.allowed[path]
	if !ok || flags&(sqlite3.SQLITE_OPEN_MAIN_JOURNAL|sqlite3.SQLITE_OPEN_TEMP_DB|sqlite3.SQLITE_OPEN_TEMP_JOURNAL|sqlite3.SQLITE_OPEN_TRANSIENT_DB|sqlite3.SQLITE_OPEN_SUBJOURNAL|sqlite3.SQLITE_OPEN_SUPER_JOURNAL) != 0 {
		v.deniedOpens.Add(1)
		return sqlite3.SQLITE_CANTOPEN
	}
	flags &^= sqlite3.SQLITE_OPEN_CREATE | sqlite3.SQLITE_OPEN_DELETEONCLOSE
	if !v.writable {
		flags &^= sqlite3.SQLITE_OPEN_READWRITE
		flags |= sqlite3.SQLITE_OPEN_READONLY
	}
	if v.writable {
		return admissionOpenDestination(tls, v, pFile, flags, out, approved)
	}
	if err := v.pinSourceFile(path, approved); err != nil {
		return sqlite3.SQLITE_CANTOPEN
	}
	base := &v.native
	open := *(*func(*libc.TLS, uintptr, uintptr, uintptr, int32, uintptr) int32)(unsafe.Pointer(&base.FxOpen))
	v.opens.Add(1)
	rc := open(tls, v.base, zName, pFile, flags, out)
	if rc != sqlite3.SQLITE_OK {
		return rc
	}
	original := loadAdmissionC[sqlite3.Tsqlite3_file](pFile).FpMethods
	native := loadAdmissionC[sqlite3.Tsqlite3_io_methods](original)
	closeFile := *(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&native.FxClose))
	if !admissionDescriptorMatches(loadAdmissionC[sqlite3.TunixFile](pFile).Fh, approved) {
		closeFile(tls, pFile)
		return sqlite3.SQLITE_CANTOPEN
	}
	f := &admissionFile{vfs: v, original: original, originalMethods: native, table: native}
	methods := &f.table
	methods.FxClose = callbackPointer(admissionClose)
	methods.FxRead = callbackPointer(admissionRead)
	methods.FxWrite = callbackPointer(admissionWrite)
	methods.FxTruncate = callbackPointer(admissionTruncate)
	methods.FxFileControl = callbackPointer(admissionFileControl)
	methods.FxShmMap = callbackPointer(admissionShmMap)
	methods.FxShmUnmap = callbackPointer(admissionShmUnmap)
	methods.FxFetch = 0
	methods.FxUnfetch = 0
	f.pin.Pin(methods)
	f.methods = libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(f.table)))
	if f.methods == 0 {
		f.pin.Unpin()
		closeFile(tls, pFile)
		return sqlite3.SQLITE_NOMEM
	}
	writeAdmissionC(f.methods, f.table)
	admissionVFSState.Lock()
	v.activeDescriptors.Add(1)
	admissionVFSState.files[pFile] = f
	admissionVFSState.Unlock()
	setAdmissionMethods(pFile, f.methods)
	return rc
}

func admissionDescriptorMatches(fd int32, expected os.FileInfo) bool {
	// Dup followed by Close drops this process's POSIX locks on the inode.
	// Verify with Fstat alone; never open or close a verification descriptor.
	var actual unix.Stat_t
	if unix.Fstat(int(fd), &actual) != nil {
		return false
	}
	before, ok := expected.Sys().(*syscall.Stat_t)
	return ok && actual.Mode&unix.S_IFMT == unix.S_IFREG && actual.Dev == before.Dev && actual.Ino == before.Ino
}

func admissionLookup(p uintptr) (*admissionFile, *sqlite3.Tsqlite3_io_methods) {
	admissionVFSState.Lock()
	f := admissionVFSState.files[p]
	admissionVFSState.Unlock()
	return f, &f.originalMethods
}

func admissionClose(tls *libc.TLS, p uintptr) int32 {
	f, m := admissionLookup(p)
	rc := int32(sqlite3.SQLITE_OK)
	if f.vfs.heldFile == nil {
		closeFile := *(*func(*libc.TLS, uintptr) int32)(unsafe.Pointer(&m.FxClose))
		setAdmissionMethods(p, f.original)
		rc = closeFile(tls, p)
	} else {
		setAdmissionMethods(p, 0)
	}
	admissionVFSState.Lock()
	delete(admissionVFSState.files, p)
	admissionVFSState.Unlock()
	f.pin.Unpin()
	libc.Xfree(tls, f.methods)
	f.vfs.activeDescriptors.Add(-1)
	return rc
}

func admissionRead(tls *libc.TLS, p, b uintptr, n int32, offset int64) int32 {
	_, m := admissionLookup(p)
	f, _ := admissionLookup(p)
	if f.vfs.heldFile != nil {
		return admissionReadDestination(f.vfs, b, n, offset)
	}
	read := *(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&m.FxRead))
	return read(tls, p, b, n, offset)
}

func admissionWrite(tls *libc.TLS, p, b uintptr, n int32, offset int64) int32 {
	f, m := admissionLookup(p)
	if !f.vfs.writable {
		return sqlite3.SQLITE_READONLY
	}
	if f.vfs.context != nil && f.vfs.context.Err() != nil {
		return sqlite3.SQLITE_INTERRUPT
	}
	if n < 0 || offset < 0 || int64(n) > f.vfs.limit-offset || offset > f.vfs.limit {
		f.vfs.deniedWrites.Add(1)
		return sqlite3.SQLITE_FULL
	}
	f.vfs.writeCalls.Add(1)
	f.vfs.writeBytes.Add(int64(n))
	end := offset + int64(n)
	for old := f.vfs.maxWriteEnd.Load(); end > old; old = f.vfs.maxWriteEnd.Load() {
		if f.vfs.maxWriteEnd.CompareAndSwap(old, end) {
			break
		}
	}
	if f.vfs.heldFile != nil {
		return admissionWriteDestination(f.vfs, b, n, offset)
	}
	write := *(*func(*libc.TLS, uintptr, uintptr, int32, int64) int32)(unsafe.Pointer(&m.FxWrite))
	return write(tls, p, b, n, offset)
}

func admissionTruncate(tls *libc.TLS, p uintptr, n int64) int32 {
	f, m := admissionLookup(p)
	if !f.vfs.writable {
		return sqlite3.SQLITE_READONLY
	}
	if f.vfs.context != nil && f.vfs.context.Err() != nil {
		return sqlite3.SQLITE_INTERRUPT
	}
	if n < 0 || n > f.vfs.limit {
		return sqlite3.SQLITE_FULL
	}
	if f.vfs.heldFile != nil {
		if err := f.vfs.heldFile.Truncate(n); err != nil {
			return sqlite3.SQLITE_IOERR_TRUNCATE
		}
		return sqlite3.SQLITE_OK
	}
	truncate := *(*func(*libc.TLS, uintptr, int64) int32)(unsafe.Pointer(&m.FxTruncate))
	return truncate(tls, p, n)
}

func admissionFileControl(tls *libc.TLS, p uintptr, op int32, arg uintptr) int32 {
	// Allocation hints and unknown controls never reach the filesystem. Query
	// controls not implemented here retain SQLite's documented NOTFOUND fallback.
	return sqlite3.SQLITE_NOTFOUND
}

func admissionShmMap(tls *libc.TLS, p uintptr, region, size, extend int32, out uintptr) int32 {
	f, m := admissionLookup(p)
	// Bound borrowed WAL-index mappings inside the charged SQLite allowance.
	if region < 0 || size <= 0 || int64(size) > 4<<20 || int64(region)+1 > (4<<20)/int64(size) {
		return sqlite3.SQLITE_READONLY
	}
	if f.vfs.writable || m.FxShmMap == 0 {
		return sqlite3.SQLITE_READONLY
	}
	native := loadAdmissionC[sqlite3.TunixFile](p)
	path := libc.GoString(native.FzPath) + "-shm"
	expected, ok := f.vfs.allowed[path]
	if !ok {
		return sqlite3.SQLITE_READONLY
	}
	if err := f.vfs.pinSourceFile(path, expected); err != nil {
		return sqlite3.SQLITE_READONLY
	}
	// Unix reuses an existing process-wide inode SHM node before consulting
	// readonly_shm. Refuse a writable node BEFORE mapping: checking afterwards
	// would be too late to guarantee read-only native memory access.
	inode := loadAdmissionC[sqlite3.TunixInodeInfo](native.FpInode)
	if inode.FpShmNode != 0 {
		node := loadAdmissionC[sqlite3.TunixShmNode](inode.FpShmNode)
		if node.FisReadonly == 0 || !admissionDescriptorMatches(node.FhShm, expected) {
			return sqlite3.SQLITE_READONLY
		}
	}
	mapFile := *(*func(*libc.TLS, uintptr, int32, int32, int32, uintptr) int32)(unsafe.Pointer(&m.FxShmMap))
	rc := mapFile(tls, p, region, size, 0, out)
	if rc != sqlite3.SQLITE_OK && rc != sqlite3.SQLITE_READONLY {
		return rc
	}
	native = loadAdmissionC[sqlite3.TunixFile](p)
	if native.FpShm == 0 {
		return sqlite3.SQLITE_READONLY
	}
	shm := loadAdmissionC[sqlite3.TunixShm](native.FpShm)
	node := loadAdmissionC[sqlite3.TunixShmNode](shm.FpShmNode)
	if node.FisReadonly == 0 || !admissionDescriptorMatches(node.FhShm, expected) {
		return sqlite3.SQLITE_READONLY
	}
	return rc
}

func admissionShmUnmap(tls *libc.TLS, p uintptr, deleteFlag int32) int32 {
	_, m := admissionLookup(p)
	unmap := *(*func(*libc.TLS, uintptr, int32) int32)(unsafe.Pointer(&m.FxShmUnmap))
	return unmap(tls, p, 0)
}

func (v *admissionVFS) copyStats(stats *AdmissionCopyStats) {
	stats.DestinationWriteCalls = int(v.writeCalls.Load())
	stats.DestinationWriteBytes = v.writeBytes.Load()
	stats.DestinationMaxWriteEnd = v.maxWriteEnd.Load()
	stats.ProhibitedWrites = int(v.deniedWrites.Load())
	stats.DestinationOpens = int(v.opens.Load())
	stats.ProhibitedOpens = int(v.deniedOpens.Load())
	stats.DestinationOpenDescriptors = int(v.activeDescriptors.Load())
}

// Native open/map callbacks cannot read outside the approved root. A rooted
// descriptor pins each observed identity before native delegation; the native
// descriptor is independently checked before SQLite can read it. Delegation is
// read-only and cannot create, truncate, delete or initialize writable SHM.
func (v *admissionVFS) pinSourceFile(path string, expected os.FileInfo) error {
	if v.sourceRoot == nil || filepath.Dir(path) != v.sourceDirectory {
		return NotChecked(Unreadable)
	}
	name := filepath.Base(path)
	before, err := v.sourceRoot.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(expected, before) {
		return NotChecked(ChangedDuringRead)
	}
	if v.sourcePins == nil {
		v.sourcePins = map[string]*os.File{}
	}
	held := v.sourcePins[path]
	if held == nil {
		held, err = v.sourceRoot.Open(name)
		if err != nil {
			return err
		}
		// Retain every descriptor until SQLite closes and the VFS unregisters.
		// Closing even a read-only fd early would drop native POSIX read locks.
		v.sourcePins[path] = held
	}
	opened, err := held.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return NotChecked(ChangedDuringRead)
	}
	return nil
}

func (v *admissionVFS) setSourceRoot(root *os.Root, directory string) {
	v.sourceRoot = root
	v.sourceDirectory = directory
}
