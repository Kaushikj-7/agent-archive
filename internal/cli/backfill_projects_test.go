package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestHistoricalProjectTopLevelCommandRemoved(t *testing.T) {
	t.Parallel()
	env := Env{Home: func() (string, error) { t.Fatal("removed command accessed runtime"); return "", nil }}
	var out, errOut bytes.Buffer
	if code := Run([]string{"historical-project", "list"}, nil, &out, &errOut, env); code != 2 || !strings.Contains(errOut.String(), `unknown command "historical-project"`) {
		t.Fatalf("removed command: exit %d %s %s", code, &out, &errOut)
	}
	if strings.Contains(usage, "historical-project") {
		t.Fatal("top-level help still advertises removed command")
	}
}
