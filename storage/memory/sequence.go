package memory

import (
	"slices"

	"github.com/go-git/go-git/v6/plumbing/storer"
)

// SequenceStorage implements storer.SequenceStateStorer for in-memory
// storage. The state lives as long as the Storage instance, so a
// Repository reopened over the same Storage can resume the sequence.
type SequenceStorage map[string][]byte

// SetSequenceState stores a copy of state under name. Storing an empty
// state removes any previously stored one.
func (s SequenceStorage) SetSequenceState(name string, state []byte) error {
	if len(state) == 0 {
		delete(s, name)
		return nil
	}
	s[name] = slices.Clone(state)
	return nil
}

// SequenceState returns a copy of the state stored under name.
func (s SequenceStorage) SequenceState(name string) ([]byte, error) {
	state, ok := s[name]
	if !ok {
		return nil, storer.ErrSequenceStateNotFound
	}
	return slices.Clone(state), nil
}

// DeleteSequenceState removes the state stored under name.
func (s SequenceStorage) DeleteSequenceState(name string) error {
	delete(s, name)
	return nil
}
