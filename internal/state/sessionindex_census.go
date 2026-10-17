package state

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Read-ahead is restricted to small registration documents. Larger documents
// use the existing serial reader, preserving support without multiplying their
// unbounded size by worker count. Results are consumed in directory order.
const recoveryReadAheadBytes = 64 * 1024

const recoveryReadAheadWorkers = 32

var errRecoveryReadAheadLarge = errors.New("registration exceeds bounded read-ahead")

type recoveryRegistrationRead struct {
	reg archive.SessionRegistration
	err error
}

// A reader belongs to one census only. Workers keep their bounded buffers, but
// decoded registrations and errors are cleared before each ordered chunk.
// close joins every worker, including on an early census error or cancellation.
type recoveryRegistrationReader struct {
	jobs    chan recoveryRegistrationReadJob
	pending sync.WaitGroup
	workers sync.WaitGroup
	results [recoveryReadAheadWorkers]recoveryRegistrationRead
}

type recoveryRegistrationReadJob struct {
	entry  os.DirEntry
	result *recoveryRegistrationRead
}

func (s *Store) newRecoveryRegistrationReader() *recoveryRegistrationReader {
	r := &recoveryRegistrationReader{jobs: make(chan recoveryRegistrationReadJob, recoveryReadAheadWorkers)}
	r.workers.Add(recoveryReadAheadWorkers)
	for range recoveryReadAheadWorkers {
		go func() {
			defer r.workers.Done()
			var data bytes.Buffer
			for job := range r.jobs {
				data.Reset()
				if data.Cap() > recoveryReadAheadBytes {
					data = bytes.Buffer{}
				}
				file, err := os.Open(s.registrationPath(strings.TrimSuffix(job.entry.Name(), ".json")))
				if err == nil {
					_, err = data.ReadFrom(io.LimitReader(file, recoveryReadAheadBytes+1))
					_ = file.Close()
					if err == nil {
						if data.Len() > recoveryReadAheadBytes {
							err = errRecoveryReadAheadLarge
						} else {
							err = archive.DecodeSessionRegistration(data.Bytes(), &job.result.reg)
						}
					}
				}
				job.result.err = err
				r.pending.Done()
			}
		}()
	}
	return r
}

func (r *recoveryRegistrationReader) readChunk(entries []os.DirEntry) []recoveryRegistrationRead {
	clear(r.results[:])
	for i, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		r.pending.Add(1)
		r.jobs <- recoveryRegistrationReadJob{entry: entry, result: &r.results[i]}
	}
	r.pending.Wait()
	return r.results[:len(entries)]
}

func (r *recoveryRegistrationReader) close() {
	close(r.jobs)
	r.workers.Wait()
}
