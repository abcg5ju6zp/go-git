package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

func TestSendPackWithReportStatus(t *testing.T) {
	t.Parallel()
	caps := capability.List{}
	caps.Add(capability.ReportStatus)

	reportStatusResponse := strings.Join([]string{
		"000eunpack ok\n",
		"0019ok refs/heads/master\n",
		"0000",
	}, "")
	reader := io.NopCloser(bytes.NewReader([]byte(reportStatusResponse)))
	writer := newMockWriteCloser(nil)

	req := &PushRequest{
		Commands: []*packp.Command{
			{
				Name: plumbing.ReferenceName("refs/heads/master"),
				Old:  plumbing.ZeroHash,
				New:  plumbing.NewHash("0123456789012345678901234567890123456789"),
			},
		},
		Packfile: io.NopCloser(&bytes.Buffer{}),
	}

	err := SendPack(context.TODO(), nil, caps, writer, reader, req)
	assert.NoError(t, err)
	assert.True(t, writer.closed)
}

func TestSendPackWithReportStatusError(t *testing.T) {
	t.Parallel()
	caps := capability.List{}
	caps.Add(capability.ReportStatus)

	reportStatusResponse := strings.Join([]string{
		"0012unpack failed\n",
		"0000",
	}, "")
	reader := io.NopCloser(bytes.NewReader([]byte(reportStatusResponse)))
	writer := newMockWriteCloser(nil)

	req := &PushRequest{
		Commands: []*packp.Command{
			{
				Name: plumbing.ReferenceName("refs/heads/master"),
				Old:  plumbing.ZeroHash,
				New:  plumbing.NewHash("0123456789012345678901234567890123456789"),
			},
		},
		Packfile: io.NopCloser(&bytes.Buffer{}),
	}

	err := SendPack(context.Background(), nil, caps, writer, reader, req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unpack error: failed")
}

func TestSendPackWithoutReportStatus(t *testing.T) {
	t.Parallel()
	caps := capability.List{}

	reader := io.NopCloser(bytes.NewReader(nil))
	writer := newMockWriteCloser(nil)

	req := &PushRequest{
		Commands: []*packp.Command{
			{
				Name: plumbing.ReferenceName("refs/heads/master"),
				Old:  plumbing.ZeroHash,
				New:  plumbing.NewHash("0123456789012345678901234567890123456789"),
			},
		},
		Packfile: io.NopCloser(&bytes.Buffer{}),
	}

	err := SendPack(context.Background(), nil, caps, writer, reader, req)
	assert.NoError(t, err)
	assert.True(t, writer.closed)
}

func TestSendPackWithProgress(t *testing.T) {
	t.Parallel()
	caps := capability.List{}
	caps.Add(capability.ReportStatus)
	caps.Add(capability.Sideband64k)

	sidebandResponse := strings.Join([]string{
		"0013\x02Progress: 50%\n",
		"0030\x01" +
			"000eunpack ok\n" +
			"0019ok refs/heads/master\n" +
			"0000",
		"0000",
	}, "")
	reader := io.NopCloser(bytes.NewReader([]byte(sidebandResponse)))
	writer := newMockWriteCloser(nil)

	progressBuf := &bytes.Buffer{}
	req := &PushRequest{
		Commands: []*packp.Command{
			{
				Name: plumbing.ReferenceName("refs/heads/master"),
				Old:  plumbing.ZeroHash,
				New:  plumbing.NewHash("0123456789012345678901234567890123456789"),
			},
		},
		Packfile: io.NopCloser(&bytes.Buffer{}),
		Progress: progressBuf,
	}

	err := SendPack(context.Background(), nil, caps, writer, reader, req)
	assert.NoError(t, err)
	assert.Contains(t, progressBuf.String(), "Progress: 50%")
}

func TestSendPackWithPackfile(t *testing.T) {
	t.Parallel()
	caps := capability.List{}
	caps.Add(capability.ReportStatus)

	reportStatusResponse := strings.Join([]string{
		"000eunpack ok\n",
		"0019ok refs/heads/master\n",
		"0000",
	}, "")
	reader := io.NopCloser(bytes.NewReader([]byte(reportStatusResponse)))
	writer := newMockWriteCloser(nil)

	packfileContent := []byte("mock packfile content")
	req := &PushRequest{
		Commands: []*packp.Command{
			{
				Name: plumbing.ReferenceName("refs/heads/master"),
				Old:  plumbing.ZeroHash,
				New:  plumbing.NewHash("0123456789012345678901234567890123456789"),
			},
		},
		Packfile: io.NopCloser(bytes.NewReader(packfileContent)),
	}

	err := SendPack(context.Background(), nil, caps, writer, reader, req)
	assert.NoError(t, err)
	assert.Contains(t, writer.writeBuf.String(), "mock packfile content")
}

func TestSendPackErrors(t *testing.T) {
	t.Parallel()
	caps := capability.List{}
	caps.Add(capability.ReportStatus)

	t.Run("EncodeError", func(t *testing.T) {
		t.Parallel()
		writer := newMockWriteCloser(nil)
		writer.writeErr = errors.New("encode error")
		reader := io.NopCloser(bytes.NewReader(nil))

		req := &PushRequest{
			Commands: []*packp.Command{
				{
					Name: plumbing.ReferenceName("refs/heads/master"),
					Old:  plumbing.ZeroHash,
					New:  plumbing.NewHash("0123456789012345678901234567890123456789"),
				},
			},
			Packfile: io.NopCloser(&bytes.Buffer{}),
		}

		err := SendPack(context.Background(), nil, caps, writer, reader, req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "encode error")
	})

	t.Run("PackfileCopyError", func(t *testing.T) {
		t.Parallel()
		writer := newMockWriteCloser(nil)
		reader := io.NopCloser(bytes.NewReader(nil))

		errPackfile := &errorReader{err: errors.New("packfile read error")}
		req := &PushRequest{
			Commands: []*packp.Command{
				{
					Name: plumbing.ReferenceName("refs/heads/master"),
					Old:  plumbing.ZeroHash,
					New:  plumbing.NewHash("0123456789012345678901234567890123456789"),
				},
			},
			Packfile: io.NopCloser(errPackfile),
		}

		err := SendPack(context.Background(), nil, caps, writer, reader, req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "packfile read error")
	})

	t.Run("WriterCloseError", func(t *testing.T) {
		t.Parallel()
		writer := newMockWriteCloser(nil)
		writer.closeErr = errors.New("writer close error")
		reader := io.NopCloser(bytes.NewReader(nil))

		req := &PushRequest{
			Commands: []*packp.Command{
				{
					Name: plumbing.ReferenceName("refs/heads/master"),
					Old:  plumbing.ZeroHash,
					New:  plumbing.NewHash("0123456789012345678901234567890123456789"),
				},
			},
			Packfile: io.NopCloser(&bytes.Buffer{}),
		}

		err := SendPack(context.Background(), nil, caps, writer, reader, req)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "writer close error")
	})
}

func newReportRequest() *PushRequest {
	return &PushRequest{
		Commands: []*packp.Command{
			{
				Name: plumbing.ReferenceName("refs/heads/a"),
				Old:  plumbing.ZeroHash,
				New:  plumbing.NewHash("0123456789012345678901234567890123456789"),
			},
			{
				Name: plumbing.ReferenceName("refs/heads/b"),
				Old:  plumbing.ZeroHash,
				New:  plumbing.NewHash("abcdefabcdefabcdefabcdefabcdefabcdefabcd"),
			},
		},
		Packfile: io.NopCloser(&bytes.Buffer{}),
		Report:   &PushReport{},
	}
}

func TestSendPackReportAllAccepted(t *testing.T) {
	t.Parallel()
	caps := capability.List{}
	caps.Add(capability.ReportStatus)

	response := strings.Join([]string{
		"000eunpack ok\n",
		"0014ok refs/heads/a\n",
		"0014ok refs/heads/b\n",
		"0000",
	}, "")
	reader := io.NopCloser(bytes.NewReader([]byte(response)))
	writer := newMockWriteCloser(nil)
	req := newReportRequest()

	err := SendPack(context.Background(), nil, caps, writer, reader, req)
	assert.NoError(t, err)
	assert.Equal(t, "ok", req.Report.UnpackStatus)
	assert.Len(t, req.Report.Refs, 2)
	assert.Equal(t, plumbing.ReferenceName("refs/heads/a"), req.Report.Refs[0].Name)
	assert.Len(t, req.Report.Accepted(), 2)
	assert.Empty(t, req.Report.Rejected())
	assert.Empty(t, req.Report.Pending())
}

func TestSendPackReportMixedAcceptedAndRejected(t *testing.T) {
	t.Parallel()
	caps := capability.List{}
	caps.Add(capability.ReportStatus)

	response := strings.Join([]string{
		"000eunpack ok\n",
		"0014ok refs/heads/a\n",
		"0025ng refs/heads/b non-fast-forward\n",
		"0000",
	}, "")
	reader := io.NopCloser(bytes.NewReader([]byte(response)))
	writer := newMockWriteCloser(nil)
	req := newReportRequest()

	err := SendPack(context.Background(), nil, caps, writer, reader, req)
	var cmdErr packp.CommandStatusErr
	assert.ErrorAs(t, err, &cmdErr)
	assert.Equal(t, plumbing.ReferenceName("refs/heads/b"), cmdErr.ReferenceName)
	assert.Equal(t, "non-fast-forward", cmdErr.Status)

	assert.Len(t, req.Report.Accepted(), 1)
	assert.Equal(t, plumbing.ReferenceName("refs/heads/a"), req.Report.Accepted()[0].Name)
	rejected := req.Report.Rejected()
	assert.Len(t, rejected, 1)
	assert.Equal(t, plumbing.ReferenceName("refs/heads/b"), rejected[0].Name)
	assert.Equal(t, "non-fast-forward", rejected[0].Reason)
	assert.Empty(t, req.Report.Pending())
}

func TestSendPackReportTruncatedKeepsAcceptedAndPending(t *testing.T) {
	t.Parallel()
	caps := capability.List{}
	caps.Add(capability.ReportStatus)

	// The connection drops after the first "ok" line, with no second
	// command status and no flush.
	response := strings.Join([]string{
		"000eunpack ok\n",
		"0014ok refs/heads/a\n",
	}, "")
	reader := io.NopCloser(bytes.NewReader([]byte(response)))
	writer := newMockWriteCloser(nil)
	req := newReportRequest()

	err := SendPack(context.Background(), nil, caps, writer, reader, req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decode report-status")

	assert.Equal(t, "ok", req.Report.UnpackStatus)
	assert.Len(t, req.Report.Accepted(), 1)
	assert.Equal(t, plumbing.ReferenceName("refs/heads/a"), req.Report.Accepted()[0].Name)
	assert.Len(t, req.Report.Pending(), 1)
	assert.Equal(t, plumbing.ReferenceName("refs/heads/b"), req.Report.Pending()[0].Name)
	assert.Empty(t, req.Report.Rejected())
}

func TestSendPackReportUnpackFailureRejectsAll(t *testing.T) {
	t.Parallel()
	caps := capability.List{}
	caps.Add(capability.ReportStatus)

	response := strings.Join([]string{
		"0012unpack failed\n",
		"0000",
	}, "")
	reader := io.NopCloser(bytes.NewReader([]byte(response)))
	writer := newMockWriteCloser(nil)
	req := newReportRequest()

	err := SendPack(context.Background(), nil, caps, writer, reader, req)
	var unpackErr packp.UnpackStatusErr
	assert.ErrorAs(t, err, &unpackErr)
	assert.Equal(t, "failed", unpackErr.Status)

	assert.Len(t, req.Report.Rejected(), 2)
	assert.Empty(t, req.Report.Accepted())
	for _, ref := range req.Report.Refs {
		assert.Equal(t, "failed", ref.Reason)
	}
}

func TestSendPackReportEmptyUnpackStatusIsAnError(t *testing.T) {
	t.Parallel()
	caps := capability.List{}
	caps.Add(capability.ReportStatus)

	// A fully decoded report whose unpack line carries an empty status is
	// malformed; the pre-report implementation surfaced it as an
	// UnpackStatusErr with an empty status.
	response := strings.Join([]string{
		"000cunpack \n",
		"0014ok refs/heads/a\n",
		"0000",
	}, "")
	reader := io.NopCloser(bytes.NewReader([]byte(response)))
	writer := newMockWriteCloser(nil)
	req := newReportRequest()

	err := SendPack(context.Background(), nil, caps, writer, reader, req)
	var unpackErr packp.UnpackStatusErr
	assert.ErrorAs(t, err, &unpackErr)
	assert.Empty(t, unpackErr.Status)
	assert.Len(t, req.Report.Rejected(), 2)
}

func TestSendPackReportWithoutCapabilityIsOptimistic(t *testing.T) {
	t.Parallel()
	caps := capability.List{}

	reader := io.NopCloser(bytes.NewReader(nil))
	writer := newMockWriteCloser(nil)
	req := newReportRequest()

	err := SendPack(context.Background(), nil, caps, writer, reader, req)
	assert.NoError(t, err)
	assert.Empty(t, req.Report.UnpackStatus)
	assert.Len(t, req.Report.Accepted(), 2)
	assert.Empty(t, req.Report.Pending())
}

type errorReader struct {
	err error
}

func (r *errorReader) Read(_ []byte) (int, error) {
	return 0, r.err
}
