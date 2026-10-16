//go:build darwin || linux

package cursorstore

import (
	"errors"
	"io"
	"os"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// The destination has exactly one SQLite connection at a time, one private
// owned file, MEMORY journals and no shared users. All native opens bind this
// held rooted file; SQLite never delegates an ambient writable pathname.
func admissionOpenDestination(tls *libc.TLS, v *admissionVFS, pFile uintptr, flags int32, out uintptr, expected os.FileInfo) int32 {
	if v.heldFile == nil || v.context.Err() != nil {
		return sqlite3.SQLITE_CANTOPEN
	}
	info, err := v.heldFile.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(expected, info) || flags&sqlite3.SQLITE_OPEN_MAIN_DB == 0 {
		return sqlite3.SQLITE_CANTOPEN
	}
	if !v.activeDescriptors.CompareAndSwap(0, 1) {
		v.deniedOpens.Add(1)
		return sqlite3.SQLITE_CANTOPEN
	}
	f := &admissionFile{vfs: v, table: sqlite3.Tsqlite3_io_methods{
		FiVersion: 1,
		FxClose:   callbackPointer(admissionClose), FxRead: callbackPointer(admissionRead),
		FxWrite: callbackPointer(admissionWrite), FxTruncate: callbackPointer(admissionTruncate),
		FxSync: callbackPointer(admissionSyncDestination), FxFileSize: callbackPointer(admissionSizeDestination),
		FxLock: callbackPointer(admissionPrivateLock), FxUnlock: callbackPointer(admissionPrivateLock),
		FxCheckReservedLock: callbackPointer(admissionPrivateReserved), FxFileControl: callbackPointer(admissionFileControl),
		FxSectorSize: callbackPointer(admissionSectorSize), FxDeviceCharacteristics: callbackPointer(admissionDeviceCharacteristics),
	}}
	f.pin.Pin(&f.table)
	f.methods = libc.Xmalloc(tls, libc.Tsize_t(unsafe.Sizeof(f.table)))
	if f.methods == 0 {
		f.pin.Unpin()
		v.activeDescriptors.Add(-1)
		return sqlite3.SQLITE_NOMEM
	}
	writeAdmissionC(f.methods, f.table)
	admissionVFSState.Lock()
	admissionVFSState.files[pFile] = f
	admissionVFSState.Unlock()
	v.opens.Add(1)
	setAdmissionMethods(pFile, f.methods)
	if out != 0 {
		writeAdmissionC(out, flags)
	}
	return sqlite3.SQLITE_OK
}

func writeAdmissionC[T any](ptr uintptr, value T) {
	source := unsafe.Slice((*byte)(unsafe.Pointer(&value)), int(unsafe.Sizeof(value)))
	copy(libc.GoBytes(ptr, len(source)), source)
}

func admissionReadDestination(v *admissionVFS, b uintptr, n int32, offset int64) int32 {
	if v.context.Err() != nil {
		return sqlite3.SQLITE_INTERRUPT
	}
	if n <= 0 || int64(n) > admissionImageLimit || offset < 0 || offset > admissionImageLimit-int64(n) {
		return sqlite3.SQLITE_IOERR_READ
	}
	buffer := make([]byte, int(n))
	got, err := v.heldFile.ReadAt(buffer, offset)
	copy(libc.GoBytes(b, int(n)), buffer)
	if got < int(n) && errors.Is(err, io.EOF) {
		return sqlite3.SQLITE_IOERR_SHORT_READ
	}
	if err != nil {
		return sqlite3.SQLITE_IOERR_READ
	}
	return sqlite3.SQLITE_OK
}

func admissionWriteDestination(v *admissionVFS, b uintptr, n int32, offset int64) int32 {
	// The caller already checked context and the reserved end offset before
	// this bounded allocation and before the actual filesystem write.
	if n < 0 || int64(n) > admissionImageLimit {
		return sqlite3.SQLITE_IOERR_WRITE
	}
	if n == 0 {
		return sqlite3.SQLITE_OK
	}
	buffer := make([]byte, int(n))
	copy(buffer, libc.GoBytes(b, int(n)))
	got, err := v.heldFile.WriteAt(buffer, offset)
	if err != nil || got != int(n) {
		return sqlite3.SQLITE_IOERR_WRITE
	}
	return sqlite3.SQLITE_OK
}

func admissionSyncDestination(tls *libc.TLS, p uintptr, flags int32) int32 {
	f, _ := admissionLookup(p)
	if f.vfs.context.Err() != nil {
		return sqlite3.SQLITE_INTERRUPT
	}
	if err := f.vfs.heldFile.Sync(); err != nil {
		return sqlite3.SQLITE_IOERR_FSYNC
	}
	return sqlite3.SQLITE_OK
}

func admissionSizeDestination(tls *libc.TLS, p, out uintptr) int32 {
	f, _ := admissionLookup(p)
	if f.vfs.context.Err() != nil {
		return sqlite3.SQLITE_INTERRUPT
	}
	info, err := f.vfs.heldFile.Stat()
	if err != nil {
		return sqlite3.SQLITE_IOERR_FSTAT
	}
	writeAdmissionC(out, info.Size())
	return sqlite3.SQLITE_OK
}

func admissionPrivateLock(*libc.TLS, uintptr, int32) int32 { return sqlite3.SQLITE_OK }

func admissionPrivateReserved(tls *libc.TLS, p, out uintptr) int32 {
	writeAdmissionC(out, int32(0))
	return sqlite3.SQLITE_OK
}

func admissionSectorSize(*libc.TLS, uintptr) int32 { return 4096 }

func admissionDeviceCharacteristics(*libc.TLS, uintptr) int32 { return 0 }
