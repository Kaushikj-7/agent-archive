package discoveryio

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/trace"
	"io"
)

// ScanRecords applies caller-owned import bounds without native interpretation.
func ScanRecords(ctx context.Context, files agentapi.DiscoveryFiles, path string, headScanLimit, headLineLimit int64, visit func([]byte) bool) (err error) {
	f, err := files.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	input, finish := trace.CountReads(io.LimitReader(f, headScanLimit), trace.HeaderBytes)
	defer finish()
	reader := bufio.NewReaderSize(input, 64*1024)
	var line []byte
	tooLong := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := reader.ReadSlice('\n')
		if !tooLong {
			if int64(len(line)+len(chunk)) > headLineLimit {
				tooLong, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if trimmed := bytes.TrimSpace(line); !tooLong && len(trimmed) > 0 && !visit(trimmed) {
			return nil
		}
		line, tooLong = line[:0], false
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
