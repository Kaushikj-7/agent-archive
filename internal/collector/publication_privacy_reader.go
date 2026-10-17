package collector

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/state"
	"os"
)

type publicationPrivacyReader struct{ scan *sessionScan }

func (r publicationPrivacyReader) NativeReadBudget() *agentapi.NativeReadBudget {
	return r.scan.readBudget()
}

func (r publicationPrivacyReader) ReadPublicationPrivacySource(ctx context.Context, input state.PreparationInput) ([]byte, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s := r.scan
	mark := len(s.retainedReleases)
	end := mark
	release := func() {
		for i := mark; i < end; i++ {
			s.releaseRetainedIndex(i)
		}
	}
	data, err := s.historyStage(state.PendingSource{Reference: input.Reference, Name: input.Reference.SHA256 + ".gz"})
	if errors.Is(err, os.ErrNotExist) {
		data, err = s.historyGet(input.Reference.Key, int64(input.Reference.CompressedBytes))
	}
	end = len(s.retainedReleases)
	if err != nil {
		release()
		return nil, nil, err
	}
	return data, release, nil
}
