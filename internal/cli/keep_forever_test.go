package cli

import (
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
