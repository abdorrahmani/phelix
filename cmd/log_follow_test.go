package cmd

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

type chunkedLogReader struct {
	chunks []string
	cancel context.CancelFunc
	output *bytes.Buffer
	t      *testing.T
}

func (r *chunkedLogReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		r.cancel()
		return 0, io.EOF
	}
	if r.output.Len() != 0 {
		r.t.Fatal("incomplete line reached output")
	}
	part := r.chunks[0]
	r.chunks = r.chunks[1:]
	return copy(p, part), io.EOF
}

func TestLogFollowRedactsValueSplitAcrossWrites(t *testing.T) {
	value := "split-private-credential"
	release := phelixerr.AcquireSecretValues([]string{value})
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	reader := &chunkedLogReader{chunks: []string{"split-private-", "credential\n"}, cancel: cancel, output: &output, t: t}
	if err := followLogLines(ctx, reader, &output); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != "***" {
		t.Fatal("split credential was not fully redacted")
	}
}
