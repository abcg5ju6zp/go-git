package git

import (
	"errors"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/utils/trace"
)

// PushRefState is the per-reference outcome of a push.
type PushRefState = storer.RefUpdateState

// Per-reference push outcomes. See storer.RefUpdateState for the contract.
var (
	PushRefPending  = storer.RefUpdatePending
	PushRefAccepted = storer.RefUpdateAccepted
	PushRefRejected = storer.RefUpdateRejected
)

// PushRefStatus is the outcome of pushing one reference.
type PushRefStatus struct {
	// Ref is the destination reference on the remote.
	Ref plumbing.ReferenceName
	// Old is the value the remote was expected to hold before the push.
	Old plumbing.Hash
	// New is the value requested by the push; zero for a delete.
	New plumbing.Hash
	// State is pending, accepted or rejected.
	State PushRefState
	// Reason is the server-provided rejection reason.
	Reason string
}

// PushResult is the per-reference result of a push, in command order.
type PushResult struct {
	Refs []PushRefStatus
}

// Status returns the outcome for ref and whether the reference was part
// of the push.
func (r *PushResult) Status(ref plumbing.ReferenceName) (PushRefStatus, bool) {
	for _, s := range r.Refs {
		if s.Ref == ref {
			return s, true
		}
	}
	return PushRefStatus{}, false
}

// Accepted returns the references the remote reported "ok" for.
func (r *PushResult) Accepted() []PushRefStatus {
	return r.inState(PushRefAccepted)
}

// Rejected returns the references the remote refused.
func (r *PushResult) Rejected() []PushRefStatus {
	return r.inState(PushRefRejected)
}

// Pending returns the updates sent without a definitive remote report.
func (r *PushResult) Pending() []PushRefStatus {
	return r.inState(PushRefPending)
}

func (r *PushResult) inState(state PushRefState) []PushRefStatus {
	var out []PushRefStatus
	for _, s := range r.Refs {
		if s.State == state {
			out = append(out, s)
		}
	}
	return out
}

// Error reports the first rejected reference as a
// packp.CommandStatusErr and is a convenience for callers consuming a
// complete result. The transport/decode error and an unpack failure are
// returned as the error value of PushContextResult/PushResult; prefer
// error-is/errors-as on that value for branching, and use this only when
// working from a result alone. Pending references never produce an error.
func (r *PushResult) Error() error {
	for _, s := range r.Refs {
		if s.State != PushRefRejected {
			continue
		}
		return packp.CommandStatusErr{
			ReferenceName: s.Ref,
			Status:        s.Reason,
		}
	}
	return nil
}

func newPushResult(report *transport.PushReport) *PushResult {
	result := &PushResult{}
	if report == nil {
		return result
	}
	result.Refs = make([]PushRefStatus, len(report.Refs))
	for i, s := range report.Refs {
		result.Refs[i] = PushRefStatus{
			Ref:    s.Name,
			Old:    s.Old,
			New:    s.New,
			State:  s.State,
			Reason: s.Reason,
		}
	}
	return result
}

// recoverPushTransaction applies the accepted outcomes of a previous push
// that never finalized (process exited, connection dropped) and returns
// the updates that were still unconfirmed. It runs before contacting the
// remote, so accepted tracking refs are repaired even when the new push
// later turns out to be a no-op.
//
// Accepted updates are applied once, with compare-and-set semantics, and
// removed from the journal. Pending updates are retained for confirmation
// against the fresh advertisement. Rejected updates need no local action
// and are dropped.
func (r *Remote) recoverPushTransaction(remoteName, remoteURL string) ([]storer.PushRefUpdate, time.Time, error) {
	pss, _ := r.s.(storer.PushStateStorer)
	if pss == nil {
		return nil, time.Time{}, nil
	}

	txn, err := pss.LoadPushTransaction(remoteName, remoteURL)
	if err != nil {
		trace.General.Printf("push: cannot load transaction for %s: %s", remoteURL, err)
		return nil, time.Time{}, nil
	}
	if txn == nil {
		return nil, time.Time{}, nil
	}

	var pending []storer.PushRefUpdate
	replayFailed := false
	for _, update := range txn.Updates {
		switch update.State {
		case storer.RefUpdateAccepted:
			if err := r.applyConfirmedTracking(update); err != nil {
				// A genuine storage failure (not the benign "tracking
				// moved on" case casTrackingReference swallows itself):
				// leave the journal untouched so the replay is retried,
				// but do not block this push — the live tracking write
				// at the end of the push still surfaces storage trouble
				// directly.
				trace.General.Printf("push: cannot replay accepted %s for %s: %s", update.Name, remoteURL, err)
				replayFailed = true
			}
		case storer.RefUpdatePending:
			pending = append(pending, update)
		}
	}

	if replayFailed {
		// Keep the stored transaction as-is so nothing durable is lost;
		// run this push without carried state.
		return nil, time.Time{}, nil
	}

	if len(pending) > 0 {
		txn.Updates = pending
		r.savePushTransaction(pss, txn)
	} else {
		r.deletePushTransaction(pss, remoteName, remoteURL)
	}

	return pending, txn.Started, nil
}

// confirmPendingFromAdvertisement resolves unconfirmed updates left by a
// previous push using the freshly fetched advertisement. An update whose
// requested end state is already on the remote (the ref holds New, or it
// is gone for a delete) is treated as accepted and applied locally; an
// update whose remote state differs stays unconfirmed and is carried into
// this push, where the normal command recomputation handles it with a
// fresh old value instead of blindly replaying the stale command.
func (r *Remote) confirmPendingFromAdvertisement(
	remoteName, remoteURL string,
	started time.Time,
	pending []storer.PushRefUpdate,
	remoteRefs storer.ReferenceStorer,
) ([]storer.PushRefUpdate, error) {
	if len(pending) == 0 {
		return nil, nil
	}

	pss, _ := r.s.(storer.PushStateStorer)

	var remaining []storer.PushRefUpdate
	for _, update := range pending {
		ref, err := remoteRefs.Reference(update.Name)
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			if update.IsDelete() {
				if err := r.applyConfirmedTracking(update); err != nil {
					return nil, err
				}
				continue
			}
			remaining = append(remaining, update)
			continue
		}
		if err != nil {
			remaining = append(remaining, update)
			continue
		}

		if !update.IsDelete() && ref.Type() == plumbing.HashReference && ref.Hash() == update.New {
			if err := r.applyConfirmedTracking(update); err != nil {
				return nil, err
			}
			continue
		}

		remaining = append(remaining, update)
	}

	// Persist the reduced set now: if the recomputation below finds
	// nothing to send and returns NoErrAlreadyUpToDate, the confirmed
	// entries are already applied and must not linger in the journal.
	if pss != nil {
		if len(remaining) > 0 {
			r.savePushTransaction(pss, &storer.PushTransaction{
				RemoteName: remoteName,
				RemoteURL:  remoteURL,
				Started:    started,
				Updates:    remaining,
			})
		} else {
			r.deletePushTransaction(pss, remoteName, remoteURL)
		}
	}

	return remaining, nil
}

// recordPendingPushTransaction durably records this push's commands as
// pending before anything is written to the wire, merged with updates a
// previous interrupted push could not confirm. A new command supersedes a
// carried pending update for the same reference. It returns the
// transaction's timestamp so later rewrites keep the original attempt
// time.
func (r *Remote) recordPendingPushTransaction(
	remoteName, remoteURL string,
	carried []storer.PushRefUpdate,
	cmds []*packp.Command,
) time.Time {
	started := time.Now()

	pss, _ := r.s.(storer.PushStateStorer)
	if pss == nil {
		return started
	}

	updates := mergePushUpdates(carried, cmds, storer.RefUpdatePending)
	r.savePushTransaction(pss, &storer.PushTransaction{
		RemoteName: remoteName,
		RemoteURL:  remoteURL,
		Started:    started,
		Updates:    updates,
	})
	return started
}

// recordPushOutcomes rewrites the journal with the definitive per-ref
// outcomes before any local tracking reference moves. That ordering is
// the crash-safety boundary: if the process dies between receiving the
// report and applying it locally, every accepted reference is still in
// the journal and the next push replays it.
func (r *Remote) recordPushOutcomes(
	remoteName, remoteURL string,
	started time.Time,
	carried []storer.PushRefUpdate,
	report *transport.PushReport,
) {
	pss, _ := r.s.(storer.PushStateStorer)
	if pss == nil {
		return
	}

	cmds := make([]*packp.Command, 0, len(report.Refs))
	states := make(map[plumbing.ReferenceName]storer.RefUpdateState, len(report.Refs))
	for _, ref := range report.Refs {
		states[ref.Name] = ref.State
		cmds = append(cmds, &packp.Command{Name: ref.Name, Old: ref.Old, New: ref.New})
	}

	updates := mergePushUpdates(carried, cmds, storer.RefUpdatePending)
	for i := range updates {
		if state, ok := states[updates[i].Name]; ok {
			updates[i].State = state
		}
	}

	r.savePushTransaction(pss, &storer.PushTransaction{
		RemoteName: remoteName,
		RemoteURL:  remoteURL,
		Started:    started,
		Updates:    updates,
	})
}

// mergePushUpdates combines carried pending updates with the new
// commands: carried entries keep their position, and a command for the
// same reference supersedes them.
func mergePushUpdates(
	carried []storer.PushRefUpdate,
	cmds []*packp.Command,
	initial storer.RefUpdateState,
) []storer.PushRefUpdate {
	updates := make([]storer.PushRefUpdate, 0, len(carried)+len(cmds))
	index := make(map[plumbing.ReferenceName]int, len(carried)+len(cmds))
	for _, update := range carried {
		index[update.Name] = len(updates)
		updates = append(updates, update)
	}
	for _, cmd := range cmds {
		update := storer.PushRefUpdate{
			Name:  cmd.Name,
			Old:   cmd.Old,
			New:   cmd.New,
			State: initial,
		}
		if pos, ok := index[cmd.Name]; ok {
			updates[pos] = update
			continue
		}
		index[cmd.Name] = len(updates)
		updates = append(updates, update)
	}
	return updates
}

// finalizePushTransaction drops the applied accepted updates and the
// terminal rejected ones, and keeps updates that never got a status
// line. A fully resolved transaction is deleted.
func (r *Remote) finalizePushTransaction(
	remoteName, remoteURL string,
	started time.Time,
	carried []storer.PushRefUpdate,
	cmds []*packp.Command,
	report *transport.PushReport,
) {
	pss, _ := r.s.(storer.PushStateStorer)
	if pss == nil {
		return
	}

	state := make(map[plumbing.ReferenceName]storer.RefUpdateState, len(report.Refs))
	for _, ref := range report.Refs {
		state[ref.Name] = ref.State
	}

	var remaining []storer.PushRefUpdate
	for _, update := range carried {
		if _, claimed := state[update.Name]; claimed {
			continue // re-sent as part of this push; its result decides
		}
		// An unclaimed carried update (this push's refspecs neither send
		// it nor did the advertisement confirm it) stays in the journal
		// until a later push resolves it. It never updates a tracking
		// ref by itself: confirmation requires an exact remote==New
		// match, so its retention is conservative, not destructive.
		remaining = append(remaining, update)
	}
	for _, cmd := range cmds {
		if state[cmd.Name] == storer.RefUpdatePending {
			remaining = append(remaining, storer.PushRefUpdate{
				Name:  cmd.Name,
				Old:   cmd.Old,
				New:   cmd.New,
				State: storer.RefUpdatePending,
			})
		}
	}

	if len(remaining) > 0 {
		r.savePushTransaction(pss, &storer.PushTransaction{
			RemoteName: remoteName,
			RemoteURL:  remoteURL,
			Started:    started,
			Updates:    remaining,
		})
		return
	}

	r.deletePushTransaction(pss, remoteName, remoteURL)
}

// applyAcceptedReport updates the local tracking references for exactly
// the references the remote reported "ok" for.
//
// Unlike journal replay (see applyConfirmedTracking), this is the live
// outcome of a push this process just sent: the report pairs with the
// exact Old→New command we wrote, and the remote confirmed it. The
// tracking reference is therefore set unconditionally, as before per-ref
// results existed. Crash protection does not come from this step but
// from recording the outcomes first; a crash here replays the same
// update through compare-and-set on the next push.
func (r *Remote) applyAcceptedReport(report *transport.PushReport) error {
	for _, ref := range report.Refs {
		if ref.State != storer.RefUpdateAccepted {
			continue
		}
		for _, spec := range r.c.Fetch {
			if !spec.Match(ref.Name) {
				continue
			}
			local := spec.Dst(ref.Name)
			if ref.New.IsZero() {
				if err := r.s.RemoveReference(local); err != nil {
					return err
				}
				continue
			}
			if err := r.s.SetReference(plumbing.NewHashReference(local, ref.New)); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyConfirmedTracking applies one confirmed remote update to the
// tracking references produced by the remote's fetch refspecs.
//
// The update is compare-and-set against the value the push began from
// (Old), so an update accepted by the remote never rolls a tracking ref
// backwards when a fetch or another push already advanced it: a tracking
// ref that holds a value different from Old is left untouched. Replays
// are idempotent (New already stored is a no-op) and therefore safe to
// run again after a crash between applying and journal cleanup.
func (r *Remote) applyConfirmedTracking(update storer.PushRefUpdate) error {
	for _, spec := range r.c.Fetch {
		if !spec.Match(update.Name) {
			continue
		}

		local := spec.Dst(update.Name)
		if err := r.casTrackingReference(local, update); err != nil {
			return err
		}
	}
	return nil
}

func (r *Remote) casTrackingReference(local plumbing.ReferenceName, update storer.PushRefUpdate) error {
	current, err := r.s.Reference(local)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		current = nil
	} else if err != nil {
		return err
	}

	// A delete confirmed remotely removes the tracking ref only while
	// it still reflects the value the push removed; a tracking ref that
	// moved on (fetch, a later push) is never deleted.
	if update.IsDelete() {
		if current == nil || current.Hash() != update.Old {
			return nil
		}
		return r.s.RemoveReference(local)
	}

	newRef := plumbing.NewHashReference(local, update.New)

	if current == nil {
		// Tracking ref absent: create it regardless of whether the
		// remote update was a create or an update — nothing newer can be
		// represented by a missing ref.
		return r.s.SetReference(newRef)
	}

	if current.Hash() == update.New {
		return nil // idempotent replay
	}

	if update.Old.IsZero() {
		// The remote update was a create: it expected the remote ref to
		// be absent, and a different tracking value here means local
		// knowledge moved on (a fetch observed another value). Do not
		// turn the create into an overwrite.
		trace.General.Printf(
			"push: keeping tracking ref %s at %s, confirmed create of %s not applied",
			local, current.Hash(), update.New,
		)
		return nil
	}

	oldRef := plumbing.NewHashReference(local, update.Old)
	if err := r.s.CheckAndSetReference(newRef, oldRef); err != nil {
		// The tracking ref changed since the push started (a fetch
		// observed a newer remote value, another concurrent push): the
		// newer local knowledge wins, matching what git does.
		if errors.Is(err, storage.ErrReferenceHasChanged) ||
			errors.Is(err, plumbing.ErrReferenceNotFound) {
			trace.General.Printf(
				"push: keeping tracking ref %s at %s, confirmed update to %s not applied",
				local, current.Hash(), update.New,
			)
			return nil
		}
		return err
	}
	return nil
}

func (r *Remote) savePushTransaction(pss storer.PushStateStorer, txn *storer.PushTransaction) {
	if err := pss.SavePushTransaction(txn); err != nil {
		trace.General.Printf("push: cannot persist transaction for %s: %s", txn.RemoteURL, err)
	}
}

func (r *Remote) deletePushTransaction(pss storer.PushStateStorer, remoteName, remoteURL string) {
	if err := pss.DeletePushTransaction(remoteName, remoteURL); err != nil {
		trace.General.Printf("push: cannot delete transaction for %s: %s", remoteURL, err)
	}
}
