package jsonwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

type testBudget struct {
	limit int64
	used  int64
	peak  int64
}

func (b *testBudget) Reserve(n int64) bool {
	if n < 0 || n > b.limit-b.used {
		return false
	}
	b.used += n
	b.peak = max(b.peak, b.used)
	return true
}

func (b *testBudget) Release(n int64) { b.used -= n }

func (b *testBudget) Available() int64 { return b.limit - b.used }

func TestStreamingWireMatchesDefaultSpecialsAndTags(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.FixedZone("offset", -3600))
	values := []any{nil, true, false, math.MaxFloat64, math.SmallestNonzeroFloat64, float32(1e-7), float32(1e20), math.Copysign(0, -1), int64(math.MinInt64), uint64(math.MaxUint64), json.Number(""), json.Number("1.234e+100"), "<> &\u2028\u2029\x00\xff", []byte{0, 1, 2, 3}, []byte{}, []byte(nil), [3]byte{1, 2, 3}, at, &at, json.RawMessage(` {"text":"<&>  ", "raw":"a\\b", "array": [0,true]} `), map[string]any{"<&>": []any{1.0, nil, "\"\\\n"}, "A": 0, "a": 1}, struct {
		A  string    `json:"a,omitempty"`
		B  []byte    `json:"b"`
		C  int       `json:"c,string"`
		D  string    `json:"d,string"`
		At time.Time `json:"at,omitzero"`
	}{B: []byte{1}, C: 3, D: "<>\\\"\n", At: time.Time{}.UTC()}}
	for _, value := range values {
		expected, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		budget := &testBudget{limit: 128 << 10}
		var actual bytes.Buffer
		if err := Encode(t.Context(), &actual, value, budget); err != nil {
			t.Fatalf("%T %v", value, err)
		}
		if actual.String() != string(expected)+"\n" {
			t.Fatalf("%T: got %q want %q", value, actual.String(), expected)
		}
		if budget.used != 0 {
			t.Fatal("loan leaked", budget.used)
		}
	}
}

func TestStreamingLargeScalarsNeedOnlyBoundedScratch(t *testing.T) {
	budget := &testBudget{limit: 68 << 10}
	value := struct {
		Text  string
		Raw   json.RawMessage
		Bytes []byte
	}{Text: strings.Repeat("<>世界\n", 1<<17), Raw: json.RawMessage(`{"text":"` + strings.Repeat("<&> ", 1<<17) + `"}`), Bytes: bytes.Repeat([]byte{1, 2, 3}, 1<<20)}
	expected, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var actual bytes.Buffer
	if err := Encode(t.Context(), &actual, value, budget); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Bytes(), append(expected, '\n')) {
		t.Fatal("large streaming bytes differ")
	}
	if budget.used != 0 || budget.peak > 68<<10 {
		t.Fatal("unbounded/retained loan", budget)
	}
}

type shortJSONWriter struct{}

func (shortJSONWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

type cancelJSONWriter struct{ cancel context.CancelFunc }

func (w cancelJSONWriter) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }

func TestStreamingPressureCancellationAndWriteFailureKeepLoansExact(t *testing.T) {
	value := map[string]any{}
	for i := range 1000 {
		value[strings.Repeat("x", i+1)] = i
	}
	pressure := &testBudget{limit: 68 << 10}
	var output bytes.Buffer
	if err := Encode(t.Context(), &output, value, pressure); !errors.Is(err, ErrMemory) {
		t.Fatal(err)
	}
	if output.Len() != 0 || pressure.used != 0 {
		t.Fatal("pressure produced output or leaked", output.Len(), pressure.used)
	}
	budget := &testBudget{limit: 64 << 10}
	if err := Encode(t.Context(), shortJSONWriter{}, strings.Repeat("x", 100000), budget); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	if err := Encode(ctx, cancelJSONWriter{cancel: cancel}, strings.Repeat("x", 100000), budget); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if budget.used != 0 {
		t.Fatal("fault loan leaked", budget.used)
	}
}

type arbitraryTextMarshaler struct{}

func (arbitraryTextMarshaler) MarshalText() ([]byte, error) {
	return []byte("caller-provided-text"), nil
}

func TestStreamingUnsupportedAndMalformedValuesRefuseBeforeOutput(t *testing.T) {
	type embedded struct{ A string }
	type object struct{ embedded }
	for _, value := range []any{arbitraryMarshaler{}, arbitraryTextMarshaler{}, object{}, json.RawMessage(`{"a":}`), json.Number("01"), math.NaN(), map[int]string{1: "x"}} {
		var out bytes.Buffer
		budget := &testBudget{limit: 64 << 10}
		if err := Encode(t.Context(), &out, value, budget); err == nil {
			t.Fatalf("accepted %T", value)
		}
		if out.Len() != 0 || budget.used != 0 {
			t.Fatal("unsupported output/loan", out.Len(), budget.used)
		}
	}
}

type countedCancelContext struct {
	context.Context
	remaining int
}

func (c *countedCancelContext) Err() error {
	c.remaining--
	if c.remaining < 0 {
		return context.Canceled
	}
	return nil
}

func TestStreamingCancelsWithinLargeRawWhitespace(t *testing.T) {
	ctx := &countedCancelContext{Context: t.Context(), remaining: 100}
	budget := &testBudget{limit: 64 << 10}
	var output bytes.Buffer
	value := json.RawMessage(strings.Repeat(" ", 1<<20) + "null")
	if err := Encode(ctx, &output, value, budget); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	if output.Len() != 0 || budget.used != 0 {
		t.Fatalf("partial output %d or leaked charge %d", output.Len(), budget.used)
	}
}
