// Command cloudflare runs opt-in live acceptance against freshly created R2
// resources. It never opens agent-archive configuration, Keychain or schedulers.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/storage"
	"golang.org/x/term"
)

var revision = "unknown"

var errEvidencePersistence = errors.New("cannot persist sanitized acceptance evidence")

type check struct {
	Name         string `json:"name"`
	Result       string `json:"result"`
	Detail       string `json:"detail,omitempty"`
	Milliseconds int64  `json:"milliseconds,omitempty"`
}

type bucket struct {
	Name    string `json:"name"`
	Created bool   `json:"created"`
	Deleted bool   `json:"deleted"`
}

type issued struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

type report struct {
	Revision     string    `json:"revision"`
	RunID        string    `json:"run_id"`
	Started      time.Time `json:"started"`
	Platform     string    `json:"platform"`
	Account      string    `json:"account_id"`
	PermissionID string    `json:"permission_id,omitempty"`
	Buckets      []bucket  `json:"buckets"`
	Tokens       []issued  `json:"tokens"`
	Checks       []check   `json:"checks"`
	Pending      []string  `json:"pending"`
}

type runner struct {
	r                   report
	path                string
	api                 *cloudflare.Client
	token               string
	secrets             []string
	ctx                 context.Context
	stores              []*storage.S3Store
	bucketCleanupClient *http.Client
	cleaning            bool
	persistenceErr      error
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("secure randomness unavailable")
	}
	return hex.EncodeToString(b)
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	var cf *cloudflare.Error
	if errors.As(err, &cf) {
		return fmt.Sprintf("Cloudflare HTTP %d, codes %v", cf.Status, cf.Codes)
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) {
		return fmt.Sprintf("S3 HTTP %d", response.HTTPStatusCode())
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		return "S3 " + api.ErrorCode()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "request or response could not be verified"
}

func (r *runner) save() error {
	data, err := json.MarshalIndent(r.r, "", "  ")
	if err != nil {
		return err
	}
	for _, secret := range r.secrets {
		if secret != "" && strings.Contains(string(data), secret) {
			return errors.New("refused secret-bearing report")
		}
	}
	f, err := os.CreateTemp(filepath.Dir(r.path), ".acceptance-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), r.path)
}

func (r *runner) record(name, result, detail string, elapsed time.Duration) {
	r.r.Checks = append(r.r.Checks, check{name, result, detail, elapsed.Milliseconds()})
	fmt.Printf("%s: %s", strings.ToUpper(result), name)
	if detail != "" {
		fmt.Printf(" (%s)", detail)
	}
	fmt.Println()
	if err := r.save(); err != nil {
		r.persistenceErr = err
		if !r.cleaning {
			panic(errEvidencePersistence)
		}
	}
}

// execute stops acceptance on a journal failure, but always attempts cleanup.
func (r *runner) execute(run func()) {
	defer r.api.Discard()
	defer func() {
		if recovered := recover(); recovered != nil {
			err, ok := recovered.(error)
			if !ok || !errors.Is(err, errEvidencePersistence) {
				panic(recovered)
			}
		}
	}()
	defer r.cleanup()
	run()
	if err := r.ctx.Err(); err != nil {
		r.record("acceptance interrupted", "fail", safeError(err), 0)
	}
}

func (r *runner) failed() bool {
	if r.ctx.Err() != nil || r.persistenceErr != nil {
		return true
	}
	for _, c := range r.r.Checks {
		if c.Result == "fail" || c.Result == "pending" {
			return true
		}
	}
	return false
}

func (r *runner) attempt(name string, fn func(context.Context) error) bool {
	ctx, cancel := context.WithTimeout(r.ctx, 45*time.Second)
	defer cancel()
	started := time.Now()
	err := fn(ctx)
	result := "pass"
	if err != nil {
		result = "fail"
	}
	r.record(name, result, safeError(err), time.Since(started))
	return err == nil
}

func (r *runner) store(key cloudflare.S3Credentials, name string) *storage.S3Store {
	cfg := aws.Config{Region: "auto", Credentials: awscreds.NewStaticCredentialsProvider(key.AccessKeyID, key.SecretAccessKey, "")}
	s, err := storage.NewS3Store(storage.S3StoreOptions{Provider: "r2", Client: storage.NewClient(cfg, cloudflare.Endpoint(r.r.Account, ""), true, 1), Bucket: name})
	if err != nil {
		panic("invalid scratch storage configuration")
	}
	return s
}

func (r *runner) createToken() (cloudflare.S3Credentials, bool) {
	name := "agent-archive r=" + randomID() + " i=" + r.r.RunID + " k=" + randomID()
	r.r.Tokens = append(r.r.Tokens, issued{Name: name})
	if err := r.save(); err != nil {
		r.persistenceErr = err
		panic(errEvidencePersistence)
	}
	resource, _ := cloudflare.BucketResource(r.r.Account, cloudflare.BucketRef{Name: r.r.Buckets[0].Name})
	ctx, cancel := context.WithTimeout(r.ctx, 45*time.Second)
	defer cancel()
	t, err := r.api.CreateToken(ctx, r.r.Account, cloudflare.TokenSpec{Name: name, Policies: []cloudflare.Policy{{PermissionGroupIDs: []string{r.r.PermissionID}, Resources: map[string]string{resource: "*"}}}})
	r.r.Tokens[len(r.r.Tokens)-1].ID = t.ID
	r.secrets = append(r.secrets, t.Value)
	key := cloudflare.DeriveS3Credentials(t)
	r.secrets = append(r.secrets, key.SecretAccessKey)
	if err != nil {
		r.record("create canonical 118-byte bucket-scoped token", "fail", safeError(err), 0)
		return key, false
	}
	r.record("create canonical 118-byte bucket-scoped token", "pass", "one-time secret retained only in memory", 0)
	return key, true
}

func (r *runner) run() {
	r.attempt("account listing with supplied permissions", func(ctx context.Context) error { _, err := r.api.Accounts(ctx); return err })
	if !r.attempt("resolve selectable bucket-item-write permission", func(ctx context.Context) error {
		groups, err := r.api.PermissionGroups(ctx, r.r.Account, cloudflare.PermissionBucketItemWrite)
		if err != nil {
			return err
		}
		r.r.PermissionID, err = cloudflare.SelectPermissionGroup(groups, cloudflare.PermissionBucketItemWrite)
		return err
	}) {
		return
	}
	for i := range r.r.Buckets {
		if !r.attempt("create private scratch bucket "+r.r.Buckets[i].Name, func(ctx context.Context) error {
			err := r.api.CreateBucket(ctx, r.r.Account, cloudflare.BucketSpec{BucketRef: cloudflare.BucketRef{Name: r.r.Buckets[i].Name}})
			r.r.Buckets[i].Created = err == nil
			return err
		}) {
			return
		}
	}
	r.attempt("duplicate bucket produces expected conflict", func(ctx context.Context) error {
		err := r.api.CreateBucket(ctx, r.r.Account, cloudflare.BucketSpec{BucketRef: cloudflare.BucketRef{Name: r.r.Buckets[0].Name}})
		var cf *cloudflare.Error
		if errors.As(err, &cf) && cf.AlreadyExists() {
			return nil
		}
		if err == nil {
			return errors.New("duplicate creation unexpectedly succeeded")
		}
		return err
	})
	r.attempt("r2.dev access is disabled", func(ctx context.Context) error {
		d, err := r.api.ManagedDomain(ctx, r.r.Account, cloudflare.BucketRef{Name: r.r.Buckets[0].Name})
		if err == nil && d.Enabled {
			return errors.New("scratch bucket is public")
		}
		return err
	})
	r.attempt("custom-domain response is readable and empty", func(ctx context.Context) error {
		d, err := r.api.CustomDomains(ctx, r.r.Account, cloudflare.BucketRef{Name: r.r.Buckets[0].Name})
		if err == nil && len(d) != 0 {
			return errors.New("scratch bucket has custom domains")
		}
		return err
	})
	var firstKey cloudflare.S3Credentials
	for i := range 3 {
		key, ok := r.createToken()
		if !ok {
			return
		}
		if i == 0 {
			firstKey = key
		}
		r.stores = append(r.stores, r.store(key, r.r.Buckets[0].Name))
		id := key.AccessKeyID
		r.attempt("provider details verify immutable name, exact bucket policy and no expiry", func(ctx context.Context) error {
			t, err := r.api.TokenDetails(ctx, r.r.Account, id)
			if err != nil {
				return err
			}
			if t.Name != r.r.Tokens[len(r.r.Tokens)-1].Name || t.ExpiresOn != "" || !cloudflare.ExactBucketPolicy(t, r.r.Account, cloudflare.BucketRef{Name: r.r.Buckets[0].Name}, r.r.PermissionID) {
				return errors.New("token policy/name/expiry did not match")
			}
			return nil
		})
	}
	r.verifyStorage(firstKey)
}

func (r *runner) verifyStorage(firstKey cloudflare.S3Credentials) {
	first := r.stores[0]
	activation := time.Now()
	active := false
	for attempt := 0; attempt < 15 && r.ctx.Err() == nil; attempt++ {
		ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
		err := storage.Probe(ctx, first)
		cancel()
		if err == nil {
			active = true
			break
		}
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	if !active {
		r.record("lowercase-hex SHA256 key derivation and activation", "fail", "probe never succeeded within bounded retries", time.Since(activation))
		return
	}
	r.record("lowercase-hex SHA256 key derivation and activation", "pass", "observed after three sequential token creations; not exact mint-to-activation latency", time.Since(activation))
	r.attempt("production storage probe/write/read/list/delete/missing round trip", func(ctx context.Context) error { return storage.VerifyAccess(ctx, first) })
	r.attempt("same key works outside archive prefix (bucket-level scope)", func(ctx context.Context) error {
		key := "outside-archive/synthetic-" + r.r.RunID
		if err := first.Put(ctx, key, []byte("synthetic acceptance data")); err != nil {
			return err
		}
		defer func() {
			// Final cleanup retries this namespace if this deletion fails.
			_ = deleteScratchObject(ctx, first, key, 45*time.Second)
		}()
		_, err := first.Get(ctx, key)
		return err
	})
	other := r.store(firstKey, r.r.Buckets[1].Name)
	r.attempt("bucket-scoped key is refused by second scratch bucket", func(ctx context.Context) error {
		_, err := other.ListPage(ctx, ".setup-test/", "", 1)
		var response *smithyhttp.ResponseError
		if errors.As(err, &response) && response.HTTPStatusCode() == 403 {
			return nil
		}
		if err == nil {
			return errors.New("key unexpectedly reached control bucket")
		}
		return err
	})
	r.attempt("bounded token inventory includes all three new tokens", func(ctx context.Context) error {
		inventory, err := r.api.TokenInventory(ctx, r.r.Account)
		if err != nil {
			return err
		}
		if !inventory.PaginationComplete {
			return errors.New("pagination incomplete")
		}
		for _, wanted := range r.r.Tokens {
			found := false
			for _, t := range inventory.Tokens {
				if t.ID == wanted.ID && t.Name == wanted.Name {
					found = true
				}
			}
			if !found {
				return errors.New("new token missing from visible inventory")
			}
		}
		return nil
	})
	r.verifyRevocation()
}

// deleteScratchObject permits cleanup after interruption, but bounds response-body reads.
func deleteScratchObject(ctx context.Context, store *storage.S3Store, key string, timeout time.Duration) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return store.Delete(cleanupCtx, key)
}

func (r *runner) verifyRevocation() {
	first := r.stores[0]
	marker := "revocation/synthetic-" + r.r.RunID
	if !r.attempt("revocation test object is readable with recipient key", func(ctx context.Context) error {
		if err := r.stores[1].Put(ctx, marker, []byte("synthetic acceptance data")); err != nil {
			return err
		}
		_, err := r.stores[1].Get(ctx, marker)
		return err
	}) {
		return
	}
	if !r.attempt("delete recipient token", func(ctx context.Context) error {
		err := r.api.DeleteToken(ctx, r.r.Account, r.r.Tokens[1].ID)
		if err == nil {
			r.r.Tokens[1].Deleted = true
		}
		return err
	}) {
		return
	}
	cutoff := time.Now()
	denied := false
	for attempt := 0; attempt < 30 && r.ctx.Err() == nil; attempt++ {
		ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
		_, err := r.stores[1].Get(ctx, marker)
		cancel()
		var response *smithyhttp.ResponseError
		if errors.As(err, &response) && (response.HTTPStatusCode() == 401 || response.HTTPStatusCode() == 403) {
			denied = true
			break
		}
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	result := "pending"
	if denied {
		result = "pass"
	}
	r.record("revoked recipient key is denied on previously readable object", result, "observed data-plane propagation; not an immediate-cutoff guarantee", time.Since(cutoff))
	r.attempt("another recipient remains active after independent revocation", func(ctx context.Context) error { _, err := first.Get(ctx, marker); return err })
	r.attempt("remove synthetic revocation object with still-active key", func(ctx context.Context) error { return first.Delete(ctx, marker) })
	ctx, cancel := context.WithTimeout(r.ctx, 20*time.Second)
	err := r.api.DeleteToken(ctx, r.r.Account, r.r.Tokens[1].ID)
	cancel()
	detail := "second delete succeeded"
	if err != nil {
		detail = safeError(err)
	}
	r.record("already-deleted token response (observation only)", "observed", detail, 0)
}

func (r *runner) cleanup() {
	r.cleaning = true
	defer func() { r.cleaning = false }()
	fmt.Println("Cleaning only resources created by this run; recovery metadata is retained.")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Remove only synthetic object namespaces in this run's newly created bucket.
	if len(r.stores) > 0 {
		for _, prefix := range []string{".setup-test/", "outside-archive/", "revocation/"} {
			objects, err := r.stores[0].List(ctx, prefix)
			if err == nil {
				for _, obj := range objects {
					_ = r.stores[0].Delete(ctx, obj.Key)
				}
			}
		}
	}
	for i := range r.r.Tokens {
		t := &r.r.Tokens[i]
		if t.Deleted {
			continue
		}
		if t.ID == "" {
			inventory, err := r.api.TokenInventory(ctx, r.r.Account)
			if err == nil && inventory.PaginationComplete {
				for _, found := range inventory.Tokens {
					if found.Name == t.Name && cloudflare.ExactBucketPolicy(found, r.r.Account, cloudflare.BucketRef{Name: r.r.Buckets[0].Name}, r.r.PermissionID) {
						if t.ID != "" {
							t.ID = ""
							break
						}
						t.ID = found.ID
					}
				}
			}
		}
		if t.ID == "" {
			r.record("cleanup ambiguous token creation", "pending", "inspect exact name in report; no guessed deletion", 0)
			continue
		}
		err := r.api.DeleteToken(ctx, r.r.Account, t.ID)
		t.Deleted = err == nil
		result := "pass"
		if err != nil {
			result = "pending"
		}
		r.record("cleanup issued token "+t.ID, result, safeError(err), 0)
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if r.bucketCleanupClient != nil {
		client = r.bucketCleanupClient
	}
	for i := range r.r.Buckets {
		b := &r.r.Buckets[i]
		if !b.Created || b.Deleted {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, cloudflare.DefaultBaseURL+"/accounts/"+r.r.Account+"/r2/buckets/"+b.Name, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+r.token)
		response, err := client.Do(req)
		result, detail := "pending", safeError(err)
		if err == nil {
			var envelope struct {
				Success bool `json:"success"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope)
			_ = response.Body.Close()
			b.Deleted = response.StatusCode >= 200 && response.StatusCode < 300 && decodeErr == nil && envelope.Success
			detail = fmt.Sprintf("HTTP %d", response.StatusCode)
			if b.Deleted {
				result = "pass"
			}
		}
		r.record("cleanup scratch bucket "+b.Name, result, detail, 0)
	}
}

func main() {
	dry := flag.Bool("dry-run", false, "describe scope without credentials or network requests")
	output := flag.String("output", "", "sanitized JSON report (must not exist)")
	flag.Parse()
	if *dry {
		fmt.Println("Uses hidden token prompt; creates two uniquely named private scratch buckets and three bucket-scoped tokens; exercises production provider/storage paths; revokes only generated tokens; deletes only generated buckets; saves nonsecret evidence. No hooks, scheduler, archive config or Keychain access. No network calls in dry-run.")
		return
	}
	if *output == "" {
		fmt.Fprintln(os.Stderr, "--output PATH is required")
		os.Exit(2)
	}
	if _, err := os.Lstat(*output); !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "report path exists or cannot be checked; choose a new path")
		os.Exit(2)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "run this acceptance binary in a human terminal; token input must be hidden")
		os.Exit(2)
	}
	fmt.Println("Cloudflare acceptance: two new private scratch buckets, three temporary keys, synthetic data, automatic cleanup.")
	fmt.Print("Cloudflare account ID (32 lowercase hex): ")
	account, err := bufio.NewReader(os.Stdin).ReadString('\n')
	account = strings.TrimSpace(account)
	if err != nil || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(account) {
		fmt.Fprintln(os.Stderr, "invalid account ID")
		os.Exit(2)
	}
	fmt.Print("Cloudflare management API token (hidden): ")
	secret, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil || len(secret) == 0 || len(secret) > 4096 || !regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`).Match(secret) {
		clear(secret)
		fmt.Fprintln(os.Stderr, "could not read a valid token")
		os.Exit(2)
	}
	token := strings.TrimSpace(string(secret))
	clear(secret)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	id := randomID()
	base := "aa-accept-" + time.Now().UTC().Format("20060102") + "-" + id[:12]
	r := &runner{path: *output, token: token, api: cloudflare.New(token, cloudflare.Options{}), secrets: []string{token}, ctx: ctx, r: report{Revision: revision, RunID: id, Started: time.Now().UTC(), Platform: string(platform.Current()) + "/" + runtime.GOARCH, Account: account, Buckets: []bucket{{Name: base}, {Name: base + "-control"}}, Pending: []string{"actual guided setup and paired CLI transaction", "exact two-permission token provenance (operator confirmation)", "r2.dev enabled-warning interaction", "eu/us/fedramp jurisdiction acceptance", "non-administrator member permissions", "R2-disabled account", "429 Retry-After observation (do not intentionally exhaust limits)", "multi-page and account-wide inventory completeness", "token creation rate limit", "prefix-scoped token-creation restrictions", "real macOS/Linux onboarding timings", "full interruption/concurrency/issuer-compromise matrix"}}}
	if r.save() != nil {
		fmt.Fprintln(os.Stderr, "cannot create private report; no provider work started")
		os.Exit(1)
	}
	r.execute(r.run)
	if r.persistenceErr != nil {
		fmt.Fprintln(os.Stderr, "cannot persist complete sanitized acceptance evidence; cleanup was attempted for all tracked resources")
	} else {
		fmt.Println("Sanitized evidence:", r.path)
	}
	if r.failed() {
		os.Exit(1)
	}
}
