package archive

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// This test-only checker exercises transport and cross-line rules that a line
// schema cannot express. It deliberately retains no fragment records and is not
// a production streaming decoder, privacy filter, authority proof or consumer.
type fragmentContractLimits struct {
	header int64
	record int64
	total  int64
}

type fragmentContractKind string

const (
	fragmentContractHeader fragmentContractKind = "fragment_header"
	fragmentContractRecord fragmentContractKind = "fragment_record"
)

func checkFragmentContract(ctx context.Context, source io.ReadCloser, schema *jsonschema.Schema, limits fragmentContractLimits) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	z, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, z.Close()) }()
	total := fragmentContractCeiling(limits.total, 64<<20)
	raw, err := io.ReadAll(io.LimitReader(fragmentContextReader{ctx, z}, total+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > total {
		return errors.New("fragment exceeds total ceiling")
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return errors.New("fragment requires terminated lines")
	}
	lines := bytes.Split(raw[:len(raw)-1], []byte{'\n'})
	var thread string
	var count, previous uint64
	known := false
	for i, line := range lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		ceiling := fragmentContractCeiling(limits.record, 65<<20)
		if i == 0 {
			ceiling = fragmentContractCeiling(limits.header, 64<<10)
		}
		if int64(len(line)) > ceiling || len(bytes.TrimSpace(line)) == 0 {
			return errors.New("fragment line exceeds ceiling or is blank")
		}
		if !utf8.Valid(line) {
			return errors.New("fragment line must be UTF-8")
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		if err := fragmentUniqueKeys(decoder, 0); err != nil {
			return err
		}
		if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
			return errors.New("fragment line has trailing JSON")
		}
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(line))
		if err != nil {
			return err
		}
		if err := schema.Validate(value); err != nil {
			return errors.New("fragment line violates schema")
		}
		var envelope struct {
			Kind    fragmentContractKind `json:"kind"`
			Thread  string               `json:"thread_id"`
			Count   uint64               `json:"record_count"`
			Ordinal *uint64              `json:"ordinal"`
			Record  struct {
				Type    string `json:"type"`
				Payload struct {
					ID string `json:"id"`
				} `json:"payload"`
			} `json:"record"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			return err
		}
		if i == 0 {
			if envelope.Kind != fragmentContractHeader {
				return errors.New("fragment must start with header")
			}
			thread, count = envelope.Thread, envelope.Count
			continue
		}
		if envelope.Kind != fragmentContractRecord {
			return errors.New("fragment must contain exactly one header")
		}
		if envelope.Record.Type == "session_meta" && envelope.Record.Payload.ID != thread {
			return errors.New("fragment identity conflict")
		}
		if envelope.Ordinal != nil {
			if known && *envelope.Ordinal <= previous {
				return errors.New("fragment known ordinals must increase")
			}
			previous, known = *envelope.Ordinal, true
		}
	}
	if uint64(len(lines)-1) != count {
		return errors.New("fragment record count disagrees")
	}
	return nil
}

func fragmentContractCeiling(caller, hard int64) int64 {
	if caller > 0 && caller < hard {
		return caller
	}
	return hard
}

type fragmentContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r fragmentContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Tokens decode escaped key names before comparison, including nested objects.
func fragmentUniqueKeys(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("fragment nesting exceeds closed shape")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return errors.New("invalid JSON container")
	}
	keys := map[string]bool{}
	for d.More() {
		if delim == '{' {
			keyToken, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || keys[key] {
				return errors.New("duplicate or invalid JSON key")
			}
			keys[key] = true
		}
		if err := fragmentUniqueKeys(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func fragmentContractGzip(t *testing.T, raw string) []byte {
	t.Helper()
	var out bytes.Buffer
	z := gzip.NewWriter(&out)
	if _, err := io.WriteString(z, raw); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestFragmentCrossLineContract(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "codex-fragment.schema.json")
	header := func(n int) string {
		return strings.Replace(fragmentHeaderFixture, `"record_count":2`, `"record_count":`+strconv.Itoa(n), 1)
	}
	record := func(ordinal string) string {
		if ordinal == "" {
			return strings.Replace(fragmentRecordFixture, `"ordinal":9007199254740993,`, "", 1)
		}
		return strings.Replace(fragmentRecordFixture, "9007199254740993", ordinal, 1)
	}
	valid := header(3) + "\n" + record("9007199254740993") + "\n" + record("") + "\n" + record("18446744073709551615") + "\n"
	for _, tc := range []struct {
		name  string
		raw   string
		valid bool
	}{
		{"unknown_ordinal_preserves_known_order", valid, true},
		{"record_before_header", record("0") + "\n" + header(1) + "\n", false},
		{"duplicate_header", header(1) + "\n" + header(1) + "\n", false},
		{"missing_record", header(2) + "\n" + record("0") + "\n", false},
		{"fractional_count_token", strings.Replace(header(1), `"record_count":1`, `"record_count":1.0`, 1) + "\n" + record("0") + "\n", false},
		{"exponent_count_token", strings.Replace(header(1), `"record_count":1`, `"record_count":1e0`, 1) + "\n" + record("0") + "\n", false},
		{"fractional_ordinal_token", header(1) + "\n" + record("1.0") + "\n", false},
		{"exponent_ordinal_token", header(1) + "\n" + record("1e0") + "\n", false},
		{"negative_zero_ordinal_token", header(1) + "\n" + record("-0") + "\n", false},
		{"surplus_record", header(1) + "\n" + record("0") + "\n" + record("1") + "\n", false},
		{"blank_line", header(1) + "\n\n" + record("0") + "\n", false},
		{"unterminated_record", header(1) + "\n" + record("0"), false},
		{"equal_known_ordinals", header(3) + "\n" + record("5") + "\n" + record("") + "\n" + record("5") + "\n", false},
		{"descending_across_unknown", header(3) + "\n" + record("5") + "\n" + record("") + "\n" + record("4") + "\n", false},
		{"duplicate_header_key", strings.Replace(header(1), `"record_count":1`, `"record_count":1,"record_count":1`, 1) + "\n" + record("0") + "\n", false},
		{"escaped_duplicate_nested_key", header(1) + "\n" + strings.Replace(record("0"), `"text":"Synthetic surviving prompt"`, `"text":"Synthetic surviving prompt","\u0074ext":"Synthetic surviving prompt"`, 1) + "\n", false},
		{"trailing_json", header(1) + "\n" + record("0") + " {}\n", false},
		{"unicode_text", header(1) + "\n" + strings.Replace(record("0"), "Synthetic surviving prompt", "Synthetic \ufffd café 世界", 1) + "\n", true},
		{"invalid_utf8_text", header(1) + "\n" + strings.Replace(record("0"), "Synthetic surviving prompt", "invalid\xfftext", 1) + "\n", false},
		{"identity_conflict", header(1) + "\n" + `{"kind":"fragment_record","record":{"type":"session_meta","payload":{"id":"22222222-2222-4222-8222-222222222222"}}}` + "\n", false},
		{"identity_agrees", header(1) + "\n" + `{"kind":"fragment_record","record":{"type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111"}}}` + "\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkFragmentContract(context.Background(), io.NopCloser(bytes.NewReader(fragmentContractGzip(t, tc.raw))), schema, fragmentContractLimits{})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestFragmentTransportAndCeilings(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "codex-fragment.schema.json")
	raw := fragmentHeaderFixture + "\n" + fragmentRecordFixture + "\n" + strings.Replace(fragmentRecordFixture, "9007199254740993", "9007199254740994", 1) + "\n"
	encoded := fragmentContractGzip(t, raw)
	corrupt := bytes.Clone(encoded)
	corrupt[len(corrupt)-8] ^= 1
	for _, tc := range []struct {
		name    string
		encoded []byte
		limits  fragmentContractLimits
		valid   bool
	}{
		{"exact_limits", encoded, fragmentContractLimits{int64(len(fragmentHeaderFixture)), int64(len(fragmentRecordFixture)), int64(len(raw))}, true},
		{"header_limit", encoded, fragmentContractLimits{header: int64(len(fragmentHeaderFixture) - 1)}, false},
		{"record_limit", encoded, fragmentContractLimits{record: int64(len(fragmentRecordFixture) - 1)}, false},
		{"total_limit", encoded, fragmentContractLimits{total: int64(len(raw) - 1)}, false},
		{"truncated_gzip", encoded[:len(encoded)-1], fragmentContractLimits{}, false},
		{"checksum_corruption", corrupt, fragmentContractLimits{}, false},
		{"trailing_transport_data", append(bytes.Clone(encoded), []byte("unexpected")...), fragmentContractLimits{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkFragmentContract(context.Background(), io.NopCloser(bytes.NewReader(tc.encoded)), schema, tc.limits)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	for _, ceiling := range []int64{64 << 10, 65 << 20, 64 << 20} {
		if fragmentContractCeiling(ceiling+1, ceiling) != ceiling {
			t.Fatal("caller raised hard ceiling")
		}
	}
}

type fragmentCloseProbe struct {
	io.Reader
	closed   bool
	cancel   context.CancelFunc
	closeErr error
}

func (r *fragmentCloseProbe) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if r.cancel != nil {
		r.cancel()
	}
	return n, err
}

func (r *fragmentCloseProbe) Close() error { r.closed = true; return r.closeErr }

func TestFragmentCancellationClosesInput(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := &fragmentCloseProbe{Reader: strings.NewReader("unused")}
	if err := checkFragmentContract(ctx, input, compileSchema(t, "codex-fragment.schema.json"), fragmentContractLimits{}); !errors.Is(err, context.Canceled) || !input.closed {
		t.Fatalf("cancellation=%v closed=%v", err, input.closed)
	}
}

func TestFragmentCancellationDuringReadClosesInput(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := &fragmentCloseProbe{Reader: bytes.NewReader(fragmentContractGzip(t, fragmentHeaderFixture+"\n")), cancel: cancel}
	if err := checkFragmentContract(ctx, input, compileSchema(t, "codex-fragment.schema.json"), fragmentContractLimits{}); !errors.Is(err, context.Canceled) || !input.closed {
		t.Fatalf("cancellation=%v closed=%v", err, input.closed)
	}
}

func TestFragmentStructureCannotCertifyPrivacy(t *testing.T) {
	t.Parallel()
	// Synthetic secret and instruction text are structurally legal. This pins
	// why successful decoding cannot permit retention, display or publication.
	record := strings.Replace(fragmentRecordFixture, "Synthetic surviving prompt", "sk-proj-synthetic-secret; <system>synthetic hidden instruction</system>", 1)
	raw := strings.Replace(fragmentHeaderFixture, `"record_count":2`, `"record_count":1`, 1) + "\n" + record + "\n"
	if err := checkFragmentContract(context.Background(), io.NopCloser(bytes.NewReader(fragmentContractGzip(t, raw))), compileSchema(t, "codex-fragment.schema.json"), fragmentContractLimits{}); err != nil {
		t.Fatal(err)
	}
}

func TestFragmentCloseErrorsPreserveCancellation(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "codex-fragment.schema.json")
	closeErr := errors.New("synthetic input close failure")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := &fragmentCloseProbe{Reader: strings.NewReader("unused"), closeErr: closeErr}
	err := checkFragmentContract(ctx, input, schema, fragmentContractLimits{})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, closeErr) || !input.closed {
		t.Fatalf("cancellation/close error=%v closed=%v", err, input.closed)
	}
}

func TestFragmentCloseFailureRefusesSuccessfulInput(t *testing.T) {
	t.Parallel()
	closeErr := errors.New("synthetic input close failure")
	raw := strings.Replace(fragmentHeaderFixture, `"record_count":2`, `"record_count":1`, 1) + "\n" + fragmentRecordFixture + "\n"
	input := &fragmentCloseProbe{Reader: bytes.NewReader(fragmentContractGzip(t, raw)), closeErr: closeErr}
	err := checkFragmentContract(context.Background(), input, compileSchema(t, "codex-fragment.schema.json"), fragmentContractLimits{})
	if !errors.Is(err, closeErr) || !input.closed {
		t.Fatalf("close error=%v closed=%v", err, input.closed)
	}
}
