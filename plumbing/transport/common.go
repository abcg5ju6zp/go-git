package transport

import (
	"context"
	"io"
	"net"

	internal "github.com/go-git/go-git/v6/internal/transport"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// DialContextFunc is the function signature for dialing network connections.
// It also implements proxy.Dialer and proxy.ContextDialer so it can be
// passed directly to proxy.FromURL without an adapter.
type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Dial implements proxy.Dialer.
func (f DialContextFunc) Dial(network, addr string) (net.Conn, error) {
	return f(context.Background(), network, addr)
}

// DialContext implements proxy.ContextDialer.
func (f DialContextFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

// RemoteError represents an error returned by the remote.
// TODO: embed error
type RemoteError struct {
	Reason string
}

// Error implements the error interface.
func (e *RemoteError) Error() string {
	return e.Reason
}

// NewRemoteError creates a new RemoteError.
func NewRemoteError(reason string) error {
	return &RemoteError{Reason: reason}
}

// FetchRequest contains the parameters for a fetch-pack request.
// This is used during the pack negotiation phase of the fetch operation.
// See https://git-scm.com/docs/pack-protocol#_packfile_negotiation
// FetchRequest is the request sent to the remote to fetch objects. It is an
// alias of the shared internal type so the v0/v1 and v2 fetch paths use the
// exact same request.
type FetchRequest = internal.FetchRequest

// PushRequest contains the parameters for a push request.
type PushRequest struct {
	// Packfile is the packfile reader.
	Packfile io.ReadCloser

	// Commands is the list of ref update commands to send to the server.
	// The caller builds these from refspec matching against local and
	// remote refs, including force-push validation and fast-forward
	// checks. This matches canonical git's send-pack, which also
	// receives pre-built commands from the caller.
	Commands []*packp.Command

	// Progress is the progress sideband.
	Progress sideband.Progress

	// Options is a set of push-options to be sent to the server during push.
	Options []string

	// Atomic indicates an atomic push.
	// If the server supports atomic push, it will update the refs in one
	// atomic transaction. Either all refs are updated or none.
	Atomic bool

	// Quiet indicates whether the server should suppress human-readable
	// output.
	Quiet bool

	// Report, when non-nil, is filled with the per-reference outcome of
	// the push. It is initialized with one pending entry per command
	// before anything is written, and entries are promoted to accepted
	// or rejected as the remote report-status is read. If the report
	// cannot be read completely (disconnect, context cancellation,
	// protocol error), references without a status line stay pending and
	// the entries decoded so far are retained.
	Report *PushReport
}

// RefStatus is the outcome of one reference update within a push.
type RefStatus struct {
	// Name is the reference the update targets, as sent to the remote.
	Name plumbing.ReferenceName
	// Old is the value the remote was expected to hold before the update.
	Old plumbing.Hash
	// New is the requested new value; zero for a delete.
	New plumbing.Hash
	// State is pending, accepted or rejected.
	State storer.RefUpdateState
	// Reason is the server-provided rejection reason for rejected updates.
	Reason string
}

// PushReport is the per-reference result of a push. The Refs slice follows
// the order of PushRequest.Commands.
type PushReport struct {
	// UnpackStatus is the unpack line reported by the remote: "ok" on
	// success or the server-provided failure reason. It is empty when
	// the server did not negotiate report-status; such a server never
	// confirms references, so a clean exchange is optimistically
	// recorded as all accepted, matching the pre-report behavior.
	UnpackStatus string
	// Refs are the per-command outcomes in command order.
	Refs []RefStatus
}

// reset seeds the report with one pending entry per command.
func (r *PushReport) reset(cmds []*packp.Command) {
	r.UnpackStatus = ""
	r.Refs = make([]RefStatus, len(cmds))
	for i, cmd := range cmds {
		r.Refs[i] = RefStatus{
			Name:  cmd.Name,
			Old:   cmd.Old,
			New:   cmd.New,
			State: storer.RefUpdatePending,
		}
	}
}

// markAll sets every entry to state.
func (r *PushReport) markAll(state storer.RefUpdateState) {
	for i := range r.Refs {
		r.Refs[i].State = state
	}
}

// Accepted returns the references the remote reported "ok" for, in
// command order.
func (r *PushReport) Accepted() []RefStatus {
	return r.refsInState(storer.RefUpdateAccepted)
}

// Rejected returns the references the remote refused, in command order.
func (r *PushReport) Rejected() []RefStatus {
	return r.refsInState(storer.RefUpdateRejected)
}

// Pending returns the updates that were sent but never got a definitive
// status line, in command order.
func (r *PushReport) Pending() []RefStatus {
	return r.refsInState(storer.RefUpdatePending)
}

func (r *PushReport) refsInState(state storer.RefUpdateState) []RefStatus {
	var out []RefStatus
	for _, ref := range r.Refs {
		if ref.State == state {
			out = append(out, ref)
		}
	}
	return out
}
