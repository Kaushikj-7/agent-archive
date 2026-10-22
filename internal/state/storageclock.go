package state

import (
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// storageClockFile records the retention sweep's last reading of the storage
// service's clock, so a sweep that keeps finding something due (a clock that
// stays ahead, a probe that keeps failing) does not write, list, and delete a
// probe object on every pass.
const storageClockFile = "storage-clock.json"

// StorageClockReading is one reading of the storage service's clock: this
// machine's time when it was taken, and the service's time then, or why it could
// not be read. It is a cache; retention decides how long one may stand in
// for a new reading.
type StorageClockReading struct {
	CheckedAt time.Time `json:"checked_at"`
	ServerAt  time.Time `json:"server_at,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// LoadStorageClockReading returns the last saved reading. found is false
// when there is none, or when the file cannot be read or decoded: a reading
// is only ever a cache, and without one the clock is read again.
func (s *Store) LoadStorageClockReading() (reading StorageClockReading, found bool) {
	reading, found, err := readJSON[StorageClockReading](s, filepath.Join(s.home, storageClockFile))
	if err != nil || reading.CheckedAt.IsZero() {
		return StorageClockReading{}, false
	}
	return reading, found
}

// SaveStorageClockReading records a reading, replacing the previous one.
func (s *Store) SaveStorageClockReading(reading StorageClockReading) error {
	return local.Write(filepath.Join(s.home, storageClockFile), reading)
}
