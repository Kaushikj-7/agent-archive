package archive

import (
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"
)

var dataURLPrefixReference = regexp.MustCompile(`(?i)data:`)

func TestDataURLPrefixIsRequiredByPattern(t *testing.T) {
	pattern, err := syntax.Parse(base64DataURL.String(), syntax.Perl)
	if err != nil {
		t.Fatal(err)
	}
	if !requires(pattern.Simplify(), []string{"data:"}) {
		t.Fatal("data URL regexp can match without the prefix gate")
	}
}

func dataURLGateSeeds() []string {
	payload := strings.Repeat("Ab0+/", 8)
	return []string{
		"", "ordinary kept words", strings.Repeat("kept words ", 6000),
		"::::", "dat:", "data", "DATAx:", "metadata:",
		"data:;base64," + payload,
		"DaTa:image/png;name=icon;base64," + payload,
		"first: value; DATA:text/plain;BASE64," + payload + "=",
		"édata:image/png;base64," + payload,
		"xdata:image/png;base64," + payload,
		" data:;base64,short",
		"data:application/json;charset=utf-8;base64," + payload,
		"data:image/png;base64," + payload + "\r\nDATA:text/plain;base64," + payload,
		`escaped\ndata:image/png;base64,` + payload,
		`data\u003aimage/png;base64,` + payload,
		`{"nested":"DATA:image/png;base64,` + payload + `"}`,
		"ＤＡＴＡ:image/png;base64," + payload,
		"da\u017fa:image/png;base64," + payload,
		"da\u212aa:image/png;base64," + payload,
		"\x00\xffDATA:image/png;base64," + payload,
		"password=secret; data:text/plain;base64," + payload,
	}
}

func checkDataURLGate(t *testing.T, text string) {
	t.Helper()
	if got := hasDataURLPrefix(text); got != dataURLPrefixReference.MatchString(text) {
		t.Fatalf("prefix classification differs for %q", text)
	}
	whole := base64DataURL.MatchString(text)
	if gated := hasDataURLPrefix(text) && base64DataURL.MatchString(text); gated != whole {
		t.Fatalf("data URL matching differs for %q", text)
	}
	replacement := "data:${1}${2};base64,[OMITTED]"
	want := text
	if whole {
		want = base64DataURL.ReplaceAllString(text, replacement)
	}
	got := text
	if hasDataURLPrefix(text) && base64DataURL.MatchString(text) {
		got = base64DataURL.ReplaceAllString(text, replacement)
	}
	if got != want {
		t.Fatalf("data URL replacement differs for %q", text)
	}
}

func TestDataURLPrefixGateMatchesUngated(t *testing.T) {
	for _, text := range dataURLGateSeeds() {
		checkDataURLGate(t, text)
	}
	// Every ASCII case spelling must remain visible to the prefix gate.
	for mask := range 16 {
		prefix := []byte("data:")
		for i := range 4 {
			if mask&(1<<i) != 0 {
				prefix[i] -= 'a' - 'A'
			}
		}
		checkDataURLGate(t, string(prefix)+"image/png;base64,"+strings.Repeat("A", 32))
	}
}

func FuzzDataURLPrefixGateMatchesUngated(f *testing.F) {
	for _, text := range dataURLGateSeeds() {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		checkDataURLGate(t, text)
	})
}
