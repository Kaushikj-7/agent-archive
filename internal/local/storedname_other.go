//go:build !darwin || !cgo

package local

// storedName has no cheap source without macOS's getattrlist (through cgo);
// listedSpelling probes the directory's case handling instead.
func storedName(string) (string, bool) {
	return "", false
}
