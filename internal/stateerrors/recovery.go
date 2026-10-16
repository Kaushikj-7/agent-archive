// Package stateerrors owns the shared durable recovery error identity.
package stateerrors

import "errors"

// ErrDurableStorageRecovery preserves unknown or damaged obligations without native substitution.
var ErrDurableStorageRecovery = errors.New("durable publication evidence requires recovery; native substitution and automatic cleanup are forbidden")
