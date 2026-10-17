package state

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// These are every root passed to the closed publication wire traversal. Check
// the entire reachable layout before relying on its direct field resolver.
func TestPublicationClosedReachableLayouts(t *testing.T) {
	type pendingJSON PendingPublication
	type publishedJSON publishedState
	roots := []reflect.Type{reflect.TypeFor[pendingJSON](), reflect.TypeFor[publishedJSON](), reflect.TypeFor[PublicationOriginalEvidence](), reflect.TypeFor[[]archive.SupplementalEvidence](), reflect.TypeFor[PublicationNativeTarget]()}
	seen := make(map[reflect.Type]bool)
	var visit func(reflect.Type)
	visit = func(typ reflect.Type) {
		if seen[typ] {
			return
		}
		seen[typ] = true
		// Mature selecting/catalog custom decoders enforce their own exact wire
		// contracts; source-set decoding additionally refuses mixed selecting proof.
		if typ == reflect.TypeFor[time.Time]() || typ == reflect.TypeFor[archive.ImportBatch]() || typ == reflect.TypeFor[json.RawMessage]() || typ == reflect.TypeFor[StagedSelection]() || typ == reflect.TypeFor[ReadySelection]() || typ == reflect.TypeFor[CatalogPublication]() || typ == reflect.TypeFor[publishedSnapshot]() {
			return
		}
		if reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) {
			t.Fatalf("unexamined custom JSON layout: %v", typ)
		}
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			visit(typ.Elem())
		case reflect.Struct:
			t.Log(typ)
			names := make(map[string]bool)
			for i := range typ.NumField() {
				f := typ.Field(i)
				if f.PkgPath != "" {
					continue
				}
				name := strings.Split(f.Tag.Get("json"), ",")[0]
				if name == "-" {
					continue
				}
				if f.Anonymous {
					t.Fatalf("unsupported embedded layout: %v.%s", typ, f.Name)
				}
				if name == "" {
					name = f.Name
				}
				folded := strings.ToLower(name)
				if names[folded] {
					t.Fatalf("ambiguous layout: %v.%s", typ, name)
				}
				names[folded] = true
				resolved, child := publicationJSONField(typ, name)
				if resolved != name || child != f.Type {
					t.Fatalf("field resolution mismatch: %v.%s", typ, name)
				}
				visit(f.Type)
			}
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32, reflect.Float64, reflect.Interface, reflect.String:
			// Scalars and interface-backed native values have no typed object fields.
		case reflect.Invalid, reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.UnsafePointer:
			t.Fatalf("unsupported reachable JSON layout: %v", typ)
		}
	}
	for _, root := range roots {
		visit(root)
	}
}

func TestPublicationClosedDecodeNestedAndOpaqueShapes(t *testing.T) {
	type nested struct {
		Version int `json:"version"`
	}
	type envelope struct {
		Authority nested          `json:"authority"`
		Native    map[string]any  `json:"native"`
		Raw       json.RawMessage `json:"raw"`
		At        time.Time       `json:"at"`
	}
	valid := []byte(`{"authority":{"version":2},"native":{"Secret":"α","secret":"β"},"raw":{"opaque":true},"at":"2026-10-08T00:00:00Z"}`)
	var got envelope
	if err := closedPublicationDecode(valid, &got); err != nil {
		t.Fatal(err)
	}
	if got.Native["Secret"] != "α" || got.Native["secret"] != "β" {
		t.Fatal("native map keys were folded")
	}
	for _, raw := range []string{
		`{"authority":{"version":2,"future":null}}`,
		`{"authority":{"version":2,"Version":2}}`,
		`{"authority":{"version":2},"unknown":null}`,
		`{"authority":{"version":2}} {}`,
		`{"authority":{"version":2}} garbage`,
	} {
		var rejected envelope
		err := closedPublicationDecode([]byte(raw), &rejected)
		if !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatalf("accepted %s: %v", raw, err)
		}
		if strings.Contains(raw, "} ") {
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) {
				t.Fatalf("trailing refusal became quarantine syntax error: %v", err)
			}
		}
	}
}

func TestPublicationClosedDecodePreservesTruncationClassification(t *testing.T) {
	for _, raw := range []string{"", " ", "{", `{"bundle":{"sche`} {
		var p publishedState
		err := decodeSourceSetPublishedState([]byte(raw), &p, t.Context(), nil)
		var syntax *json.SyntaxError
		if !errors.Is(err, ErrDurableStorageRecovery) || !errors.As(err, &syntax) {
			t.Fatalf("truncated bytes lost original syntax classification: %q: %v", raw, err)
		}
	}
	for _, raw := range []string{`{} {}`, `{} garbage`} {
		var p publishedState
		err := decodeSourceSetPublishedState([]byte(raw), &p, t.Context(), nil)
		var syntax *json.SyntaxError
		if !errors.Is(err, ErrDurableStorageRecovery) || errors.As(err, &syntax) {
			t.Fatalf("valid leading value trailing refusal changed classification: %q: %v", raw, err)
		}
	}
}

func TestPublicationClosedDecodeSyntaxPrecedesSemanticRefusal(t *testing.T) {
	for _, tc := range []struct {
		body      string
		malformed bool
	}{
		{`{"future":null}`, false},
		{`{"bundle":{},"bundle":{}}`, false},
		{`{"future":null,`, true},
		{`{"future":null,"bundle":{"sche`, true},
		{`{"bundle":{},"bundle":{"sche`, true},
	} {
		var p publishedState
		err := decodeSourceSetPublishedState([]byte(tc.body), &p, t.Context(), nil)
		var syntax *json.SyntaxError
		if !errors.Is(err, ErrDurableStorageRecovery) || errors.As(err, &syntax) != tc.malformed {
			t.Fatalf("wrong semantic/syntax disposition: %q: %v", tc.body, err)
		}
	}
	for _, malformed := range []bool{false, true} {
		body := strings.Repeat("[", 130) + "null" + strings.Repeat("]", 130)
		if malformed {
			body = body[:len(body)-1]
		}
		var raw json.RawMessage
		err := closedPublicationDecode([]byte(body), &raw)
		var syntax *json.SyntaxError
		if !errors.Is(err, ErrDurableStorageRecovery) || errors.As(err, &syntax) != malformed {
			t.Fatalf("wrong depth/syntax disposition: %v: %v", malformed, err)
		}
	}
}
