package storer

import "errors"

// ErrSequenceStateNotFound is returned by SequenceStateStorer.SequenceState
// when no state has been stored under the requested name.
var ErrSequenceStateNotFound = errors.New("sequence state not found")

// SequenceStateStorer persists the state of an in-progress multi-commit
// sequence, such as a multi-commit cherry-pick (Git's "sequencer"). The
// state is opaque to the storer: it is written as a whole by the caller
// and must survive the storer being closed and reopened.
//
// A repository stores at most one state per sequence name. Storing an
// empty state clears it, equivalent to calling DeleteSequenceState.
type SequenceStateStorer interface {
	// SetSequenceState stores (or replaces) the state under name.
	SetSequenceState(name string, state []byte) error
	// SequenceState returns the stored state, or
	// ErrSequenceStateNotFound when none is stored.
	SequenceState(name string) ([]byte, error)
	// DeleteSequenceState removes the state under name. Removing a state
	// that does not exist is not an error.
	DeleteSequenceState(name string) error
}
