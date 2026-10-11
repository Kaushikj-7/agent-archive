package cli

import (
	"bytes"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/config"
	"strings"
	"testing"
	"time"
)

func TestKeepForeverSetupAnswersAndDraft(t *testing.T) {
	existing := config.Config{MachineID: "m", RetentionDays: 90}
	cfg, _, _ := setupAnswers(existing, setupOptions{keepForever: true}, "", "", true, Env{})
	if cfg.RetentionDays != 0 || !cfg.RetentionProtection {
		t.Fatalf("forever %+v", cfg)
	}
	if _, _, err := setupAnswers(existing, setupOptions{keepForever: true, retentionSupplied: true, retentionDays: 90}, "", "", true, Env{}); err == nil {
		t.Fatal("conflicting policy accepted")
	}
	if got := reviewedSetupConfig(cfg, setupDraft{Config: existing}); got.RetentionDays != 0 {
		t.Fatal("stale draft shortened forever")
	}
	if got := reviewedSetupConfig(cfg, setupDraft{Config: existing, RetentionEdited: true}); got.RetentionDays != 90 {
		t.Fatal("explicit finite edit lost")
	}
	if got := anotherMachineCommand(cfg, ""); !strings.Contains(got, "--keep-forever") || strings.Contains(got, "--retention-days") {
		t.Fatalf("machine command %s", got)
	}
	if days, err := parseRetentionAnswer("forever"); err != nil || days != 0 {
		t.Fatalf("parse %d %v", days, err)
	}
}

func TestKeepForeverFreshRepeatAndFiniteSetup(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "claude", "--project", project, "--keep-forever")
	for _, args := range [][]string{{"--yes"}, {"--yes", "--prefix", "next/"}} {
		setupYes(t, env, "", 0, args...)
		cfg := mustLoadConfig(t, home)
		if cfg.RetentionDays != 0 || !cfg.RetentionProtection {
			t.Fatalf("repeat lost forever %+v", cfg)
		}
	}
	setupYes(t, env, "", 0, "--yes", "--retention-days", "30")
	cfg := mustLoadConfig(t, home)
	if cfg.RetentionDays != 30 || !cfg.RetentionProtection {
		t.Fatalf("finite lost fence %+v", cfg)
	}
}

func TestGuidedRetentionChoicesPreserveSavedPolicy(t *testing.T) {
	for _, tc := range []struct {
		input       string
		saved, want int
	}{
		{"\n", 90, 90}, {"\n\n", 14, 14}, {"\n", 0, 0},
		{"3\n", 90, 0}, {"1\n", 0, 90}, {"custom\n30\n", 0, 30}, {"custom\nforever\n", 90, 0},
	} {
		var out bytes.Buffer
		got, err := newPrompter(strings.NewReader(tc.input), &out).setupRetentionDays(tc.saved)
		if err != nil || got != tc.want {
			t.Fatalf("saved %d input %q: %d %v\n%s", tc.saved, tc.input, got, err, &out)
		}
		for _, choice := range []string{"90 days", "Custom duration", "Keep forever", "Provider lifecycle rules"} {
			if !strings.Contains(out.String(), choice) {
				t.Fatalf("missing %q in %s", choice, &out)
			}
		}
	}
}

func TestStatusReportsForeverAndFiniteRetention(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	for _, days := range []int{0, 90} {
		must(t, config.Save(home, config.Config{RetentionDays: days}))
		var out, errs bytes.Buffer
		if code := Run([]string{"status", "--json"}, nil, &out, &errs, env); code != 0 {
			t.Fatalf("status %d: %s", code, &errs)
		}
		var view statusView
		must(t, json.Unmarshal(out.Bytes(), &view))
		if view.Retention == nil || view.Retention.Days != days || view.Retention.AutomaticExpiry != (days != 0) {
			t.Fatalf("policy %+v", view.Retention)
		}
		out.Reset()
		printStatusDetails(&out, view)
		if !strings.Contains(out.String(), config.RetentionLabel(days)) || strings.Contains(out.String(), "Retention:     0 days") {
			t.Fatalf("status: %s", &out)
		}
	}
}

func TestKeepForeverConflictsFailBeforeReadingPairingOrConfig(t *testing.T) {
	for _, args := range [][]string{
		{"setup", "--yes", "--keep-forever", "--retention-days", "90"},
		{"setup", "--pair-file", "-", "--yes", "--keep-forever"},
		{"setup", "--pair-file", "-", "--yes", "--keep-forever", "--retention-days", "90"},
	} {
		env := testEnv(t, t.TempDir(), time.Now())
		var out bytes.Buffer
		if code := Run(args, &unreadInput{}, &out, &out, env); code != 2 {
			t.Fatalf("%v: code %d %s", args, code, &out)
		}
	}
}
