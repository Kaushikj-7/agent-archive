package config

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/destination"
)

func TestNativeValidationDestinationRequiresOriginalBirthAndExplicitIdentity(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	cfg := Config{Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic"}, DestinationSince: since}
	owner := archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}, NativeSessionID: "synthetic", DestinationID: cfg.DestinationID(), SessionStartedAt: since, AdmittedAt: since.Add(time.Hour)}
	candidate := archive.NativeSourceBinding{Agent: "muse", Version: 2, NativeID: owner.NativeSessionID, NativeCreatedAt: since, Cwd: "/work", Path: "/native/session.jsonl", RootRevision: "one", Checkpoint: &archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 1, SHA256: strings.Repeat("a", 64)}}
	if !cfg.NativeValidationInCurrentDestination(owner, &candidate) {
		t.Fatal("valid native window refused")
	}
	tests := map[string]func(*Config, *archive.SessionRegistration, *archive.NativeSourceBinding){
		"native predates destination despite later admission": func(_ *Config, _ *archive.SessionRegistration, b *archive.NativeSourceBinding) {
			b.NativeCreatedAt = since.Add(-time.Microsecond)
		},
		"empty ID": func(_ *Config, r *archive.SessionRegistration, _ *archive.NativeSourceBinding) { r.DestinationID = "" },
		"foreign ID": func(_ *Config, r *archive.SessionRegistration, _ *archive.NativeSourceBinding) {
			r.DestinationID = "foreign"
		},
		"invalid checkpoint": func(_ *Config, _ *archive.SessionRegistration, b *archive.NativeSourceBinding) { b.Checkpoint = nil },
		"legacy binding": func(_ *Config, _ *archive.SessionRegistration, b *archive.NativeSourceBinding) {
			b.Version = 1
			b.Checkpoint = nil
		},
		"other owner": func(_ *Config, r *archive.SessionRegistration, _ *archive.NativeSourceBinding) {
			r.NativeSessionID = "other"
		},
		"child": func(_ *Config, _ *archive.SessionRegistration, b *archive.NativeSourceBinding) { b.ParentID = "parent" },
		"import": func(_ *Config, r *archive.SessionRegistration, _ *archive.NativeSourceBinding) {
			r.Origin = archive.SessionOriginImport
		},
		"zero boundary empty ID": func(c *Config, r *archive.SessionRegistration, _ *archive.NativeSourceBinding) {
			c.DestinationSince = time.Time{}
			r.DestinationID = ""
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, r, b := cfg, owner, candidate
			mutate(&c, &r, &b)
			if c.NativeValidationInCurrentDestination(r, &b) {
				t.Fatal("unproven native window accepted")
			}
		})
	}
	zero := cfg
	zero.DestinationSince = time.Time{}
	if !zero.NativeValidationInCurrentDestination(owner, &candidate) {
		t.Fatal("explicit zero-boundary destination refused")
	}
	if cfg.NativeValidationInCurrentDestination(owner, nil) {
		t.Fatal("absent candidate accepted")
	}
}

func TestNativeValidationWindowDoesNotChangeRetainedDestinationOwnership(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	cfg := Config{Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "original"}, DestinationSince: since}
	for _, harness := range []string{"claude", "codex", "muse"} {
		for _, origin := range []archive.SessionOrigin{"", archive.SessionOriginImport} {
			for _, frozen := range []bool{false, true} {
				owner := archive.SessionRegistration{Harness: archive.Harness{Name: harness}, Origin: origin, CaptureFrozen: frozen, DestinationID: cfg.DestinationID(), NativeSessionID: "synthetic", SessionStartedAt: since.Add(-time.Hour), AdmittedAt: since.Add(-time.Hour)}
				if harness == "muse" {
					owner.NativeBinding = &archive.NativeSourceBinding{Agent: "muse", Version: 2, NativeID: owner.NativeSessionID, NativeCreatedAt: since.Add(-time.Hour), Cwd: "/work", Path: "/native/session.jsonl", RootRevision: "one", Checkpoint: &archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 1, SHA256: strings.Repeat("a", 64)}}
				}
				if !cfg.InCurrentDestination(owner) {
					t.Fatal("explicit retained ownership lost", harness, origin, frozen)
				}
				away := cfg
				away.Storage.Bucket = "other"
				if away.InCurrentDestination(owner) {
					t.Fatal("foreign destination accepted")
				}
				back := cfg
				back.DestinationSince = since.Add(time.Hour)
				if !back.InCurrentDestination(owner) {
					t.Fatal("switch-back ownership reinterpreted by time", harness, origin, frozen)
				}
			}
		}
	}
}
