// Package cursorformat owns pure Cursor format refusal values.
package cursorformat

import "fmt"

// Reason says why Cursor's database was not checked.
type Reason string

// Reasons a read of Cursor's database was not done.
const (
	// Locked means a rollback journal shows an unfinished write (which may be
	// a hot journal only Cursor can roll back) or that Cursor held a lock
	// past the busy timeout.
	Locked Reason = "locked"
	// Unreadable means the file is not a database SQLite can open, or its
	// side files are in a state that can't be read without changing them.
	Unreadable Reason = "unreadable"
	// UnknownFormat means the table, or a value read from it, is not a shape
	// this release knows.
	UnknownFormat Reason = "unknown_format"
	// ChangedDuringRead means Cursor wrote the file while it was read in
	// place with Cursor closed.
	ChangedDuringRead Reason = "changed_during_read"
)

// NotCheckedError is a read of Cursor's database that could not be done
// safely, or whose result can't be trusted. Nothing it read is returned.
type NotCheckedError struct {
	Reason Reason
	// Err is the underlying failure, when there is one.
	Err error
}

// Error names only the reason. The underlying error, reachable through
// Unwrap, can name the database's path (a path error from resolving it, or
// SQLite's own message), which never goes into a message that may be
// printed or recorded.
func (e *NotCheckedError) Error() string {
	return fmt.Sprintf("Cursor database not checked (%s)", e.Reason)
}

// Unwrap returns the underlying failure, or nil when there is none. It may
// name the database's path, so it is for errors.Is and errors.As, not for
// display.
func (e *NotCheckedError) Unwrap() error { return e.Err }
