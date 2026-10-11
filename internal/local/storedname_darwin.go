//go:build darwin && cgo

package local

/*
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <unistd.h>

// aa_stored_name copies the name path's last component is stored under
// (getattrlist ATTR_CMN_NAME, not following a final symlink) into out, NUL
// terminated. It returns the name's length, or -1.
static int aa_stored_name(const char *path, char *out, size_t size) {
	struct attrlist attrs;
	memset(&attrs, 0, sizeof(attrs));
	attrs.bitmapcount = ATTR_BIT_MAP_COUNT;
	attrs.commonattr = ATTR_CMN_NAME;
	struct {
		u_int32_t length;
		attrreference_t name;
		char data[1024 + 1];
	} __attribute__((aligned(4), packed)) buf;
	if (getattrlist(path, &attrs, &buf, sizeof(buf), FSOPT_NOFOLLOW) != 0) {
		return -1;
	}
	const char *start = (const char *)&buf.name + buf.name.attr_dataoffset;
	const char *end = (const char *)&buf + sizeof(buf);
	if (start < buf.data || buf.name.attr_length == 0 || buf.name.attr_length > (size_t)(end - start)) {
		return -1;
	}
	size_t n = strnlen(start, buf.name.attr_length);
	if (n == 0 || n >= size) {
		return -1;
	}
	memcpy(out, start, n);
	out[n] = 0;
	return (int)n;
}
*/
import "C"

import "unsafe"

// storedName asks the volume for the name path's last component is stored
// under: one call, however many entries share its directory. ok is false
// when the volume does not answer.
func storedName(path string) (name string, ok bool) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var out [1025]C.char
	n := C.aa_stored_name(cpath, &out[0], C.size_t(len(out)))
	if n <= 0 {
		return "", false
	}
	return C.GoStringN(&out[0], n), true
}
