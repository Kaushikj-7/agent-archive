package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go"
)

// bucketCall is one request the fake S3 server received.
type bucketCall struct {
	method string
	path   string
	query  string
	body   string
}

// bucketRecorder answers each request with reply and records every request, so a
// test can say what was and was not sent.
type bucketRecorder struct {
	mu    sync.Mutex
	calls []bucketCall
	reply func(bucketCall, http.ResponseWriter)
}

func (f *bucketRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	call := bucketCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body)}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	f.reply(call, w)
}

func (f *bucketRecorder) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, call := range f.calls {
		out = append(out, call.method+" "+call.path+"?"+call.query)
	}
	return out
}

func replyS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>synthetic</Message><RequestId>r</RequestId></Error>`, code)
}

// adminIn is a BucketAdmin whose client is in region and talks to server for
// every service, as AWS_ENDPOINT_URL (the global override) makes it.
func adminIn(server *httptest.Server, region string) *BucketAdmin {
	cfg := aws.Config{
		Region:       region,
		Credentials:  awscredentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		BaseEndpoint: aws.String(server.URL),
	}
	return NewBucketAdmin(NewClient(cfg, server.URL, true, 1), cfg)
}

// bucketAnswers scripts the fake server: how it answers the HEAD request
// (name check), the STS call (credentials check), and the PUT that creates
// the bucket. A nil answer is a 404 for HEAD, a caller identity for STS, and
// 200 for PUT.
type bucketAnswers struct {
	head func(http.ResponseWriter)
	sts  func(http.ResponseWriter)
	put  func(http.ResponseWriter)
}

func status(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

func apiError(code int, name string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { replyS3Error(w, code, name) }
}

// stsRefuses answers the credentials check as STS answers a key it does not
// recognize.
func stsRefuses(code string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>%s</Code><Message>synthetic</Message></Error><RequestId>r</RequestId></ErrorResponse>`, code)
	}
}

func stsIdentity(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/xml")
	_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:iam::111111111111:user/synthetic</Arn><UserId>AIDAEXAMPLE</UserId><Account>111111111111</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
}

func (a bucketAnswers) reply(c bucketCall, w http.ResponseWriter) {
	answer := func(f, fallback func(http.ResponseWriter)) {
		if f == nil {
			f = fallback
		}
		f(w)
	}
	switch {
	case c.method == http.MethodHead:
		answer(a.head, status(http.StatusNotFound))
	case c.method == http.MethodPost && strings.Contains(c.body, "GetCallerIdentity"):
		answer(a.sts, stsIdentity)
	default:
		answer(a.put, status(http.StatusOK))
	}
}

// createWith runs CreateBucket for a bucket in region against a fake server
// that answers as answers says, and returns the error and the requests made.
func createWith(t *testing.T, region string, answers bucketAnswers) (requests []string, err error) {
	t.Helper()
	fake := &bucketRecorder{reply: answers.reply}
	server := httptest.NewServer(fake)
	defer server.Close()
	err = adminIn(server, region).CreateBucket(context.Background(), "agent-archive-1", region)
	return fake.methods(), err
}

func TestCreateBucketChecksTheNameThenCreatesWithTheRightConstraint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		region     string
		constraint string
	}{
		{"us-east-1", ""},
		{"eu-west-2", "<LocationConstraint>eu-west-2</LocationConstraint>"},
	} {
		fake := &bucketRecorder{reply: bucketAnswers{}.reply}
		server := httptest.NewServer(fake)
		err := adminIn(server, tc.region).CreateBucket(context.Background(), "agent-archive-1", tc.region)
		server.Close()
		if err != nil {
			t.Fatalf("%s: %v", tc.region, err)
		}
		want := []string{"HEAD /agent-archive-1/?", "PUT /agent-archive-1/?"}
		if got := fake.methods(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: requests %q, want %q (the name is checked first in every region)", tc.region, got, want)
		}
		body := fake.calls[1].body
		if tc.constraint == "" && strings.Contains(body, "LocationConstraint") {
			t.Errorf("us-east-1 must not send a location constraint (S3 refuses one), got %q", body)
		}
		if tc.constraint != "" && !strings.Contains(body, tc.constraint) {
			t.Errorf("%s: body %q lacks %s", tc.region, body, tc.constraint)
		}
	}
}

func TestCreateBucketNameInUseIsTakenAndCreatesNothingMoreThanNeeded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		region  string
		answers bucketAnswers
		puts    int
	}{
		{"name owned by this account", "us-east-1", bucketAnswers{head: status(http.StatusOK)}, 0},
		{"name owned by another account", "us-east-1", bucketAnswers{head: status(http.StatusForbidden)}, 0},
		{"name in another region", "us-east-1", bucketAnswers{head: status(http.StatusMovedPermanently)}, 0},
		{"same name owned in us-east-1, creating elsewhere", "eu-west-1", bucketAnswers{head: status(http.StatusMovedPermanently)}, 0},
		{"BucketAlreadyExists after a free check", "eu-west-1", bucketAnswers{put: apiError(http.StatusConflict, "BucketAlreadyExists")}, 1},
		{"BucketAlreadyOwnedByYou after a free check", "eu-west-1", bucketAnswers{put: apiError(http.StatusConflict, "BucketAlreadyOwnedByYou")}, 1},
		{"OperationAborted after a free check", "eu-west-1", bucketAnswers{put: apiError(http.StatusConflict, "OperationAborted")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			requests, err := createWith(t, tc.region, tc.answers)
			if !errors.Is(err, ErrBucketNameTaken) {
				t.Fatalf("err = %v, want ErrBucketNameTaken", err)
			}
			puts := 0
			for _, request := range requests {
				if strings.HasPrefix(request, "PUT") {
					puts++
				}
			}
			if puts != tc.puts {
				t.Fatalf("%d PUTs, want %d: %q", puts, tc.puts, requests)
			}
		})
	}
}

// A refused key gets the same bare 403 to a HEAD request as another
// account's bucket does; it must read as a credential problem, never as a
// taken name, or setup would say "taken" for every name.
func TestCreateBucketRefusedCredentialsAreNotATakenName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		code string
		want string
	}{
		{"InvalidClientTokenId", "The storage provider doesn't recognize the access key, or its secret is wrong."},
		{"SignatureDoesNotMatch", "The storage provider doesn't recognize the access key, or its secret is wrong."},
		{"ExpiredToken", "The storage credentials have expired."},
	} {
		requests, err := createWith(t, "us-east-1", bucketAnswers{head: status(http.StatusForbidden), sts: stsRefuses(tc.code)})
		if err == nil || errors.Is(err, ErrBucketNameTaken) {
			t.Fatalf("%s: err = %v, want a credentials failure", tc.code, err)
		}
		if d := Diagnose(err); d.Cause != CauseNoCredentials || d.Explanation != tc.want {
			t.Errorf("%s: diagnosis %+v", tc.code, d)
		}
		for _, request := range requests {
			if strings.HasPrefix(request, "PUT") {
				t.Errorf("%s: a bucket was requested with refused credentials: %q", tc.code, requests)
			}
		}
	}
}

// Any HEAD answer other than found, missing, forbidden or redirected leaves
// the name's state unknown, so nothing is created.
func TestCreateBucketUnexpectedNameCheckAnswerStopsBeforeCreating(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		requests, err := createWith(t, "eu-west-1", bucketAnswers{head: status(code)})
		if err == nil || errors.Is(err, ErrBucketNameTaken) || errors.Is(err, ErrBucketMayExist) {
			t.Fatalf("HEAD %d: err = %v, want the request's own error", code, err)
		}
		if len(requests) != 1 || !strings.HasPrefix(requests[0], "HEAD") {
			t.Errorf("HEAD %d: requests %q, want the HEAD alone", code, requests)
		}
	}
}

// A credentials check that got a server error is not retried: one attempt,
// and its answer is inconclusive, so the name reads as in use.
func TestCreateBucketCredentialsCheckMakesOneAttempt(t *testing.T) {
	t.Parallel()
	requests, err := createWith(t, "us-east-1", bucketAnswers{head: status(http.StatusForbidden), sts: status(http.StatusInternalServerError)})
	if !errors.Is(err, ErrBucketNameTaken) {
		t.Fatalf("err = %v, want ErrBucketNameTaken", err)
	}
	posts := 0
	for _, request := range requests {
		if strings.HasPrefix(request, "POST") {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("%d STS requests, want 1: %q", posts, requests)
	}
}

// Only a credential-class answer from STS is a credentials failure. An STS
// that cannot be reached, a proxy's own 403, a server error, or an answer
// that is not about the keys leaves the 403 to the name check: in use.
func TestCreateBucketInconclusiveCredentialsCheckReadsAsInUse(t *testing.T) {
	t.Parallel()
	s3Server := httptest.NewServer(&bucketRecorder{reply: bucketAnswers{head: status(http.StatusForbidden)}.reply})
	defer s3Server.Close()
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachableURL := unreachable.URL
	unreachable.Close()
	for _, tc := range []struct {
		name string
		sts  string
		ans  func(http.ResponseWriter)
	}{
		{"STS unreachable", unreachableURL, nil},
		{"proxy answers AccessDenied", "", apiError(http.StatusForbidden, "AccessDenied")},
		{"STS server error", "", status(http.StatusServiceUnavailable)},
		{"unrecognized STS answer", "", apiError(http.StatusBadRequest, "SomethingNew")},
	} {
		endpoint := tc.sts
		if tc.ans != nil {
			sts := httptest.NewServer(&bucketRecorder{reply: bucketAnswers{sts: tc.ans}.reply})
			defer sts.Close()
			endpoint = sts.URL
		}
		cfg := aws.Config{
			Region:       "us-east-1",
			Credentials:  awscredentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
			BaseEndpoint: aws.String(endpoint),
		}
		err := NewBucketAdmin(NewClient(cfg, s3Server.URL, true, 1), cfg).CreateBucket(context.Background(), "agent-archive-1", "us-east-1")
		if !errors.Is(err, ErrBucketNameTaken) || IsCredentialsCheck(err) {
			t.Errorf("%s: err = %v, want ErrBucketNameTaken and no credentials failure", tc.name, err)
		}
	}
}

func TestIsCredentialsCheck(t *testing.T) {
	t.Parallel()
	wrapped := NewCredentialsCheckError(&smithy.GenericAPIError{Code: "AccessDenied", Message: "synthetic"})
	if !IsCredentialsCheck(wrapped) || !IsCredentialsCheck(fmt.Errorf("outer: %w", wrapped)) || IsCredentialsCheck(errors.New("other")) {
		t.Fatal("IsCredentialsCheck does not follow the wrapped error")
	}
}

// An override meant for S3 alone (AWS_ENDPOINT_URL_S3, a profile's
// services.s3 endpoint_url) reaches S3 only. STS is asked where the global
// override, or its own, says.
func TestCreateBucketCredentialsCheckDoesNotUseAnS3OnlyEndpoint(t *testing.T) {
	t.Parallel()
	s3Server := &bucketRecorder{reply: bucketAnswers{head: status(http.StatusForbidden)}.reply}
	s3 := httptest.NewServer(s3Server)
	defer s3.Close()
	var stsHosts []string
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: awscredentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		HTTPClient: privacyHTTP(func(r *http.Request) (*http.Response, error) {
			if !strings.HasPrefix(r.URL.Host, "127.0.0.1") {
				stsHosts = append(stsHosts, r.URL.Host)
				rec := httptest.NewRecorder()
				stsIdentity(rec)
				return rec.Result(), nil
			}
			// Parallel httptest server cleanup closes the default transport's
			// idle connections, so use this server's isolated transport.
			return s3.Client().Do(r)
		}),
	}
	// The S3 client alone is pointed at the fake server, as an S3-only
	// override does; cfg carries no global override.
	admin := NewBucketAdmin(NewClient(cfg, s3.URL, true, 1), cfg)
	err := admin.CreateBucket(context.Background(), "agent-archive-1", "us-east-1")
	if !errors.Is(err, ErrBucketNameTaken) {
		t.Fatalf("err = %v, want ErrBucketNameTaken", err)
	}
	if len(stsHosts) != 1 || !strings.HasPrefix(stsHosts[0], "sts.") {
		t.Fatalf("STS was asked at %q, want its own endpoint (sts.<region>.amazonaws.com), not the S3 override", stsHosts)
	}
	for _, request := range s3Server.methods() {
		if strings.HasPrefix(request, "POST") {
			t.Errorf("the S3-only endpoint was sent an STS request: %q", s3Server.methods())
		}
	}
}

// The global override applies to STS as well.
func TestCreateBucketCredentialsCheckUsesTheGlobalEndpoint(t *testing.T) {
	t.Parallel()
	s3Server := &bucketRecorder{reply: bucketAnswers{head: status(http.StatusForbidden)}.reply}
	s3 := httptest.NewServer(s3Server)
	defer s3.Close()
	stsServer := &bucketRecorder{reply: bucketAnswers{}.reply}
	sts := httptest.NewServer(stsServer)
	defer sts.Close()
	cfg := aws.Config{
		Region:       "us-east-1",
		Credentials:  awscredentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
		BaseEndpoint: aws.String(sts.URL),
	}
	admin := NewBucketAdmin(NewClient(cfg, s3.URL, true, 1), cfg)
	if err := admin.CreateBucket(context.Background(), "agent-archive-1", "us-east-1"); !errors.Is(err, ErrBucketNameTaken) {
		t.Fatalf("err = %v, want ErrBucketNameTaken", err)
	}
	if got := stsServer.methods(); len(got) != 1 || !strings.HasPrefix(got[0], "POST") {
		t.Fatalf("the global endpoint got %q, want the one STS call", got)
	}
}

func TestReadBlockPublicAccess(t *testing.T) {
	t.Parallel()
	all := `<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`
	partial := `<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls><BlockPublicPolicy>false</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`
	for _, tc := range []struct {
		name   string
		reply  func(http.ResponseWriter)
		allOn  bool
		denied bool
		failed bool
	}{
		{"all four on", func(w http.ResponseWriter) { _, _ = io.WriteString(w, all) }, true, false, false},
		{"one off", func(w http.ResponseWriter) { _, _ = io.WriteString(w, partial) }, false, false, false},
		{"no settings in the answer", status(http.StatusOK), false, false, true},
		{"denied", apiError(http.StatusForbidden, "AccessDenied"), false, true, true},
		{"not there yet", apiError(http.StatusNotFound, "NoSuchBucket"), false, false, true},
	} {
		server := httptest.NewServer(&bucketRecorder{reply: func(c bucketCall, w http.ResponseWriter) {
			if c.method != http.MethodGet || !strings.Contains(c.query, "publicAccessBlock") {
				t.Errorf("%s: unexpected request %s ?%s", tc.name, c.method, c.query)
			}
			tc.reply(w)
		}})
		allOn, err := adminIn(server, "us-east-1").ReadBlockPublicAccess(context.Background(), "agent-archive-1")
		server.Close()
		if allOn != tc.allOn || (err != nil) != tc.failed || (err != nil && (Diagnose(err).Cause == CauseAccessDenied) != tc.denied) {
			t.Errorf("%s: allOn=%v err=%v", tc.name, allOn, err)
		}
	}
}

func TestCreateBucketDeniedIsAccessDeniedNotTaken(t *testing.T) {
	t.Parallel()
	_, err := createWith(t, "eu-west-1", bucketAnswers{put: apiError(http.StatusForbidden, "AccessDenied")})
	if err == nil || errors.Is(err, ErrBucketNameTaken) || errors.Is(err, ErrBucketMayExist) || Diagnose(err).Cause != CauseAccessDenied {
		t.Fatalf("err = %v, want access denied that is neither taken nor unclear", err)
	}
}

func TestCreateBucketWithNoClearAnswerMayHaveCreatedIt(t *testing.T) {
	t.Parallel()
	_, err := createWith(t, "eu-west-1", bucketAnswers{put: status(http.StatusServiceUnavailable)})
	if !errors.Is(err, ErrBucketMayExist) {
		t.Fatalf("500-class answer: err = %v, want ErrBucketMayExist", err)
	}
	// The connection drops after the name check.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, _, _ := hijacker.Hijack()
		_ = conn.Close()
	}))
	defer server.Close()
	err = adminIn(server, "eu-west-1").CreateBucket(context.Background(), "agent-archive-1", "eu-west-1")
	if !errors.Is(err, ErrBucketMayExist) {
		t.Fatalf("dropped connection: err = %v, want ErrBucketMayExist", err)
	}
}

func TestCreateBucketNamesTheAccountLimitAndInvalidNames(t *testing.T) {
	t.Parallel()
	_, err := createWith(t, "eu-west-1", bucketAnswers{put: apiError(http.StatusBadRequest, "TooManyBuckets")})
	if !errors.Is(err, ErrTooManyBuckets) {
		t.Fatalf("err = %v, want ErrTooManyBuckets", err)
	}
	_, err = createWith(t, "eu-west-1", bucketAnswers{put: apiError(http.StatusBadRequest, "InvalidBucketName")})
	if !errors.Is(err, ErrInvalidBucketName) {
		t.Fatalf("err = %v, want ErrInvalidBucketName", err)
	}
}

func TestBlockPublicAccessTurnsAllFourSettingsOn(t *testing.T) {
	t.Parallel()
	fake := &bucketRecorder{reply: func(_ bucketCall, w http.ResponseWriter) { w.WriteHeader(http.StatusOK) }}
	server := httptest.NewServer(fake)
	defer server.Close()
	if err := adminIn(server, "us-east-1").BlockPublicAccess(context.Background(), "agent-archive-1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || fake.calls[0].method != http.MethodPut || fake.calls[0].path != "/agent-archive-1/" || !strings.Contains(fake.calls[0].query, "publicAccessBlock") {
		t.Fatalf("requests %q, want one PUT ?publicAccessBlock", fake.methods())
	}
	for _, flag := range []string{"BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets"} {
		if !strings.Contains(fake.calls[0].body, "<"+flag+">true</"+flag+">") {
			t.Errorf("body %q does not set %s to true", fake.calls[0].body, flag)
		}
	}
}

func TestBlockPublicAccessDeniedIsAccessDenied(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(&bucketRecorder{reply: func(_ bucketCall, w http.ResponseWriter) { replyS3Error(w, http.StatusForbidden, "AccessDenied") }})
	defer server.Close()
	err := adminIn(server, "us-east-1").BlockPublicAccess(context.Background(), "agent-archive-1")
	if Diagnose(err).Cause != CauseAccessDenied {
		t.Fatalf("err = %v, want access denied", err)
	}
}

func TestDeleteBucketDeletesOnlyThatBucket(t *testing.T) {
	t.Parallel()
	fake := &bucketRecorder{reply: func(_ bucketCall, w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }}
	server := httptest.NewServer(fake)
	defer server.Close()
	if err := adminIn(server, "us-east-1").DeleteBucket(context.Background(), "agent-archive-1"); err != nil {
		t.Fatal(err)
	}
	if got := fake.methods(); len(got) != 1 || got[0] != "DELETE /agent-archive-1/?" {
		t.Fatalf("requests %q, want one DELETE of the bucket", got)
	}
}

func TestBucketAdminInspectPrivacyReadsBlockPublicAccessBack(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(&bucketRecorder{reply: func(c bucketCall, w http.ResponseWriter) {
		if c.method != http.MethodGet || !strings.Contains(c.query, "publicAccessBlock") {
			t.Errorf("unexpected request %s %s?%s", c.method, c.path, c.query)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`)
	}})
	defer server.Close()
	report := adminIn(server, "us-east-1").InspectPrivacy(context.Background(), "agent-archive-1")
	if report.State != "verified_private" {
		t.Fatalf("report %+v, want verified_private", report)
	}
}

func TestCredentialFailureIsDecidedByCodesAndNamedNoCredentialErrorsOnly(t *testing.T) {
	t.Parallel()
	stsOp := func(operation string, err error) error {
		return &smithy.OperationError{ServiceID: "STS", OperationName: operation, Err: err}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"expired token", stsOp("GetCallerIdentity", &smithy.GenericAPIError{Code: "ExpiredToken"}), true},
		{"unknown key", stsOp("GetCallerIdentity", &smithy.GenericAPIError{Code: "InvalidClientTokenId"}), true},
		{"bad signature", stsOp("GetCallerIdentity", &smithy.GenericAPIError{Code: "SignatureDoesNotMatch"}), true},
		{"empty static credentials", stsOp("GetCallerIdentity", &awscredentials.StaticCredentialsEmptyError{}), true},
		{"a proxy's AccessDenied", stsOp("GetCallerIdentity", &smithy.GenericAPIError{Code: "AccessDenied"}), false},
		{"an unrecognized code", stsOp("GetCallerIdentity", &smithy.GenericAPIError{Code: "SomethingNew"}), false},
		{"a plain error", stsOp("GetCallerIdentity", errors.New("boom")), false},
		{"a doubly wrapped non-credential operation error", stsOp("GetCallerIdentity", stsOp("AssumeRole", errors.New("boom"))), false},
		{"a doubly wrapped proxy refusal", stsOp("GetCallerIdentity", stsOp("AssumeRole", &smithy.GenericAPIError{Code: "AccessDenied"})), false},
	} {
		if got := credentialFailure(tc.err); got != tc.want {
			t.Errorf("%s: credentialFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}
