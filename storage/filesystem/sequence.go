package filesystem

import (
	"os"

	"github.com/go-git/go-billy/v6"
	billyutil "github.com/go-git/go-billy/v6/util"

	"github.com/go-git/go-git/v6/plumbing/storer"
)

// SequencerDir is the directory, relative to the git directory, where
// multi-commit sequence state (such as cherry-pick) is persisted. It mirrors
// Git's own `.git/sequencer` directory.
const SequencerDir = "sequencer"

// SequenceStorage implements storer.SequenceStateStorer on top of the
// git directory. Each sequence is stored as a single file under
// `sequencer/<name>`, so the state survives the repository being closed
// and reopened.
type SequenceStorage struct {
	fs billy.Filesystem
}

// NewSequenceStorage returns a SequenceStorage rooted at the git directory fs.
func NewSequenceStorage(fs billy.Filesystem) SequenceStorage {
	return SequenceStorage{fs: fs}
}

func (s SequenceStorage) path(name string) string {
	return s.fs.Join(SequencerDir, name)
}

// SetSequenceState writes state to sequencer/<name>.
func (s SequenceStorage) SetSequenceState(name string, state []byte) error {
	if len(state) == 0 {
		return s.DeleteSequenceState(name)
	}
	return billyutil.WriteFile(s.fs, s.path(name), state, 0o644)
}

// SequenceState reads sequencer/<name>, returning
// storer.ErrSequenceStateNotFound when it does not exist.
func (s SequenceStorage) SequenceState(name string) ([]byte, error) {
	state, err := billyutil.ReadFile(s.fs, s.path(name))
	if err != nil {
		// The chroot filesystem wraps lookup errors in a way that does not
		// unwrap to os.ErrNotExist; os.IsNotExist, used throughout the
		// filesystem storage, recognises both shapes.
		if os.IsNotExist(err) {
			return nil, storer.ErrSequenceStateNotFound
		}
		return nil, err
	}
	return state, nil
}

// DeleteSequenceState removes sequencer/<name> if present.
func (s SequenceStorage) DeleteSequenceState(name string) error {
	err := s.fs.Remove(s.path(name))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
