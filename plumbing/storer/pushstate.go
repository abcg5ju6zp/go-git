package storer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
)

// RefUpdateState is the per-reference outcome of a push while it is
// recorded by a [PushTransaction].
type RefUpdateState string

const (
	// RefUpdatePending means the update was sent but no definitive
	// report-status line for the reference has been received yet. A
	// disconnect, context cancellation or protocol error while the
	// report is being read leaves references in this state.
	RefUpdatePending RefUpdateState = "pending"
	// RefUpdateAccepted means the remote reported "ok" for the
	// reference: the ref update is durable on the remote side.
	RefUpdateAccepted RefUpdateState = "accepted"
	// RefUpdateRejected means the remote reported an "ng" line (or an
	// unpack failure refused the whole batch) for the reference.
	RefUpdateRejected RefUpdateState = "rejected"
)

// PushTransactionVersion is the on-disk layout version understood by this
// implementation.
const PushTransactionVersion = 1

// PushRefUpdate is one reference update inside a [PushTransaction].
//
// A zero New hash denotes a delete, matching packp.Command.Action.
type PushRefUpdate struct {
	Name   plumbing.ReferenceName `json:"name"`
	Old    plumbing.Hash          `json:"old"`
	New    plumbing.Hash          `json:"new"`
	State  RefUpdateState         `json:"state"`
	Reason string                 `json:"reason,omitempty"`
}

// IsDelete reports whether the update removes the reference (New is zero).
func (u PushRefUpdate) IsDelete() bool {
	return u.New.IsZero()
}

// MarshalJSON encodes hashes as hex strings. plumbing.ObjectID has
// unexported fields and would otherwise round-trip as an empty object,
// silently losing every update target.
func (u PushRefUpdate) MarshalJSON() ([]byte, error) {
	type wire struct {
		Name   plumbing.ReferenceName `json:"name"`
		Old    string                 `json:"old"`
		New    string                 `json:"new"`
		State  RefUpdateState         `json:"state"`
		Reason string                 `json:"reason,omitempty"`
	}
	return json.Marshal(wire{
		Name:   u.Name,
		Old:    u.Old.String(),
		New:    u.New.String(),
		State:  u.State,
		Reason: u.Reason,
	})
}

// UnmarshalJSON is the inverse of MarshalJSON.
func (u *PushRefUpdate) UnmarshalJSON(data []byte) error {
	type wire struct {
		Name   plumbing.ReferenceName `json:"name"`
		Old    string                 `json:"old"`
		New    string                 `json:"new"`
		State  RefUpdateState         `json:"state"`
		Reason string                 `json:"reason,omitempty"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}

	parse := func(s string) (plumbing.Hash, error) {
		if s == "" {
			return plumbing.ZeroHash, nil
		}
		h, ok := plumbing.FromHex(s)
		if !ok {
			return plumbing.ZeroHash, fmt.Errorf("invalid object id in push transaction: %q", s)
		}
		return h, nil
	}

	oldHash, err := parse(w.Old)
	if err != nil {
		return err
	}
	newHash, err := parse(w.New)
	if err != nil {
		return err
	}

	u.Name = w.Name
	u.Old = oldHash
	u.New = newHash
	u.State = w.State
	u.Reason = w.Reason
	return nil
}

// PushTransaction is the durable record of one push attempt, keyed by the
// remote name and URL it was sent to. It is the push counterpart of the
// in-memory per-ref report: a live push records every update as
// [RefUpdatePending] before the request is written, then promotes updates
// to [RefUpdateAccepted] or [RefUpdateRejected] as the remote report is
// read. A transaction that still holds pending updates when the process
// exits is recovered by the next push to the same remote.
type PushTransaction struct {
	Version    int             `json:"version"`
	RemoteName string          `json:"remote"`
	RemoteURL  string          `json:"url"`
	Started    time.Time       `json:"started"`
	Updates    []PushRefUpdate `json:"updates,omitempty"`
}

// PushStateStorer is an optional storer capability: storers that can
// persist push transactions durably implement it. The filesystem storage
// keeps the record inside the dotgit directory; the memory storage keeps
// it for the lifetime of the process. A storer that does not implement
// the interface makes push recovery best-effort within a single call and
// does not change any push error semantics.
type PushStateStorer interface {
	// LoadPushTransaction returns the stored transaction for the given
	// remote, or (nil, nil) when none exists. A corrupt record is
	// treated as absent.
	LoadPushTransaction(remoteName, remoteURL string) (*PushTransaction, error)
	// SavePushTransaction atomically creates or replaces the stored
	// transaction identified by txn.RemoteName and txn.RemoteURL.
	SavePushTransaction(txn *PushTransaction) error
	// DeletePushTransaction removes the stored transaction for the
	// given remote. A missing record is not an error.
	DeletePushTransaction(remoteName, remoteURL string) error
}

// PushTransactionKey returns the stable storage key for a remote name and
// URL pair. Hashing keeps the on-disk namespace independent of URL
// punctuation and credentials while staying deterministic, so a reopened
// repository addresses the same record.
func PushTransactionKey(remoteName, remoteURL string) string {
	sum := sha256.Sum256([]byte(remoteName + "\x00" + remoteURL))
	return hex.EncodeToString(sum[:])
}
