package archive

import (
	"errors"
	"testing"
)

func TestNativeImportEscapedSelectorSurvivesDamagedEarlierString(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string][]byte{
		"escaped":       []byte(`{"ignored":"unterminated,"native_\u0069mport":{}}`),
		"escaped-alias": []byte(`{"ignored":"unterminated,"NATIVE_\u0049MPORT":{}}`),
		"fully-escaped": []byte(`{"ignored":"unterminated,"\u006e\u0061\u0074\u0069\u0076\u0065\u005f\u0069\u006d\u0070\u006f\u0072\u0074":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reg := SessionRegistration{NativeSessionID: "unchanged"}
			if err := DecodeSessionRegistration(raw, &reg); !errors.Is(err, ErrUnsupportedNativeImport) {
				t.Fatal("escaped private selector lost after damaged string", err)
			}
			if reg.NativeSessionID != "unchanged" {
				t.Fatal("malformed selector changed registration")
			}
			control := NativeImportBatchControl{ID: "unchanged"}
			if err := DecodeNativeImportBatchControl(raw, &control); !errors.Is(err, ErrUnsupportedNativeImport) {
				t.Fatal("escaped confirmation selector lost after damaged string", err)
			}
			if control.ID != "unchanged" {
				t.Fatal("malformed selector changed batch")
			}
		})
	}
}

func TestNativeImportEscapedSelectorValueRemainsOrdinary(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"project_root":"literal \"native_\u0069mport\":{}"}`)
	var reg SessionRegistration
	if err := DecodeSessionRegistration(raw, &reg); err != nil {
		t.Fatal("ordinary escaped string value acquired authority", err)
	}
	if reg.NativeImport != nil || reg.ProjectRoot != `literal "native_import":{}` {
		t.Fatal("ordinary value changed")
	}
}
