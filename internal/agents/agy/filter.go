package agy

import (
	"io"

	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func filterAgyReader(r io.Reader) (archive.FilteredTranscript, error) {
	return nativecodec.AgyAdapter{}.FilterJSONL(r)
}
