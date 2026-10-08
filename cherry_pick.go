package git

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/utils/merkletrie"
)

// cherryPickSequenceName is the key under which the cherry-pick
// sequencer state is stored in the storer.SequenceStateStorer. On a
// filesystem backend it lands at `.git/sequencer/cherry-pick`, the same
// directory Git itself uses.
const cherryPickSequenceName = "cherry-pick"

// Cherry-pick sequence errors.
var (
	// ErrCherryPickNoSequence is returned by Continue, Skip and Abort when
	// no cherry-pick sequence is in progress.
	ErrCherryPickNoSequence = errors.New("no cherry-pick sequence in progress")
	// ErrCherryPickInProgress is returned when a cherry-pick sequence is
	// started while another one is already in progress.
	ErrCherryPickInProgress = errors.New("cherry-pick sequence already in progress")
	// ErrCherryPickConflict is returned when a picked commit conflicts with
	// the current branch. The returned error wraps a CherryPickConflictError.
	ErrCherryPickConflict = errors.New("cherry-pick conflict")
	// ErrCherryPickUnresolvedConflicts is returned by Continue when the index
	// still contains unmerged conflict stages.
	ErrCherryPickUnresolvedConflicts = errors.New("cherry-pick conflicts must be resolved and staged before continuing")
	// ErrCherryPickEmpty is returned when a picked commit results in an empty
	// commit and empty commits were not allowed. The caller can Skip it or
	// Continue with AllowEmptyCommits set.
	ErrCherryPickEmpty = errors.New("cherry-pick produced an empty commit")
	// ErrCherryPickMainline is returned when cherry-picking a merge commit
	// whose requested mainline parent does not exist.
	ErrCherryPickMainline = errors.New("cherry-pick: invalid mainline parent for merge commit")
	// ErrSequenceStateUnsupported is returned when the storer does not
	// implement storer.SequenceStateStorer. The built-in memory and
	// filesystem storers both do.
	ErrSequenceStateUnsupported = errors.New("storer does not support durable sequence state")
)

// CherryPickConflictError describes the commit that could not be applied and
// the paths whose conflict stages were recorded in the index.
type CherryPickConflictError struct {
	// Commit is the commit that was being cherry-picked.
	Commit plumbing.Hash
	// Conflicts lists the conflicted paths, sorted.
	Conflicts []string
}

func (e *CherryPickConflictError) Error() string {
	return fmt.Sprintf("%s: %s conflicts: %s", ErrCherryPickConflict, e.Commit, strings.Join(e.Conflicts, ", "))
}

// Is reports whether the target is ErrCherryPickConflict.
func (e *CherryPickConflictError) Is(target error) bool {
	return target == ErrCherryPickConflict
}

// CherryPickEmptyError reports that the picked commit is empty once applied
// to the current branch (its changes are already present).
type CherryPickEmptyError struct {
	// Commit is the commit that was being cherry-picked.
	Commit plumbing.Hash
}

func (e *CherryPickEmptyError) Error() string {
	return fmt.Sprintf("%s: %s", ErrCherryPickEmpty, e.Commit)
}

// Is reports whether the target is ErrCherryPickEmpty.
func (e *CherryPickEmptyError) Is(target error) bool {
	return target == ErrCherryPickEmpty
}

// CherryPickState is a point-in-time view of an in-progress cherry-pick
// sequence.
type CherryPickState struct {
	// OriginalHead is the HEAD the sequence started from; Abort returns here.
	OriginalHead plumbing.Hash
	// Current is the commit being applied, or stopped on while waiting for
	// conflict resolution.
	Current plumbing.Hash
	// CurrentIndex is the zero-based position of Current in the sequence.
	CurrentIndex int
	// Total is the number of commits in the sequence.
	Total int
	// Stopped is true while the sequence waits for conflict resolution (or an
	// empty commit decision).
	Stopped bool
	// Conflicts lists the paths still unmerged when Stopped is true.
	Conflicts []string
	// Remaining lists Current and every not-yet-applied commit, in order.
	Remaining []plumbing.Hash
}

// cherryPickData is the durable representation of the sequence, stored in the
// storer.SequenceStateStorer. Hashes are kept as hex strings because
// plumbing.Hash carries unexported state and cannot round-trip through
// encoding/json.
type cherryPickData struct {
	Version           int               `json:"version"`
	OriginalHead      string            `json:"original_head"`
	Commits           []string          `json:"commits"`
	Current           int               `json:"current"`
	Stopped           bool              `json:"stopped"`
	AllowEmptyCommits bool              `json:"allow_empty_commits"`
	Strategy          int8              `json:"strategy"`
	MainlineParent    int               `json:"mainline_parent"`
	Committer         *object.Signature `json:"committer,omitempty"`
	Conflicts         []string          `json:"conflicts,omitempty"`
}

func (d *cherryPickData) originalHeadHash() plumbing.Hash {
	return plumbing.NewHash(d.OriginalHead)
}

func (d *cherryPickData) commitHash(i int) plumbing.Hash {
	return plumbing.NewHash(d.Commits[i])
}

// CherryPick cherry picks commits and merges them into the worktree based on
// the selected merge strategy. Each commit sits on the top of the worktree's
// current head. It resembles
// `git cherry-pick <commit-hash-1> <commit-hash-2> ... --strategy-option [theirs,ours]`.
//
// Before the first commit is applied the original HEAD and the full order of
// commits are recorded durably. The position is saved after every applied
// commit, so the sequence can be resumed with CherryPickContinue, advanced
// with CherryPickSkip or rolled back with CherryPickAbort, including after
// reopening the repository.
func (w *Worktree) CherryPick(commitOpts *CommitOptions, ortStrategyOption OrtMergeStrategyOption, commits ...*object.Commit) error {
	return w.CherryPickWithOptions(&CherryPickOptions{
		CommitOptions:  commitOpts,
		Strategy:       ortStrategyOption,
		MainlineParent: 1,
	}, commits...)
}

// CherryPickWithOptions starts a cherry-pick sequence, allowing the merge
// commit mainline parent to be selected.
func (w *Worktree) CherryPickWithOptions(opts *CherryPickOptions, commits ...*object.Commit) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	if len(commits) == 0 {
		return nil
	}

	if _, err := w.loadCherryPickData(); err == nil {
		return ErrCherryPickInProgress
	} else if !errors.Is(err, ErrCherryPickNoSequence) {
		return err
	}

	head, err := w.r.Head()
	if err != nil {
		return err
	}

	data := &cherryPickData{
		Version:           1,
		OriginalHead:      head.Hash().String(),
		Current:           0,
		Strategy:          int8(opts.Strategy),
		MainlineParent:    opts.MainlineParent,
		AllowEmptyCommits: opts.CommitOptions.AllowEmptyCommits,
		Committer:         opts.CommitOptions.Committer,
	}
	for _, c := range commits {
		data.Commits = append(data.Commits, c.Hash.String())
	}

	if err := w.saveCherryPickData(data); err != nil {
		return err
	}

	return w.runCherryPickSequence(data, opts.CommitOptions)
}

// CherryPickContinue resumes an in-progress cherry-pick sequence after a
// conflict. Every conflicted path must first be resolved in the worktree and
// staged (with Add or Remove). The resolved index is then committed using the
// picked commit's message and author, and the remaining commits are applied.
//
// commitOpts may be nil to reuse the committer stored when the sequence
// started; supply it to provide a signer or a different committer for the
// remaining commits.
//
// If a previous process left the sequence half-applied without stopping on a
// conflict, the worktree and index are first rebuilt from the current HEAD.
func (w *Worktree) CherryPickContinue(commitOpts *CommitOptions) error {
	data, err := w.loadCherryPickData()
	if err != nil {
		return err
	}

	live := cherryPickLiveOptions(data, commitOpts)

	if !data.Stopped {
		// The process went away while a commit was being applied. Rebuild the
		// clean pre-application state so it can be retried from scratch.
		head, err := w.r.Head()
		if err != nil {
			return err
		}
		if err := w.Reset(&ResetOptions{Mode: HardReset, Commit: head.Hash()}); err != nil {
			return err
		}
	}

	return w.runCherryPickSequence(data, live)
}

// CherryPickSkip discards the current commit of a stopped sequence and
// continues with the remaining commits. Worktree and index changes introduced
// by the skipped commit are discarded.
func (w *Worktree) CherryPickSkip(commitOpts *CommitOptions) error {
	data, err := w.loadCherryPickData()
	if err != nil {
		return err
	}
	if !data.Stopped {
		return fmt.Errorf("%w: sequence is not stopped on a commit", ErrCherryPickNoSequence)
	}

	live := cherryPickLiveOptions(data, commitOpts)

	head, err := w.r.Head()
	if err != nil {
		return err
	}
	if err := w.Reset(&ResetOptions{Mode: HardReset, Commit: head.Hash()}); err != nil {
		return err
	}

	data.Stopped = false
	data.Conflicts = nil
	data.Current++
	if err := w.saveCherryPickData(data); err != nil {
		return err
	}

	return w.runCherryPickSequence(data, live)
}

// CherryPickAbort abandons the in-progress cherry-pick sequence and restores
// the HEAD, index and worktree recorded before the sequence started. The
// durable sequence state is removed.
func (w *Worktree) CherryPickAbort() error {
	data, err := w.loadCherryPickData()
	if err != nil {
		return err
	}

	if err := w.Reset(&ResetOptions{Mode: HardReset, Commit: data.originalHeadHash()}); err != nil {
		return err
	}

	return w.clearCherryPickData()
}

// CherryPickState returns the state of the in-progress cherry-pick sequence,
// or nil when no sequence is in progress.
func (w *Worktree) CherryPickState() (*CherryPickState, error) {
	data, err := w.loadCherryPickData()
	if errors.Is(err, ErrCherryPickNoSequence) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	state := &CherryPickState{
		OriginalHead: data.originalHeadHash(),
		CurrentIndex: data.Current,
		Total:        len(data.Commits),
		Stopped:      data.Stopped,
		Conflicts:    append([]string(nil), data.Conflicts...),
	}
	if data.Current < len(data.Commits) {
		state.Current = data.commitHash(data.Current)
		state.Remaining = make([]plumbing.Hash, 0, len(data.Commits)-data.Current)
		for _, h := range data.Commits[data.Current:] {
			state.Remaining = append(state.Remaining, plumbing.NewHash(h))
		}
	}
	return state, nil
}

func cherryPickLiveOptions(data *cherryPickData, override *CommitOptions) *CommitOptions {
	if override != nil {
		return override
	}
	return &CommitOptions{
		Committer:         data.Committer,
		AllowEmptyCommits: data.AllowEmptyCommits,
	}
}

// runCherryPickSequence drives the sequence to completion (or to its next
// stop). On entry, data.Stopped==true means the current commit has been
// resolved in the index and only needs to be committed; otherwise the current
// commit is applied from scratch.
func (w *Worktree) runCherryPickSequence(data *cherryPickData, live *CommitOptions) error {
	if data.Stopped {
		picked, err := w.r.CommitObject(data.commitHash(data.Current))
		if err != nil {
			return err
		}
		if err := w.commitResolvedPick(picked, live); err != nil {
			if errors.Is(err, ErrEmptyCommit) {
				return &CherryPickEmptyError{Commit: picked.Hash}
			}
			return err
		}

		data.Stopped = false
		data.Conflicts = nil
		data.Current++
		if err := w.saveCherryPickData(data); err != nil {
			return err
		}
	}

	for data.Current < len(data.Commits) {
		picked, err := w.r.CommitObject(data.commitHash(data.Current))
		if err != nil {
			return err
		}

		// Record the position before touching the worktree: a process that
		// dies while applying this commit finds the same position on reopen.
		data.Stopped = false
		data.Conflicts = nil
		if err := w.saveCherryPickData(data); err != nil {
			return err
		}

		stopped, err := w.applyPick(data, picked, live)
		if err != nil {
			return err
		}
		if stopped != nil {
			data.Stopped = true
			data.Conflicts = stopped.conflicts
			if err := w.saveCherryPickData(data); err != nil {
				return err
			}
			return stopped.err
		}

		data.Current++
		if err := w.saveCherryPickData(data); err != nil {
			return err
		}
	}

	return w.clearCherryPickData()
}

// pickStop is the outcome of applying a single commit that needs the sequence
// to pause: either a conflict or an empty result.
type pickStop struct {
	conflicts []string
	err       error
}

// applyPick applies one picked commit. A nil pickStop means the commit was
// created; otherwise the sequence must pause.
func (w *Worktree) applyPick(data *cherryPickData, commit *object.Commit, live *CommitOptions) (*pickStop, error) {
	strategy := OrtMergeStrategyOption(data.Strategy)
	if strategy == TheirsMergeStrategy || strategy == OursMergeStrategy {
		if err := w.applyForcedPick(commit, strategy, live); err != nil {
			if errors.Is(err, ErrEmptyCommit) {
				return &pickStop{err: &CherryPickEmptyError{Commit: commit.Hash}}, nil
			}
			return nil, err
		}
		return nil, nil
	}

	return w.applyThreeWayPick(data, commit, live)
}

// applyForcedPick keeps the historical theirs/ours behaviour: materialise the
// full tree diff for the chosen side and commit the result. Conflicts are
// always auto-resolved in favour of the chosen side.
func (w *Worktree) applyForcedPick(commit *object.Commit, strategy OrtMergeStrategyOption, live *CommitOptions) error {
	cfg, err := w.r.Config()
	if err != nil {
		return err
	}

	// Materialise changes through the same validating filesystem and
	// checkout path as reset/checkout, so cherry-pick shares their
	// leading-symlink handling, mode awareness (symlinks, exec bits,
	// CRLF) and root reuse instead of writing raw bytes via Create.
	fs, closeFS := w.reusableRootFS()
	defer closeFS()

	headRef, err := w.r.Head()
	if err != nil {
		return err
	}
	headCommit, err := w.r.CommitObject(headRef.Hash())
	if err != nil {
		return err
	}
	currentTree, err := headCommit.Tree()
	if err != nil {
		return err
	}

	commitTree, err := commit.Tree()
	if err != nil {
		return err
	}

	var changes object.Changes
	switch strategy {
	case TheirsMergeStrategy:
		changes, err = currentTree.Diff(commitTree)
	case OursMergeStrategy:
		changes, err = commitTree.Diff(currentTree)
	}
	if err != nil {
		return err
	}

	for _, change := range changes {
		action, err := change.Action()
		if err != nil {
			return err
		}

		switch action {
		case merkletrie.Delete:
			if _, err := w.Remove(change.From.Name); err != nil {
				return err
			}
		case merkletrie.Insert, merkletrie.Modify:
			_, to, err := change.Files()
			if err != nil {
				return err
			}
			if to == nil {
				continue
			}
			// change.Files names the *File after the tree leaf. The
			// worktree write needs the full path so it lands at the
			// right location and is validated by the wrapper.
			to.Name = change.To.Name
			if err := w.checkoutFile(cfg, fs, to); err != nil {
				return err
			}
			if _, err := w.Add(to.Name); err != nil {
				return err
			}
		}
	}

	return w.commitPicked(commit, live)
}

// applyThreeWayPick performs a standard three-way merge of one commit. Clean
// changes are applied immediately; conflicting paths get base/ours/theirs
// stages in the index and conflict markers in the worktree, and a pickStop is
// returned. An empty result pauses with a CherryPickEmptyError.
func (w *Worktree) applyThreeWayPick(data *cherryPickData, commit *object.Commit, live *CommitOptions) (*pickStop, error) {
	cfg, err := w.r.Config()
	if err != nil {
		return nil, err
	}

	headRef, err := w.r.Head()
	if err != nil {
		return nil, err
	}
	headCommit, err := w.r.CommitObject(headRef.Hash())
	if err != nil {
		return nil, err
	}
	oursTree, err := headCommit.Tree()
	if err != nil {
		return nil, err
	}
	theirsTree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	baseTree, err := w.cherryPickBaseTree(data, commit)
	if err != nil {
		return nil, err
	}

	// No rename detection: each change is a single path, which is what the
	// three-way per-path rules below require.
	changes, err := object.DiffTree(baseTree, theirsTree)
	if err != nil {
		return nil, err
	}

	fs, closeFS := w.reusableRootFS()
	defer closeFS()

	var conflicts []conflictRecord
	for _, ch := range changes {
		action, err := ch.Action()
		if err != nil {
			return nil, err
		}

		var baseEntry, theirsEntry *object.TreeEntry
		if ch.From != (object.ChangeEntry{}) {
			baseEntry = &ch.From.TreeEntry
		}
		if ch.To != (object.ChangeEntry{}) {
			theirsEntry = &ch.To.TreeEntry
		}
		// Submodule gitlinks are outside the file merge rules; the legacy
		// pick skipped them too.
		if baseEntry != nil && baseEntry.Mode == filemode.Submodule {
			continue
		}
		if theirsEntry != nil && theirsEntry.Mode == filemode.Submodule {
			continue
		}

		name := ch.To.Name
		if name == "" {
			name = ch.From.Name
		}
		oursEntry := treeEntryOrNil(oursTree, name)

		switch action {
		case merkletrie.Delete:
			switch {
			case oursEntry == nil, sameTreeEntry(oursEntry, baseEntry):
				// We did not touch it (or deleted it already): take the
				// deletion.
				if oursEntry != nil {
					if _, err := w.Remove(name); err != nil {
						return nil, err
					}
				}
			default:
				// We modified a file they deleted.
				conflicts = append(conflicts, conflictRecord{
					name: name, base: baseEntry, ours: oursEntry,
					worktreeKept: true,
				})
			}

		case merkletrie.Insert:
			switch {
			case oursEntry == nil:
				if err := w.checkoutTheirs(cfg, fs, theirsTree, name, theirsEntry); err != nil {
					return nil, err
				}
			case sameTreeEntry(oursEntry, theirsEntry):
				// Already have the exact content: nothing to do.
			default:
				// Both sides added the path differently.
				rec := conflictRecord{name: name, ours: oursEntry, theirs: theirsEntry}
				if err := w.stageContentConflict(rec, commit.Hash, baseTree, oursTree, theirsTree, fs); err != nil {
					return nil, err
				}
				conflicts = append(conflicts, rec)
			}

		case merkletrie.Modify:
			switch {
			case oursEntry == nil:
				// We deleted a file they modified.
				conflicts = append(conflicts, conflictRecord{
					name: name, base: baseEntry, theirs: theirsEntry,
				})
			case sameTreeEntry(oursEntry, baseEntry):
				// Only they touched it: take their version.
				if err := w.checkoutTheirs(cfg, fs, theirsTree, name, theirsEntry); err != nil {
					return nil, err
				}
			case sameTreeEntry(oursEntry, theirsEntry):
				// Our version already contains their end state.
			default:
				// Both sides changed the path: try an automatic content
				// merge, otherwise record a content conflict.
				rec := conflictRecord{name: name, base: baseEntry, ours: oursEntry, theirs: theirsEntry}
				merged, mergedOK, err := w.autoMergeContent(rec, baseTree, oursTree, theirsTree)
				if err != nil {
					return nil, err
				}
				if mergedOK {
					if err := w.writeMergedFile(fs, name, merged, oursEntry.Mode); err != nil {
						return nil, err
					}
					if _, err := w.Add(name); err != nil {
						return nil, err
					}
				} else {
					if err := w.stageContentConflict(rec, commit.Hash, baseTree, oursTree, theirsTree, fs); err != nil {
						return nil, err
					}
					conflicts = append(conflicts, rec)
				}
			}
		}
	}

	if len(conflicts) == 0 {
		if err := w.commitPicked(commit, live); err != nil {
			if errors.Is(err, ErrEmptyCommit) {
				return &pickStop{err: &CherryPickEmptyError{Commit: commit.Hash}}, nil
			}
			return nil, err
		}
		return nil, nil
	}

	names, err := w.recordConflictStages(conflicts)
	if err != nil {
		return nil, err
	}
	return &pickStop{
		conflicts: names,
		err:       &CherryPickConflictError{Commit: commit.Hash, Conflicts: names},
	}, nil
}

// conflictRecord carries the three tree entries involved in a conflict and
// whether the worktree file already holds the version to keep on disk.
type conflictRecord struct {
	name         string
	base         *object.TreeEntry
	ours         *object.TreeEntry
	theirs       *object.TreeEntry
	worktreeKept bool
}

// cherryPickBaseTree returns the tree the picked commit should be merged
// against: the selected parent of a merge commit, the only parent of an
// ordinary commit, or nil (empty tree) for a root commit.
func (w *Worktree) cherryPickBaseTree(data *cherryPickData, commit *object.Commit) (*object.Tree, error) {
	switch commit.NumParents() {
	case 0:
		return nil, nil
	case 1:
		parent, err := commit.Parent(0)
		if err != nil {
			return nil, err
		}
		return parent.Tree()
	default:
		idx := data.MainlineParent - 1
		if idx < 0 || idx >= commit.NumParents() {
			return nil, fmt.Errorf("%w: commit %s has %d parents", ErrCherryPickMainline, commit.Hash, commit.NumParents())
		}
		parent, err := commit.Parent(idx)
		if err != nil {
			return nil, err
		}
		return parent.Tree()
	}
}

func treeEntryOrNil(t *object.Tree, name string) *object.TreeEntry {
	if t == nil {
		return nil
	}
	e, err := t.FindEntry(name)
	if err != nil {
		return nil
	}
	return e
}

func sameTreeEntry(a, b *object.TreeEntry) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Hash == b.Hash && a.Mode == b.Mode
}

// checkoutTheirs writes the incoming version of a cleanly applied path and
// stages it.
func (w *Worktree) checkoutTheirs(cfg *config.Config, fs *worktreeFilesystem, theirsTree *object.Tree, name string, _ *object.TreeEntry) error {
	f, err := theirsTree.File(name)
	if err != nil {
		return err
	}
	if err := w.checkoutFile(cfg, fs, f); err != nil {
		return err
	}
	_, err = w.Add(name)
	return err
}

// autoMergeContent tries to merge the three versions of a regular text file.
// It returns merged=false when the file is binary, changes type, or contains
// overlapping edits.
func (w *Worktree) autoMergeContent(
	rec conflictRecord,
	baseTree, oursTree, theirsTree *object.Tree,
) (merged []byte, ok bool, err error) {
	if !mergeableMode(rec.base.Mode, rec.ours.Mode, rec.theirs.Mode) {
		return nil, false, nil
	}

	baseBytes, err := treeFileContents(baseTree, rec.name)
	if err != nil {
		return nil, false, err
	}
	oursBytes, err := treeFileContents(oursTree, rec.name)
	if err != nil {
		return nil, false, err
	}
	theirsBytes, err := treeFileContents(theirsTree, rec.name)
	if err != nil {
		return nil, false, err
	}

	if isBinaryContent(baseBytes) || isBinaryContent(oursBytes) || isBinaryContent(theirsBytes) {
		return nil, false, nil
	}

	dmp := diffmatchpatch.New()
	dmp.DiffEditCost = 11

	// Apply both sides of the diff independently. A patch that replaces a
	// whole region carries no context to mismatch, so checking only one
	// direction would accept conflicting full-file rewrites. Require both
	// applications to be exact and to converge on the same result, as Git's
	// xdl merge does when intersecting hunks.
	fromTheirs, theirsApplied := dmp.PatchApply(
		dmp.PatchMake(string(baseBytes), string(theirsBytes)),
		string(oursBytes),
	)
	for _, clean := range theirsApplied {
		if !clean {
			return nil, false, nil
		}
	}
	fromOurs, oursApplied := dmp.PatchApply(
		dmp.PatchMake(string(baseBytes), string(oursBytes)),
		string(theirsBytes),
	)
	for _, clean := range oursApplied {
		if !clean {
			return nil, false, nil
		}
	}
	if fromTheirs != fromOurs {
		return nil, false, nil
	}
	return []byte(fromTheirs), true, nil
}

// stageContentConflict prepares the worktree for a content conflict: the
// file on disk gets standard conflict markers for text files, or keeps our
// version for binary/type conflicts. The index stages themselves are written
// later in a single pass by recordConflictStages.
func (w *Worktree) stageContentConflict(
	rec conflictRecord,
	label plumbing.Hash,
	_, oursTree, theirsTree *object.Tree,
	fs *worktreeFilesystem,
) error {
	if rec.worktreeKept {
		return nil
	}

	mode := rec.ours.Mode
	if mode == 0 {
		mode = rec.theirs.Mode
	}
	if !mode.IsFile() || mode == filemode.Symlink {
		// Leave our file (or absence) untouched for links/type changes.
		return nil
	}

	oursBytes, err := treeFileContents(oursTree, rec.name)
	if err != nil {
		return err
	}
	theirsBytes, err := treeFileContents(theirsTree, rec.name)
	if err != nil {
		return err
	}
	if isBinaryContent(oursBytes) || isBinaryContent(theirsBytes) {
		// Binary conflicts keep our content in the worktree.
		return nil
	}

	markers := buildConflictMarkers(oursBytes, theirsBytes, label)
	if err := w.writeMergedFile(fs, rec.name, markers, mode); err != nil {
		return err
	}
	return nil
}

// recordConflictStages replaces the index entry of every conflicted path with
// its 1/2/3 stages and returns the conflicted path names in order.
func (w *Worktree) recordConflictStages(records []conflictRecord) ([]string, error) {
	idx, err := w.r.Storer.Index()
	if err != nil {
		return nil, err
	}

	byName := make(map[string]struct{}, len(records))
	for _, rec := range records {
		byName[rec.name] = struct{}{}
	}

	entries := idx.Entries[:0]
	for _, e := range idx.Entries {
		if _, ok := byName[e.Name]; !ok {
			entries = append(entries, e)
		}
	}

	for _, rec := range records {
		addStage := func(stage index.Stage, e *object.TreeEntry) {
			if e == nil || e.Hash.IsZero() {
				return
			}
			entries = append(entries, &index.Entry{
				Name:  rec.name,
				Hash:  e.Hash,
				Mode:  e.Mode,
				Stage: stage,
			})
		}
		addStage(index.AncestorMode, rec.base)
		addStage(index.OurMode, rec.ours)
		addStage(index.TheirMode, rec.theirs)
	}
	idx.Entries = entries

	if err := w.r.Storer.SetIndex(idx); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(records))
	for _, rec := range records {
		names = append(names, rec.name)
	}
	return names, nil
}

func mergeableMode(modes ...filemode.FileMode) bool {
	for _, m := range modes {
		if m != filemode.Regular && m != filemode.Executable && m != filemode.Deprecated {
			return false
		}
	}
	return true
}

func treeFileContents(t *object.Tree, name string) ([]byte, error) {
	if t == nil {
		return nil, nil
	}
	f, err := t.File(name)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) {
			return nil, nil
		}
		return nil, err
	}
	content, err := f.Contents()
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}

func isBinaryContent(b []byte) bool {
	return bytes.IndexByte(b, 0) >= 0
}

func buildConflictMarkers(ours, theirs []byte, theirsCommit plumbing.Hash) []byte {
	var b bytes.Buffer
	b.WriteString("<<<<<<< HEAD\n")
	b.Write(ours)
	if len(ours) > 0 && !bytes.HasSuffix(ours, []byte("\n")) {
		b.WriteByte('\n')
	}
	b.WriteString("=======\n")
	b.Write(theirs)
	if len(theirs) > 0 && !bytes.HasSuffix(theirs, []byte("\n")) {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, ">>>>>>> %s\n", theirsCommit)
	return b.Bytes()
}

// writeMergedFile writes content to the worktree through the validating
// filesystem wrapper, mirroring checkoutFile for regular files.
func (w *Worktree) writeMergedFile(fs *worktreeFilesystem, name string, content []byte, mode filemode.FileMode) error {
	if err := w.clearBlockingSymlinks(fs, name); err != nil {
		return err
	}
	osMode, err := mode.ToOSFileMode()
	if err != nil {
		return err
	}
	f, err := fs.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, osMode.Perm())
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// commitResolvedPick commits an index that the user prepared while resolving
// a stopped pick.
func (w *Worktree) commitResolvedPick(picked *object.Commit, live *CommitOptions) error {
	idx, err := w.r.Storer.Index()
	if err != nil {
		return err
	}
	for _, e := range idx.Entries {
		if e.Stage > 0 {
			return fmt.Errorf("%w: %s", ErrCherryPickUnresolvedConflicts, e.Name)
		}
	}
	return w.commitPicked(picked, live)
}

// commitPicked creates the picked commit, preserving the original author and
// message and taking the committer/signer from the live options.
func (w *Worktree) commitPicked(picked *object.Commit, live *CommitOptions) error {
	_, err := w.Commit(picked.Message, &CommitOptions{
		Author:            &picked.Author,
		Committer:         live.Committer,
		Signer:            live.Signer,
		AllowEmptyCommits: live.AllowEmptyCommits,
	})
	return err
}

// --- durable sequencer state ---

func (w *Worktree) sequenceStore() (storer.SequenceStateStorer, error) {
	s, ok := w.r.Storer.(storer.SequenceStateStorer)
	if !ok {
		return nil, ErrSequenceStateUnsupported
	}
	return s, nil
}

func (w *Worktree) loadCherryPickData() (*cherryPickData, error) {
	s, err := w.sequenceStore()
	if err != nil {
		return nil, err
	}
	raw, err := s.SequenceState(cherryPickSequenceName)
	if err != nil {
		if errors.Is(err, storer.ErrSequenceStateNotFound) {
			return nil, ErrCherryPickNoSequence
		}
		return nil, err
	}

	var data cherryPickData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("read cherry-pick sequence state: %w", err)
	}
	if data.MainlineParent <= 0 {
		data.MainlineParent = 1
	}
	return &data, nil
}

func (w *Worktree) saveCherryPickData(data *cherryPickData) error {
	s, err := w.sequenceStore()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.SetSequenceState(cherryPickSequenceName, raw)
}

func (w *Worktree) clearCherryPickData() error {
	s, err := w.sequenceStore()
	if err != nil {
		return err
	}
	return s.DeleteSequenceState(cherryPickSequenceName)
}
