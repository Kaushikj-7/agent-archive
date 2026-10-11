// Package setupresources preserves bucket mutation evidence independently of setup drafts.
package setupresources

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Provider identifies the bucket service independently of its account binding.
type Provider string

// Supported bucket providers.
const (
	S3 Provider = "s3"
	R2 Provider = "r2"
)

// State is the last durably observed provider outcome.
type State string

// RequestSent is uncertain until a provider response is durably recorded.
const (
	RequestSent    State = "request-sent"
	Created        State = "created"
	Refused        State = "refused"
	Secured        State = "secured"
	Deleted        State = "deleted"
	CleanupPending State = "cleanup-pending"
)

// Receipt contains identifiers and evidence only, never credential values.
// Account is the Cloudflare account ID; Profile binds S3 to the creation profile,
// which is not evidence of an AWS account's identity.
type Receipt struct {
	Version      int       `json:"version"`
	ID           string    `json:"id"`
	Provider     Provider  `json:"provider"`
	Account      string    `json:"account,omitempty"`
	Profile      string    `json:"profile,omitempty"`
	Bucket       string    `json:"bucket"`
	Region       string    `json:"region,omitempty"`
	Jurisdiction string    `json:"jurisdiction,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	State        State     `json:"state"`
}

var s3BucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`)

const maxReceipts = 1000

func (r Receipt) validate() error {
	if r.Version != 1 || len(r.ID) != 32 || strings.Trim(r.ID, "0123456789abcdef") != "" || r.Bucket == "" || r.CreatedAt.IsZero() || (r.Provider != S3 && r.Provider != R2) {
		return errors.New("invalid setup resource receipt")
	}
	if !validBucket(r.Provider, r.Bucket) || !config.SafeMachineText(r.Profile, 256) || !config.SafeMachineText(r.Region, 64) || (r.Provider == R2 && (!config.ValidMachineID(r.Account) || r.Profile != "" || (r.Jurisdiction != "" && !cloudflare.ValidJurisdiction(r.Jurisdiction)))) || (r.Provider == S3 && (r.Account != "" || r.Jurisdiction != "")) {
		return errors.New("invalid setup resource binding")
	}
	switch r.State {
	case RequestSent, Created, Refused, Secured, CleanupPending, Deleted:
	default:
		return errors.New("invalid setup resource outcome")
	}
	return nil
}

// Begin records uncertainty before the provider mutation. Failure prevents the call.
func Begin(home string, r Receipt) (Receipt, error) {
	// Do not send another mutation while its recovery history is unreadable,
	// or add an intent that would make that history exceed the reader's cap.
	receipts, err := List(home)
	if err != nil {
		return Receipt{}, err
	}
	if len(receipts) >= maxReceipts {
		return Receipt{}, errors.New("too many setup resource receipts; preserve and reconcile the existing receipts before creating another bucket")
	}
	id, err := local.ID()
	if err != nil {
		return Receipt{}, err
	}
	r.ID, r.Version, r.State = id, 1, RequestSent
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	r.CreatedAt = r.CreatedAt.UTC()
	if err = r.validate(); err != nil {
		return Receipt{}, err
	}
	if err = local.Write(filepath.Join(home, "setup-resources", "bucket-"+id+".json"), r); err != nil {
		return r, err
	}
	// The first write creates setup-resources. Sync its parent too, so the
	// directory entry survives a machine crash before the provider call.
	parent, err := os.Open(home)
	if err != nil {
		return r, err
	}
	defer func() { _ = parent.Close() }()
	return r, parent.Sync()
}

// Finish records a provider outcome without changing the mutation's binding.
func Finish(home string, r Receipt, state State) error {
	path := filepath.Join(home, "setup-resources", "bucket-"+r.ID+".json")
	data, err := readReceipt(path)
	if err != nil {
		return err
	}
	var prior Receipt
	if len(data) > 16384 || json.Unmarshal(data, &prior) != nil || prior.validate() != nil {
		return errors.New("invalid setup resource receipt")
	}
	candidate := r
	candidate.State = prior.State
	if candidate != prior {
		return errors.New("setup resource binding changed")
	}
	allowed := prior.State == state || prior.State == RequestSent && (state == Created || state == Refused) || prior.State == Created && (state == Secured || state == CleanupPending || state == Deleted) || prior.State == Secured && (state == CleanupPending || state == Deleted) || prior.State == CleanupPending && state == Deleted
	if !allowed {
		return errors.New("setup resource transition refused")
	}
	r.State = state
	if err = r.validate(); err != nil {
		return err
	}
	return local.Write(path, r)
}

// List returns bounded, private, validated receipts. Missing directories mean no receipts.
func List(home string) ([]Receipt, error) {
	dir := filepath.Join(home, "setup-resources")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []Receipt
	for _, entry := range entries {
		// Atomic writes abandoned by a crashed process are not committed receipts.
		if strings.HasPrefix(entry.Name(), ".pending-") {
			continue
		}
		if len(result) >= maxReceipts {
			return nil, errors.New("too many setup resource receipts")
		}
		info, e := entry.Info()
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, errors.New("invalid setup resource receipt file")
		}
		data, e := readReceipt(filepath.Join(dir, entry.Name()))
		if e != nil {
			return nil, e
		}
		var r Receipt
		if len(data) > 16384 || json.Unmarshal(data, &r) != nil || r.validate() != nil || entry.Name() != "bucket-"+r.ID+".json" {
			return nil, errors.New("invalid setup resource receipt")
		}
		result = append(result, r)
	}
	return result, nil
}

// readReceipt bounds allocation before decoding untrusted local state.
func readReceipt(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, 16385))
}

func validBucket(provider Provider, name string) bool {
	if provider == R2 {
		return cloudflare.ValidateBucketName(name) == nil
	}
	return provider == S3 && len(name) >= 3 && len(name) <= 63 && s3BucketName.MatchString(name) && !strings.Contains(name, "..")
}
