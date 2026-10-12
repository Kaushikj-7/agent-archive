package local

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// CollectorGuard proves ownership of the actual collector.lock inode. Its
// operation fence joins admitted publication work before releasing the flock.
// A copied or replaced state directory cannot recover an earlier origin.
type CollectorGuard struct {
	mu        sync.Mutex
	joined    *sync.Cond
	file      *os.File
	path      string
	principal string
	origin    string
	held      bool
	active    int
	claims    map[string]bool
	unlock    func()
}

// LockCollectorGuard acquires the existing collector lock without waiting.
// The durable principal and inode bind subsequent crash recovery to this home.
func LockCollectorGuard(home string) (*CollectorGuard, error) {
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, "collector.lock")
	for range lockAttempts {
		file, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return nil, e
		}
		unlock, current, e := lockOpened(path, file)
		if e != nil {
			return nil, e
		}
		if !current {
			continue
		}
		guard, e := newCollectorGuard(home, path, file, unlock)
		if e != nil {
			unlock()
		}
		return guard, e
	}
	return nil, errors.New("collector lock changed during acquisition")
}

func newCollectorGuard(home, path string, file *os.File, unlock func()) (*CollectorGuard, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	named, err := os.Lstat(path)
	if err != nil || !named.Mode().IsRegular() || !os.SameFile(info, named) {
		return nil, errors.New("collector lock origin changed")
	}
	principalPath := filepath.Join(home, "collector-principal.json")
	var principal string
	if err := readGuardRecord(principalPath, &principal); errors.Is(err, os.ErrNotExist) {
		principal, err = ID()
		if err != nil {
			return nil, err
		}
		if err = Write(principalPath, principal); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	decoded, err := hex.DecodeString(principal)
	if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != principal {
		return nil, errors.New("invalid collector principal")
	}
	if err = errors.Join(file.Sync(), syncGuardDirectory(home)); err != nil {
		return nil, err
	}
	inode, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("collector lock identity unavailable")
	}
	digest := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s", home, inode.Dev, inode.Ino, principal))
	g := &CollectorGuard{file: file, path: path, principal: principal, origin: hex.EncodeToString(digest[:]), held: true, unlock: unlock}
	g.joined = sync.NewCond(&g.mu)
	return g, nil
}

// Origin returns the exact durable origin only while the actual lock is held.
func (g *CollectorGuard) Origin() (string, error) {
	if g == nil {
		return "", ErrBusy
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.validate(); err != nil {
		return "", err
	}
	return g.origin, nil
}

// Home returns the guarded state directory only while its lock is held.
func (g *CollectorGuard) Home() (string, error) {
	if _, err := g.Origin(); err != nil {
		return "", err
	}
	return filepath.Dir(g.path), nil
}

func (g *CollectorGuard) validate() error {
	if !g.held {
		return ErrBusy
	}
	opened, err := g.file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(g.path)
	if err != nil || !os.SameFile(opened, named) || !named.Mode().IsRegular() {
		return errors.New("collector lock origin changed")
	}
	var principal string
	if err = readGuardRecord(filepath.Join(filepath.Dir(g.path), "collector-principal.json"), &principal); err != nil || principal != g.principal {
		return errors.New("collector principal changed")
	}
	return nil
}

// BeginOperation joins one exact publication invocation to the lock lifetime.
// Its returned release must run after every subordinate operation has joined.
func (g *CollectorGuard) BeginOperation(origin string) (func(), error) {
	if g == nil {
		return nil, ErrBusy
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.validate(); err != nil {
		return nil, err
	}
	if origin != g.origin {
		return nil, errors.New("foreign collector journal origin")
	}
	g.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.active--
			g.joined.Broadcast()
		})
	}, nil
}

// ClaimJournal excludes parallel invocations of one durable journal even when
// separately constructed destination wrappers share this actual lock guard.
func (g *CollectorGuard) ClaimJournal(origin, owner string) (func(), error) {
	done, err := g.BeginOperation(origin)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.claims[owner] {
		g.mu.Unlock()
		done()
		return nil, ErrBusy
	}
	if g.claims == nil {
		g.claims = map[string]bool{}
	}
	g.claims[owner] = true
	g.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { g.mu.Lock(); delete(g.claims, owner); g.mu.Unlock(); done() }) }, nil
}

// Release closes new operations, joins existing work, and releases the flock.
func (g *CollectorGuard) Release() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.held {
		return
	}
	g.held = false
	for g.active != 0 {
		g.joined.Wait()
	}
	g.unlock()
}
