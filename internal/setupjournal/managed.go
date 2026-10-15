package setupjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// Intent is the lifecycle effect of one setup transaction on a managed job.
type Intent string

const (
	// Keep changes files without consulting or changing the manager.
	Keep Intent = "keep"
	// EnsureRunning starts the job after applying its definition.
	EnsureRunning Intent = "ensure_running"
	// RestartIfLoaded refreshes a definition without enabling a missing job.
	RestartIfLoaded Intent = "restart_if_loaded"
	// EnsureStopped disables a previously managed job.
	EnsureStopped Intent = "ensure_stopped"
)

// ManagedRole names one of the two finite lifecycle records.
type ManagedRole string

const (
	// CollectorRole is the original publication job.
	CollectorRole ManagedRole = "collector"
	// SearchRole is the optional local content indexing job.
	SearchRole ManagedRole = "search"
)

// ManagedJob records one of the installation's two owned jobs.
type ManagedJob struct {
	Role       ManagedRole `json:"role"`
	Backend    string      `json:"backend"`
	Ref        string      `json:"ref"`
	Definition string      `json:"definition"`
	WasLoaded  bool        `json:"was_loaded"`
	Intent     Intent      `json:"intent"`
}

type journalJSON Journal

type journalV2 struct {
	Version int `json:"schema_version"`
	Changes *struct {
		Files []fileapply.Change `json:"files"`
	} `json:"changes"`
	Jobs          []ManagedJob `json:"jobs"`
	Legacy        *LegacyJob   `json:"legacy,omitempty"`
	Relabeled     *LegacyJob   `json:"relabeled,omitempty"`
	MoreRelabeled []*LegacyJob `json:"more_relabeled,omitempty"`
}

// MarshalJSON preserves the legacy wire and fences old readers before actions.
func (j Journal) MarshalJSON() ([]byte, error) {
	if len(j.Jobs) == 0 {
		return json.Marshal(journalJSON(j))
	}
	if err := validateJobs(j.Jobs); err != nil {
		return nil, err
	}
	if j.Changes == nil {
		j.Changes = []fileapply.Change{}
	}
	return json.Marshal(journalV2{Version: 2, Changes: &struct {
		Files []fileapply.Change `json:"files"`
	}{Files: j.Changes}, Jobs: j.Jobs, Legacy: j.Legacy, Relabeled: j.Relabeled, MoreRelabeled: j.MoreRelabeled})
}

// UnmarshalJSON refuses future or malformed lifecycle records before recovery.
func (j *Journal) UnmarshalJSON(data []byte) error {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	if _, ok := shape["schema_version"]; !ok {
		if _, ok := shape["jobs"]; ok {
			return errors.New("managed jobs require journal version 2")
		}
		var legacy journalJSON
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		*j = Journal(legacy)
		return nil
	}
	if err := checkJournalKeys(json.NewDecoder(bytes.NewReader(data)), 0); err != nil {
		return err
	}
	var wire journalV2
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if wire.Version != 2 || wire.Changes == nil {
		return errors.New("unsupported setup journal version or changes shape")
	}
	var changes map[string]json.RawMessage
	if err := json.Unmarshal(shape["changes"], &changes); err != nil {
		return err
	}
	if rawFiles, ok := changes["files"]; !ok || bytes.Equal(bytes.TrimSpace(rawFiles), []byte("null")) {
		return errors.New("journal changes requires files")
	}
	if err := validateJobs(wire.Jobs); err != nil {
		return err
	}
	*j = Journal{Changes: wire.Changes.Files, Jobs: wire.Jobs, Legacy: wire.Legacy, Relabeled: wire.Relabeled, MoreRelabeled: wire.MoreRelabeled}
	return nil
}

func validateJobs(jobs []ManagedJob) error {
	if len(jobs) == 0 || len(jobs) > 2 {
		return errors.New("setup journal requires one or two managed roles")
	}
	seen := map[ManagedRole]bool{}
	search := false
	for _, job := range jobs {
		if (job.Role != CollectorRole && job.Role != SearchRole) || seen[job.Role] {
			return errors.New("invalid or duplicate managed role")
		}
		seen[job.Role] = true
		search = search || job.Role == SearchRole
		if job.Backend == "" || job.Ref == "" || job.Definition == "" {
			return errors.New("managed role requires backend, ref and definition")
		}
		switch job.Intent {
		case Keep, EnsureRunning, RestartIfLoaded, EnsureStopped:
		default:
			return errors.New("unsupported managed lifecycle intent")
		}
	}
	if !search {
		return errors.New("version 2 requires a managed search role")
	}
	return nil
}

type managedTarget struct {
	ManagedJob
	target target
	state  scheduler.JobState
}

func (j managedTarget) mutates() bool {
	return j.Intent != Keep && (j.Intent != RestartIfLoaded || j.WasLoaded)
}

func (j managedTarget) starts() bool {
	return j.Intent == EnsureRunning || j.Intent == RestartIfLoaded && j.WasLoaded
}

// stateError preserves legacy recovery while guarding both managed roles.
func (j managedTarget) stateError(home string, guarded bool) error {
	switch j.state {
	case scheduler.Unknown:
		if !guarded {
			return &RecoveryBlockedError{home: home, cause: "the background collector's state is unknown, so recovery cannot safely continue; restore access to " + j.target.tool() + " and rerun setup"}
		}
	case scheduler.AnotherInstallation:
		if !guarded {
			return nil
		}
	default:
		return nil
	}
	return fmt.Errorf("background %s state is %s; restore access to %s and rerun setup", j.Role, j.state, j.target.tool())
}

func (b Backends) managed(home string, j Journal, recovering bool) ([]managedTarget, error) {
	records := j.Jobs
	if len(records) == 0 {
		intent := EnsureRunning
		if j.FilesOnly {
			intent = Keep
		}
		records = []ManagedJob{{Role: CollectorRole, Backend: j.Backend, Ref: j.JobRef, Definition: j.Plist, WasLoaded: j.WasLoaded, Intent: intent}}
	} else if err := validateJobs(records); err != nil {
		return nil, err
	}
	// Resolve every recorded target before the first manager question. Legacy
	// commits retain their original no-Inspect behavior; their caller preflights.
	if len(j.Jobs) == 0 && !j.FilesOnly {
		if _, err := b.resolve(j); err != nil {
			return nil, err
		}
	}
	for _, retired := range j.retired() {
		if retired.WasLoaded && (!j.FilesOnly || len(j.Jobs) > 0) {
			t := b.target(retired.Backend, retired.JobRef, retired.Change.Path)
			if t.err != nil {
				return nil, fmt.Errorf("the job %s: %w", retired.Change.Path, t.err)
			}
		}
	}
	var jobs []managedTarget
	for _, record := range records {
		job := managedTarget{ManagedJob: record}
		if !job.mutates() && len(j.Jobs) == 0 {
			jobs = append(jobs, job)
			continue
		}
		resolved, err := b.resolveManaged(home, record, len(j.Jobs) > 0)
		if err != nil {
			return nil, err
		}
		job.target = resolved

		jobs = append(jobs, job)
	}
	// Every role and retired target has resolved before the first state query.
	for i := range jobs {
		job := &jobs[i]
		if !job.mutates() || len(j.Jobs) == 0 && !recovering {
			continue
		}
		job.state = job.target.state()
		if err := job.stateError(home, len(j.Jobs) > 0); err != nil {
			return nil, err
		}
	}
	if j.FilesOnly && len(j.Jobs) == 0 {
		return jobs, nil
	}
	for _, retired := range j.retired() {
		if retired.WasLoaded {
			t := b.target(retired.Backend, retired.JobRef, retired.Change.Path)
			if t.err != nil {
				return nil, t.err
			}
			if len(j.Jobs) > 0 && t.state() == scheduler.Unknown {
				return nil, errors.New("retired job ownership is unknown")
			}
		}
	}
	return jobs, nil
}

func (b Backends) resolveManaged(home string, record ManagedJob, guarded bool) (target, error) {
	resolved := b.target(record.Backend, record.Ref, record.Definition)
	if resolved.err != nil {
		return target{}, resolved.err
	}
	if guarded {
		role := scheduler.Collector
		if record.Role == SearchRole {
			role = scheduler.Search
		}
		own := resolved.sched.Ref(scheduler.Installation{DataHome: local.CanonicalPath(home), Role: role})
		defaultRef := resolved.sched.Ref(scheduler.Installation{DataHome: local.CanonicalPath(home), Default: true, Role: role})
		if resolved.ref != own && resolved.ref != defaultRef {
			return target{}, fmt.Errorf("%s ref does not belong to this installation", record.Role)
		}
	}
	return resolved, nil
}

// checkJournalKeys rejects ambiguous authority fields before decoding v2 values.
func checkJournalKeys(d *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("setup journal nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			// JSON field matching also folds Unicode aliases of ASCII letters.
			folded := strings.ToLower(strings.ToUpper(name))
			if !ok || seen[folded] {
				return errors.New("duplicate setup journal field")
			}
			seen[folded] = true
			if err = checkJournalKeys(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = checkJournalKeys(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid setup journal container")
	}
	_, err = d.Token()
	return err
}
