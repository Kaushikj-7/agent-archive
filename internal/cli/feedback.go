package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

const maxFeedbackBytes = 64 << 10

// runFeedbackCommand records a user's explicit assessment as supplemental
// evidence. The input path is local operational data and is never archived;
// only privacy-filtered file content and fixed provenance reach the durable
// request consumed by the next collector pass.
func runFeedbackCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("feedback", stderr)
	file := fs.String("file", "", "read explicit feedback from this local UTF-8 text file")
	// Accepts the documented `feedback SESSION_ID --file PATH` form as well
	// as flags before SESSION_ID.
	sessionID, ok := fs.parseWithArgument(args)
	if !ok {
		return 2
	}
	if sessionID == "" {
		return fs.usageError("a SESSION_ID is required (see agent-archive list)")
	}
	if *file == "" {
		return fs.usageError("--file PATH is required")
	}
	//lint:ignore LV1001 a session ID is an open external identifier; "." and ".." are rejected path components, not choices
	if sessionID == "." || sessionID == ".." || strings.ContainsAny(sessionID, "/\\") {
		return fs.usageError("invalid SESSION_ID %q", sessionID)
	}

	content, err := readFeedbackFile(*file)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: feedback: %v\n", err)
		return 1
	}
	home, err := env.readHome()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: feedback: resolve home: %v\n", err)
		return 1
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: feedback: load config: %v\n", err)
		return 1
	} else if !found || !cfg.Archive.Enabled {
		terminal.Println(stderr, "agent-archive: feedback: not set up yet; run `agent-archive setup` first")
		return 1
	}
	store, err := state.Open(home)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: feedback: open local store: %v\n", err)
		return 1
	}
	registration, found, err := store.LoadRegistration(sessionID)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: feedback: %v\n", err)
		return 1
	} else if !found {
		terminal.Printf(stderr, "agent-archive: feedback: no local session %q\n", sessionID)
		return 1
	}
	if !cfg.AcceptSession(registration) {
		terminal.Printf(stderr, "agent-archive: feedback: session %q is no longer eligible under the current setup\n", sessionID)
		return 1
	}
	now := env.now()
	eventID, err := feedbackEventID()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: feedback: create evidence identity: %v\n", err)
		return 1
	}
	filtered, gaps, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindExplicitFeedback, ObservedAt: now,
		Provenance: "user:agent-archive-feedback-file",
		Payload:    map[string]any{"event_id": eventID, "text": string(content), "source": "user"},
	}})
	if err != nil {
		terminal.Printf(stderr, "agent-archive: feedback: filter input: %v\n", err)
		return 1
	}
	if len(filtered) != 1 {
		terminal.Println(stderr, "agent-archive: feedback: input contained no retainable text")
		return 1
	}
	archive.AnnotateSupplementalGaps(filtered[0].Payload, gaps)
	if err := store.SaveRequest(sessionID, "explicit_feedback", now, filtered[0]); err != nil {
		terminal.Printf(stderr, "agent-archive: feedback: save: %v\n", err)
		return 1
	}
	terminal.Printf(stdout, "Feedback queued for session %s.\n", sessionID)
	if cfg.Paused {
		terminal.Println(stdout, "Capture is paused, so it is published after you run agent-archive resume.")
	}
	return 0
}

func feedbackEventID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func readFeedbackFile(path string) ([]byte, error) {
	return readFeedbackFileWithOpen(path, func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	})
}

func readFeedbackFileWithOpen(path string, open func(string) (*os.File, error)) ([]byte, error) {
	// Follow symlinks as before, but never block opening a substituted FIFO.
	before, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect --file: %w", err)
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("--file must name a regular file")
	}
	file, err := open(path)
	if err != nil {
		return nil, fmt.Errorf("read --file: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect --file: %w", err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, errors.New("--file must name a regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxFeedbackBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read --file: %w", err)
	}
	if len(content) > maxFeedbackBytes {
		return nil, fmt.Errorf("--file exceeds %d bytes", maxFeedbackBytes)
	}
	if !utf8.Valid(content) {
		return nil, errors.New("--file must contain valid UTF-8 text")
	}
	if strings.TrimSpace(string(content)) == "" {
		return nil, errors.New("--file is empty")
	}
	return content, nil
}
