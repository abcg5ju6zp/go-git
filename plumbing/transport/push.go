package transport

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/utils/ioutil"
)

// reportOK is the successful status value of unpack and command lines.
const reportOK = "ok"

// SendPack sends a packfile to a remote server.
func SendPack(
	ctx context.Context,
	_ storage.Storer,
	caps capability.List,
	writer io.WriteCloser,
	reader io.ReadCloser,
	req *PushRequest,
) error {
	writer = ioutil.NewContextWriteCloser(ctx, writer)
	reader = ioutil.NewContextReadCloser(ctx, reader)

	// Seed the per-reference report before anything goes on the wire so
	// that an early failure leaves every update pending.
	report := req.Report
	if report == nil {
		report = &PushReport{}
	}
	report.reset(req.Commands)

	var needPackfile bool
	for _, cmd := range req.Commands {
		if cmd.Action() != packp.Delete {
			needPackfile = true
			break
		}
	}

	if !needPackfile && req.Packfile != nil {
		return fmt.Errorf("packfile is not accepted for push request without new objects")
	}
	if needPackfile && req.Packfile == nil {
		return fmt.Errorf("packfile is required for push request with new objects")
	}

	upreq := buildUpdateRequests(caps, req)
	if err := upreq.Encode(writer); err != nil {
		return err
	}

	if upreq.Capabilities.Supports(capability.PushOptions) {
		var opts packp.PushOptions
		opts.Options = req.Options
		if err := opts.Encode(writer); err != nil {
			return fmt.Errorf("encoding push-options: %w", err)
		}
	}

	if req.Packfile != nil {
		if _, err := ioutil.CopyBufferPool(writer, req.Packfile); err != nil {
			return err
		}
		if err := req.Packfile.Close(); err != nil {
			return fmt.Errorf("closing packfile: %w", err)
		}
	}

	if err := writer.Close(); err != nil {
		return err
	}

	var reportStatus int
	if upreq.Capabilities.Supports(capability.ReportStatusV2) {
		reportStatus = 2
	} else if upreq.Capabilities.Supports(capability.ReportStatus) {
		reportStatus = 1
	}

	if reportStatus == 0 {
		// Without report-status the server never confirms individual
		// references. Preserve the legacy contract — a clean exchange
		// is taken as success and every tracking reference is allowed
		// to advance — instead of leaving the batch pending forever.
		report.UnpackStatus = ""
		report.markAll(storer.RefUpdateAccepted)
		return nil
	}

	var r io.Reader = reader
	if req.Progress != nil {
		var d *sideband.Demuxer
		if upreq.Capabilities.Supports(capability.Sideband64k) {
			d = sideband.NewDemuxer(sideband.Sideband64k, reader)
		} else if upreq.Capabilities.Supports(capability.Sideband) {
			d = sideband.NewDemuxer(sideband.Sideband, reader)
		}
		if d != nil {
			if !upreq.Capabilities.Supports(capability.Quiet) {
				d.Progress = req.Progress
			}
			r = d
		}
	}

	decoded := &packp.ReportStatus{}
	decodeErr := decoded.Decode(r)

	// Map whatever the report carried, even when reading it failed: the
	// server applies a reference before answering for it, so status
	// lines decoded before a disconnect are authoritative for those
	// references. References without a line stay pending.
	//
	// A truncation before the unpack line carries no outcome at all: the
	// decode error is the whole answer there and every reference stays
	// pending rather than being declared rejected.
	var reportError error
	if decodeErr == nil || decoded.UnpackStatus != "" {
		reportError = applyDecodedReport(report, decoded)
	}

	if decodeErr != nil {
		return fmt.Errorf("decode report-status: %w", decodeErr)
	}

	if reportStatus > 0 && len(upreq.Commands) > 0 {
		_, err := io.ReadAll(r)
		if err != nil && !errors.Is(err, io.EOF) {
			_ = reader.Close()
			if reportError != nil {
				return reportError
			}
			return fmt.Errorf("reading progress messages: %w", err)
		}
	}

	if err := reader.Close(); err != nil {
		if reportError != nil {
			return reportError
		}
		return fmt.Errorf("closing reader: %w", err)
	}

	return reportError
}

// applyDecodedReport records the decoded report-status lines onto report.
//
// Git's receive-pack applies a reference before it answers "ok" for it, so
// every line decoded so far is authoritative even when the overall decode
// failed mid-report: entries already read keep their outcome while entries
// with no line stay pending. On an unpack failure the server reports no
// per-command lines and applied nothing, so every entry is rejected with
// the unpack reason.
//
// It returns the same error packp.ReportStatus.Error would return — the
// unpack error first, otherwise the first rejected command — so the
// transport error contract is unchanged when the full report was read.
func applyDecodedReport(report *PushReport, decoded *packp.ReportStatus) error {
	report.UnpackStatus = decoded.UnpackStatus

	// An empty status only reaches here on a complete decode of a
	// malformed server response (a disconnect before the unpack line is
	// handled by the caller and leaves everything pending); ReportStatus
	// has treated anything other than "ok" as an unpack failure.
	if decoded.UnpackStatus != reportOK {
		for i := range report.Refs {
			report.Refs[i].State = storer.RefUpdateRejected
			report.Refs[i].Reason = decoded.UnpackStatus
		}
		return packp.UnpackStatusErr{Status: decoded.UnpackStatus}
	}

	byName := make(map[plumbing.ReferenceName]string, len(decoded.CommandStatuses))
	for _, cs := range decoded.CommandStatuses {
		byName[cs.ReferenceName] = cs.Status
	}

	var firstErr error
	for i := range report.Refs {
		status, ok := byName[report.Refs[i].Name]
		if !ok {
			// No line for this command: leave it pending. This is the
			// truncated-report path; a complete report carries one
			// line per command.
			continue
		}
		if status == reportOK {
			report.Refs[i].State = storer.RefUpdateAccepted
			report.Refs[i].Reason = ""
			continue
		}
		report.Refs[i].State = storer.RefUpdateRejected
		report.Refs[i].Reason = status
		if firstErr == nil {
			firstErr = packp.CommandStatusErr{
				ReferenceName: report.Refs[i].Name,
				Status:        status,
			}
		}
	}

	return firstErr
}

func buildUpdateRequests(caps capability.List, req *PushRequest) *packp.UpdateRequests {
	upreq := &packp.UpdateRequests{}

	if caps.Supports(capability.ReportStatus) {
		upreq.Capabilities.Set(capability.ReportStatus)
	}
	if req.Progress != nil {
		if caps.Supports(capability.Sideband64k) {
			upreq.Capabilities.Set(capability.Sideband64k)
		} else if caps.Supports(capability.Sideband) {
			upreq.Capabilities.Set(capability.Sideband)
		}
		if req.Quiet && caps.Supports(capability.Quiet) {
			upreq.Capabilities.Set(capability.Quiet)
		}
	}
	if req.Atomic && caps.Supports(capability.Atomic) {
		upreq.Capabilities.Set(capability.Atomic)
	}
	if len(req.Options) > 0 && caps.Supports(capability.PushOptions) {
		upreq.Capabilities.Set(capability.PushOptions)
	}
	if caps.Supports(capability.Agent) {
		upreq.Capabilities.Set(capability.Agent, capability.DefaultAgent())
	}

	upreq.Commands = req.Commands
	return upreq
}
