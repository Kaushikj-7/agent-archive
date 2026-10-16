package local

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// CanonicalPath is one spelling for every spelling of a location: path made
// absolute, with its existing symlinks resolved (ResolveExistingSymlinks),
// and each existing component spelled the way its directory lists it, so a
// different case on a case-insensitive volume (APFS and HFS+ are by default)
// comes out the same too. The part of path that does not exist is kept as
// written, cleaned. It looks up each directory along the path, a few calls
// per component (listedSpelling), so it is for identities and candidate
// roots, not for every file of a scan.
func CanonicalPath(path string) string {
	resolved, err := ResolveExistingSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	volume := filepath.VolumeName(resolved)
	parts := strings.Split(strings.TrimPrefix(resolved[len(volume):], string(filepath.Separator)), string(filepath.Separator))
	current := volume + string(filepath.Separator)
	for i, part := range parts {
		if part == "" {
			continue
		}
		spelled, exists := listedSpelling(current, part)
		if !exists {
			return filepath.Join(append([]string{current}, parts[i:]...)...)
		}
		current = filepath.Join(current, spelled)
	}
	return current
}

// listedSpelling is name as dir lists it: name itself when dir holds an entry
// spelled exactly so, otherwise the entry that differs from it only in case
// and is the same file. exists is false when name is not in dir at all.
//
// It does not list dir unless it must: CanonicalPath runs on every parent of
// a path, and a parent such as $TMPDIR can hold tens of thousands of
// entries. A name no other case can spell is listed as written; macOS
// reports the stored name in one call (storedName); elsewhere, a directory
// where the name's other case does not reach the same file is case
// sensitive, so the exact spelling is the listed one. Only a case-folding
// directory without storedName, or a stored name that does not line up
// (another Unicode normalization), is listed.
func listedSpelling(dir, name string) (spelled string, exists bool) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return name, false
	}
	swapped := swapCase(name)
	if swapped == name {
		return name, true
	}
	if stored, ok := storedName(path); ok && strings.EqualFold(stored, name) {
		if stored == name || sameEntry(dir, stored, info) {
			return stored, true
		}
	} else if !ok && !sameEntry(dir, swapped, info) {
		return name, true
	}
	return listedCaseVariant(dir, name, info), true
}

// swapCase is name with every letter that has another case in it, so a
// lookup of it reaches name's file only where the directory folds case.
func swapCase(name string) string {
	return strings.Map(func(r rune) rune {
		if u := unicode.ToUpper(r); u != r {
			return u
		}
		return unicode.ToLower(r)
	}, name)
}

// sameEntry reports whether dir's entry name is the file info describes.
func sameEntry(dir, name string, info os.FileInfo) bool {
	other, err := os.Lstat(filepath.Join(dir, name))
	return err == nil && os.SameFile(info, other)
}

// listedCaseVariant is the slow path of listedSpelling: it reads all of dir.
func listedCaseVariant(dir, name string, info os.FileInfo) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return name
	}
	for _, e := range entries {
		if e.Name() == name {
			return name
		}
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), name) && sameEntry(dir, e.Name(), info) {
			return e.Name()
		}
	}
	return name
}

// SameLocation reports whether a and b name one location: the same
// CanonicalPath, or two existing paths that are the same file (os.SameFile),
// which also covers a spelling CanonicalPath cannot line up, such as another
// Unicode normalization of a name.
func SameLocation(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	if CanonicalPath(a) == CanonicalPath(b) {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// PathWithin reports whether path is root or lies beneath it. It compares
// the two lexically, after filepath.Clean: a trailing slash changes nothing,
// ".." is resolved against the path as written (so /a/b/../../c is not in
// /a), and names are compared exactly as spelled, case included. It does
// not touch the file system: a caller comparing file system locations
// resolves symlinks first (filepath.EvalSymlinks) and passes both resolved
// paths. Nothing lies within an empty root, and a relative path is never
// within an absolute root or the reverse.
func PathWithin(path, root string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
