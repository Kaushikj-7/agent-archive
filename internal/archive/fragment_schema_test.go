package archive

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const fragmentHeaderFixture = `{"kind":"fragment_header","fragment_version":1,"agent":"codex","thread_id":"11111111-1111-4111-8111-111111111111","physical_rollout_id":"22222222-2222-4222-8222-222222222222","completeness":"incomplete","activity_ownership":"unknown","capture":{"adapter_name":"codex","adapter_version":"0.18.0","source_format":"codex-jsonl","filter_version":"18","captured_at":"2026-10-12T06:00:00Z"},"missing_evidence":[{"reason":"missing_base","physical_rollout_id":"33333333-3333-4333-8333-333333333333"},{"reason":"unproven_ownership"}],"record_count":2}`

const fragmentRecordFixture = `{"kind":"fragment_record","ordinal":9007199254740993,"record":{"type":"response_item","timestamp":"2026-10-11T12:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic surviving prompt"}]}}}`

func validateFragmentLine(t *testing.T, schema *jsonschema.Schema, raw string) error {
	t.Helper()
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return schema.Validate(value)
}

func TestProposedFragmentSchemaAcceptsOnlyIncompleteUnknownOwnership(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "codex-fragment.schema.json")
	if err := validateFragmentLine(t, schema, fragmentHeaderFixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		from string
		to   string
	}{
		{"complete", `"incomplete"`, `"complete"`},
		{"owned_activity", `"unknown"`, `"own"`},
		{"future_version", `"fragment_version":1`, `"fragment_version":2`},
		{"source_discriminator", `"fragment_header"`, `"header"`},
		{"other_agent", `"agent":"codex"`, `"agent":"claude"`},
		{"missing_identity", `"thread_id":"11111111-1111-4111-8111-111111111111",`, ``},
		{"invalid_identity", `11111111-1111-4111-8111-111111111111`, `not-a-thread`},
		{"native_path", `"record_count":2`, `"record_count":2,"cwd":"/synthetic/private"`},
		{"creation_claim", `"record_count":2`, `"record_count":2,"created_at":"2026-10-11T12:00:00Z"`},
		{"identity_reservation", `"record_count":2`, `"record_count":2,"archive_session_id":"reserved"`},
		{"missing_provenance", `"filter_version":"18",`, ``},
		{"unknown_provenance_field", `"filter_version":"18"`, `"filter_version":"18","privacy_approved":true`},
		{"invalid_capture_time", `2026-10-12T06:00:00Z`, `yesterday`},
		{"unsupported_source_format", `"codex-jsonl"`, `"generic-json"`},
		{"empty_adapter_version", `"adapter_version":"0.18.0"`, `"adapter_version":""`},
		{"control_adapter_version", `"adapter_version":"0.18.0"`, `"adapter_version":"bad\nversion"`},
		{"invalid_filter_version", `"filter_version":"18"`, `"filter_version":"unknown"`},
		{"zero_records", `"record_count":2`, `"record_count":0`},
		{"too_many_records", `"record_count":2`, `"record_count":1000001`},
		{"fractional_count", `"record_count":2`, `"record_count":1.5`},
		{"unknown_missing_reason", `"missing_base"`, `"unknown_format_is_safe"`},
		{"privacy_initialization_failure", `"missing_base"`, `"missing_privacy_context"`},
		{"dependency_path", `"reason":"unproven_ownership"`, `"reason":"unproven_ownership","path":"/synthetic/ancestor"`},
		{"missing_evidence_absent", `"missing_evidence":[{"reason":"missing_base","physical_rollout_id":"33333333-3333-4333-8333-333333333333"},{"reason":"unproven_ownership"}],`, ``},
		{"empty_missing_evidence", `[{"reason":"missing_base","physical_rollout_id":"33333333-3333-4333-8333-333333333333"},{"reason":"unproven_ownership"}]`, `[]`},
		{"duplicate_missing_evidence", `[{"reason":"missing_base","physical_rollout_id":"33333333-3333-4333-8333-333333333333"},{"reason":"unproven_ownership"}]`, `[{"reason":"missing_base"},{"reason":"missing_base"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if validateFragmentLine(t, schema, strings.Replace(fragmentHeaderFixture, tc.from, tc.to, 1)) == nil {
				t.Fatal("invalid proposed fragment header accepted")
			}
		})
	}
}

func TestProposedFragmentSchemaRestrictsSupportedRecordShapes(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "codex-fragment.schema.json")
	for _, raw := range []string{
		fragmentRecordFixture,
		`{"kind":"fragment_record","record":{"type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111"}}}`,
		`{"kind":"fragment_record","record":{"type":"turn_context","payload":{"model":"synthetic-model"}}}`,
		`{"kind":"fragment_record","record":{"type":"event_msg","payload":{"type":"user_message","message":"Synthetic prompt"}}}`,
		`{"kind":"fragment_record","record":{"type":"event_msg","payload":{"type":"agent_message","message":"Synthetic response"}}}`,
		`{"kind":"fragment_record","record":{"type":"event_msg","payload":{"type":"task_started","turn_id":"synthetic-turn"}}}`,
		`{"kind":"fragment_record","record":{"type":"event_msg","payload":{"type":"task_complete"}}}`,
	} {
		if err := validateFragmentLine(t, schema, raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		from string
		to   string
	}{
		{"other_line_kind", `"fragment_record"`, `"native_record"`},
		{"unknown_record", `"response_item"`, `"arbitrary_json"`},
		{"unknown_payload", `"type":"message"`, `"type":"unrecognized"`},
		{"hidden_role", `"role":"user"`, `"role":"system"`},
		{"developer_role", `"role":"user"`, `"role":"developer"`},
		{"raw_field", `"type":"response_item"`, `"type":"response_item","raw":"private"`},
		{"hidden_payload", `"role":"user"`, `"role":"user","instructions":"private"`},
		{"binary_content", `"input_text"`, `"input_image"`},
		{"unknown_content_field", `"text":"Synthetic surviving prompt"`, `"text":"Synthetic surviving prompt","private_field":"private"`},
		{"invalid_timestamp", `2026-10-11T12:00:00Z`, `not-a-time`},
		{"unknown_envelope_field", `"ordinal":9007199254740993`, `"ordinal":9007199254740993,"path":"/synthetic"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if validateFragmentLine(t, schema, strings.Replace(fragmentRecordFixture, tc.from, tc.to, 1)) == nil {
				t.Fatal("unsupported fragment record accepted")
			}
		})
	}
}

func TestProposedFragmentSchemaPreservesExactOptionalUnsignedOrdinals(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "codex-fragment.schema.json")
	for _, ordinal := range []string{"0", "9007199254740993", "18446744073709551615"} {
		if err := validateFragmentLine(t, schema, strings.Replace(fragmentRecordFixture, "9007199254740993", ordinal, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateFragmentLine(t, schema, strings.Replace(fragmentRecordFixture, `"ordinal":9007199254740993,`, "", 1)); err != nil {
		t.Fatalf("unknown ordinal must remain absent: %v", err)
	}
	for _, ordinal := range []string{"-1", "18446744073709551616", "1.5", "null", `"1"`} {
		if validateFragmentLine(t, schema, strings.Replace(fragmentRecordFixture, "9007199254740993", ordinal, 1)) == nil {
			t.Fatalf("invalid ordinal %s accepted", ordinal)
		}
	}
}

func TestCompleteSourceReaderRefusesProposedFragmentWire(t *testing.T) {
	t.Parallel()
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	header := strings.Replace(fragmentHeaderFixture, `"record_count":2`, `"record_count":1`, 1)
	if _, err := w.Write([]byte(header + "\n" + fragmentRecordFixture + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSourceBundle(&compressed, DecodeOptions{}); err == nil {
		t.Fatal("fragment accepted as a complete source bundle")
	}
}

func TestProposedFragmentSchemaBoundsProvenanceAndText(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "codex-fragment.schema.json")
	for _, raw := range []string{
		strings.Replace(fragmentHeaderFixture, `"record_count":2`, `"record_count":1000000`, 1),
		strings.Replace(fragmentRecordFixture, "Synthetic surviving prompt", strings.Repeat("a", 65536), 1),
	} {
		if err := validateFragmentLine(t, schema, raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{
		strings.Replace(fragmentHeaderFixture, `"adapter_version":"0.18.0"`, `"adapter_version":"`+strings.Repeat("a", 65)+`"`, 1),
		strings.Replace(fragmentHeaderFixture, `"filter_version":"18"`, `"filter_version":"`+strings.Repeat("1", 33)+`"`, 1),
		strings.Replace(fragmentRecordFixture, "Synthetic surviving prompt", strings.Repeat("a", 65537), 1),
	} {
		if validateFragmentLine(t, schema, raw) == nil {
			t.Fatal("fragment schema accepted oversized provenance or text")
		}
	}
}
