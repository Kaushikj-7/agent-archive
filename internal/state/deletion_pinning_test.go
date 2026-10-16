package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestForgetPinsOwnerBeforeRetirementCallback(t *testing.T) {
	s := newTestStore(t)
	reg := registration(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	key, err := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Write(s.generationHeadPath(key), generationHead{Version: 1, Key: key, Active: reg.ArchiveSessionID}); err != nil {
		t.Fatal(err)
	}
	path := s.registrationPath(reg.ArchiveSessionID)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	foreign := reg
	foreign.NativeSessionID = "unrelated-replacement"
	wire, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	hit := false
	s.onIndexStep = func(step string) error {
		if step != "generation-retired" {
			return nil
		}
		hit = true
		if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+"-original"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, wire, 0600); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	forgotten, err := s.ForgetIdleSession(reg.ArchiveSessionID, key, false, nil)
	if !hit {
		t.Fatal("retirement callback not reached")
	}
	if err == nil || forgotten {
		t.Error("replacement registration adopted during forget", forgotten, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(after, wire) {
		t.Errorf("foreign registration deleted: err=%v forgetErr=%v", readErr, err)
	}
	before, readErr := os.ReadFile(filepath.Join(filepath.Dir(path)+"-original", filepath.Base(path)))
	if readErr != nil || !bytes.Equal(before, original) {
		t.Error("original lost", readErr)
	}
}
