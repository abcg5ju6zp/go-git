package git

import (
	"context"
	"io"
	"testing"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-billy/v6/osfs"
	"github.com/go-git/go-billy/v6/util"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/memory"
)

// cpEnv is a fully in-memory repository ready for cherry-pick tests.
type cpEnv struct {
	t  *testing.T
	r  *Repository
	w  *Worktree
	fs billy.Filesystem
}

func newCPEnv(t *testing.T) *cpEnv {
	t.Helper()
	fs := memfs.New()
	r, err := Init(memory.NewStorage(), WithWorkTree(fs))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	w, err := r.Worktree()
	require.NoError(t, err)
	return &cpEnv{t: t, r: r, w: w, fs: fs}
}

func (e *cpEnv) write(name, content string) {
	e.t.Helper()
	require.NoError(e.t, util.WriteFile(e.fs, name, []byte(content), 0o644))
}

func (e *cpEnv) add(name string) {
	e.t.Helper()
	_, err := e.w.Add(name)
	require.NoError(e.t, err)
}

func (e *cpEnv) commit(msg string) plumbing.Hash {
	e.t.Helper()
	h, err := e.w.Commit(msg, &CommitOptions{Author: defaultSignature()})
	require.NoError(e.t, err)
	return h
}

func (e *cpEnv) writeCommit(name, content, msg string) plumbing.Hash {
	e.write(name, content)
	e.add(name)
	return e.commit(msg)
}

func (e *cpEnv) checkout(h plumbing.Hash) {
	e.t.Helper()
	require.NoError(e.t, e.w.Checkout(&CheckoutOptions{Hash: h}))
}

func (e *cpEnv) commitObject(h plumbing.Hash) *object.Commit {
	e.t.Helper()
	c, err := e.r.CommitObject(h)
	require.NoError(e.t, err)
	return c
}

func (e *cpEnv) readFile(name string) string {
	e.t.Helper()
	b, err := util.ReadFile(e.fs, name)
	require.NoError(e.t, err)
	return string(b)
}

func (e *cpEnv) headTreeFile(name string) string {
	e.t.Helper()
	head, err := e.r.Head()
	require.NoError(e.t, err)
	c, err := e.r.CommitObject(head.Hash())
	require.NoError(e.t, err)
	tree, err := c.Tree()
	require.NoError(e.t, err)
	f, err := tree.File(name)
	require.NoError(e.t, err)
	content, err := f.Contents()
	require.NoError(e.t, err)
	return content
}

func (e *cpEnv) headHash() plumbing.Hash {
	e.t.Helper()
	head, err := e.r.Head()
	require.NoError(e.t, err)
	return head.Hash()
}

// headTreeFileMissing asserts the path is absent from the HEAD tree.
func (e *cpEnv) headTreeFileMissing(name string) {
	e.t.Helper()
	head, err := e.r.Head()
	require.NoError(e.t, err)
	c, err := e.r.CommitObject(head.Hash())
	require.NoError(e.t, err)
	tree, err := c.Tree()
	require.NoError(e.t, err)
	_, err = tree.File(name)
	require.ErrorIs(e.t, err, object.ErrFileNotFound)
}

// rawMergeCommit creates a merge commit object with the given tree and
// parents, without touching HEAD.
func (e *cpEnv) rawMergeCommit(treeHash plumbing.Hash, parents []plumbing.Hash, msg string) plumbing.Hash {
	e.t.Helper()
	sig := defaultSignature()
	commit := &object.Commit{
		Author:       *sig,
		Committer:    *sig,
		Message:      msg,
		TreeHash:     treeHash,
		ParentHashes: parents,
	}
	obj := e.r.Storer.NewEncodedObject()
	require.NoError(e.t, commit.Encode(obj))
	h, err := e.r.Storer.SetEncodedObject(obj)
	require.NoError(e.t, err)
	return h
}

// indexStages groups index entries for a path by stage.
func indexStages(t *testing.T, r *Repository, path string) map[index.Stage]plumbing.Hash {
	t.Helper()
	idx, err := r.Storer.Index()
	require.NoError(t, err)
	stages := make(map[index.Stage]plumbing.Hash)
	for _, e := range idx.Entries {
		if e.Name == path {
			stages[e.Stage] = e.Hash
		}
	}
	return stages
}

// buildConflictChain creates a three-commit chain on top of base:
// C1 adds "g", Cf changes "f", C3 adds "h".
func buildConflictChain(e *cpEnv, base plumbing.Hash, cherryF string) (c1, cf, c3 plumbing.Hash) {
	e.checkout(base)
	c1 = e.writeCommit("g", "g1\n", "pick one\n")
	cf = e.writeCommit("f", cherryF, "pick conflict\n")
	c3 = e.writeCommit("h", "h1\n", "pick three\n")
	return c1, cf, c3
}

func TestCherryPickSequenceConflictContinue(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)

	init := e.writeCommit("f", "line\n", "init\n")
	c1, cf, c3 := buildConflictChain(e, init, "cherry\n")

	// Diverge on the same line of f.
	e.checkout(init)
	head := e.writeCommit("f", "head\n", "head side\n")

	err := e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy,
		e.commitObject(c1), e.commitObject(cf), e.commitObject(c3),
	)
	var conflict *CherryPickConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, cf, conflict.Commit)
	require.Equal(t, []string{"f"}, conflict.Conflicts)

	// The first commit landed; the sequence is stopped on the second one.
	require.NotEqual(t, head, e.headHash())

	st, err := e.w.CherryPickState()
	require.NoError(t, err)
	require.True(t, st.Stopped)
	require.Equal(t, head, st.OriginalHead)
	require.Equal(t, cf, st.Current)
	require.Equal(t, 1, st.CurrentIndex)
	require.Equal(t, 3, st.Total)
	require.Equal(t, []plumbing.Hash{cf, c3}, st.Remaining)
	require.Equal(t, []string{"f"}, st.Conflicts)

	// Conflict stages 1/2/3 are recorded for f.
	stages := indexStages(t, e.r, "f")
	require.Len(t, stages, 3)
	require.Contains(t, stages, index.AncestorMode)
	require.Contains(t, stages, index.OurMode)
	require.Contains(t, stages, index.TheirMode)

	// The clean change of the first pick is already staged and on disk.
	require.Equal(t, "g1\n", e.readFile("g"))

	// Markers are in the worktree.
	marked := e.readFile("f")
	require.Contains(t, marked, "<<<<<<< HEAD")
	require.Contains(t, marked, "head")
	require.Contains(t, marked, "cherry")
	require.Contains(t, marked, ">>>>>>>")

	// Starting another sequence while one is in progress is rejected.
	err = e.w.CherryPick(
		&CommitOptions{Author: defaultSignature()},
		DefaultMergeStrategy, e.commitObject(c3),
	)
	require.ErrorIs(t, err, ErrCherryPickInProgress)

	// Continuing without resolving is rejected.
	require.ErrorIs(t, e.w.CherryPickContinue(nil), ErrCherryPickUnresolvedConflicts)

	// Resolve and continue.
	e.write("f", "resolved\n")
	e.add("f")
	require.NoError(t, e.w.CherryPickContinue(nil))

	// Everything landed and the sequence state is gone.
	st, err = e.w.CherryPickState()
	require.NoError(t, err)
	require.Nil(t, st)

	require.Equal(t, "resolved\n", e.headTreeFile("f"))
	require.Equal(t, "g1\n", e.headTreeFile("g"))
	require.Equal(t, "h1\n", e.headTreeFile("h"))

	// The resolved picks kept the original commit messages and authors.
	picked := e.commitObject(e.headHash())
	require.Equal(t, "pick three\n", picked.Message)
	parent, err := picked.Parent(0)
	require.NoError(t, err)
	require.Equal(t, "pick conflict\n", parent.Message)
	require.Equal(t, e.commitObject(cf).Author, parent.Author)

	// No unmerged stages remain.
	idx, err := e.r.Storer.Index()
	require.NoError(t, err)
	for _, entry := range idx.Entries {
		require.Zero(t, entry.Stage, "unexpected stage for %s", entry.Name)
	}
}

func TestCherryPickSequenceCleanAutoMerge(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)

	init := e.writeCommit("f", "l1\nl2\nl3\n", "init\n")
	e.checkout(init)
	cherry := e.writeCommit("f", "L1\nl2\nl3\n", "cherry changes line 1\n")

	e.checkout(init)
	e.writeCommit("f", "l1\nl2\nL3\n", "head changes line 3\n")

	err := e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy, e.commitObject(cherry),
	)
	require.NoError(t, err)

	require.Equal(t, "L1\nl2\nL3\n", e.headTreeFile("f"))
	st, err := e.w.CherryPickState()
	require.NoError(t, err)
	require.Nil(t, st)
}

func TestCherryPickSequenceAbort(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)

	init := e.writeCommit("f", "line\n", "init\n")
	c1, cf, _ := buildConflictChain(e, init, "cherry\n")

	e.checkout(init)
	head := e.writeCommit("f", "head\n", "head side\n")

	err := e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy,
		e.commitObject(c1), e.commitObject(cf),
	)
	require.ErrorAs(t, err, new(*CherryPickConflictError))

	require.NoError(t, e.w.CherryPickAbort())

	// HEAD, index and worktree are back to the pre-sequence state.
	require.Equal(t, head, e.headHash())
	require.Equal(t, "head\n", e.readFile("f"))
	_, err = util.ReadFile(e.fs, "g")
	require.Error(t, err)

	st, err := e.w.CherryPickState()
	require.NoError(t, err)
	require.Nil(t, st)

	// A fresh sequence can start right after aborting.
	require.NoError(t, e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy, e.commitObject(c1),
	))
}

func TestCherryPickSequenceSkip(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)

	init := e.writeCommit("f", "line\n", "init\n")
	c1, cf, _ := buildConflictChain(e, init, "cherry\n")

	e.checkout(init)
	head := e.writeCommit("f", "head\n", "head side\n")

	err := e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy,
		e.commitObject(c1), e.commitObject(cf),
	)
	require.ErrorAs(t, err, new(*CherryPickConflictError))

	require.NoError(t, e.w.CherryPickSkip(nil))

	// The first pick is kept, the conflicting one is dropped.
	require.Equal(t, "head\n", e.headTreeFile("f"))
	require.Equal(t, "g1\n", e.headTreeFile("g"))
	require.NotEqual(t, head, e.headHash())

	st, err := e.w.CherryPickState()
	require.NoError(t, err)
	require.Nil(t, st)
}

func TestCherryPickSequenceEmptyAndDuplicate(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)

	e.writeCommit("f", "line\n", "init\n")
	dup := e.writeCommit("f", "duplicate\n", "duplicate change\n")

	// Cherry-picking the current HEAD commit is an empty duplicate.
	err := e.w.CherryPick(
		&CommitOptions{Author: defaultSignature()},
		DefaultMergeStrategy, e.commitObject(dup),
	)
	var empty *CherryPickEmptyError
	require.ErrorAs(t, err, &empty)
	require.Equal(t, dup, empty.Commit)
	require.ErrorIs(t, err, ErrCherryPickEmpty)

	st, err := e.w.CherryPickState()
	require.NoError(t, err)
	require.True(t, st.Stopped)
	require.Equal(t, dup, st.Current)

	before := e.headHash()

	// Continue still refuses without AllowEmptyCommits.
	require.ErrorAs(t, e.w.CherryPickContinue(nil), new(*CherryPickEmptyError))
	require.Equal(t, before, e.headHash())

	// Skip finishes the sequence without moving HEAD.
	require.NoError(t, e.w.CherryPickSkip(nil))
	require.Equal(t, before, e.headHash())

	// With AllowEmptyCommits the duplicate creates an empty commit.
	err = e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy, e.commitObject(dup),
	)
	require.NoError(t, err)

	after := e.headHash()
	require.NotEqual(t, before, after)
	emptyCommit := e.commitObject(after)
	require.Equal(t, "duplicate change\n", emptyCommit.Message)
	parent, err := emptyCommit.Parent(0)
	require.NoError(t, err)
	require.Equal(t, before, parent.Hash)
	require.Equal(t, e.commitObject(before).TreeHash, emptyCommit.TreeHash)
}

func TestCherryPickSequenceRepeatedCommit(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)
	init := e.writeCommit("f", "line\n", "init\n")
	e.checkout(init)
	change := e.writeCommit("f", "change\n", "the change\n")
	picked := e.commitObject(change)

	// Back on the base commit, pick the same change twice: the first pick
	// applies, the second is a duplicate and pauses the sequence as empty.
	e.checkout(init)
	err := e.w.CherryPick(
		&CommitOptions{Author: defaultSignature()},
		DefaultMergeStrategy, picked, picked,
	)
	require.ErrorAs(t, err, new(*CherryPickEmptyError))

	st, err := e.w.CherryPickState()
	require.NoError(t, err)
	require.True(t, st.Stopped)
	require.Equal(t, 1, st.CurrentIndex)
	require.Equal(t, 2, st.Total)
	require.Equal(t, change, st.Current)

	// Skip the duplicate; the sequence completes with one pick applied.
	require.NoError(t, e.w.CherryPickSkip(nil))
	st, err = e.w.CherryPickState()
	require.NoError(t, err)
	require.Nil(t, st)
	require.Equal(t, "change\n", e.headTreeFile("f"))
}

func TestCherryPickSequenceModifyDeleteConflict(t *testing.T) {
	t.Parallel()
	// They delete a file we modified: stages 1 and 2, file is kept on disk.
	e := newCPEnv(t)
	init := e.writeCommit("f", "line\n", "init\n")

	e.checkout(init)
	_, err := e.w.Remove("f")
	require.NoError(t, err)
	deleteCommit := e.commit("delete f\n")

	e.checkout(init)
	e.writeCommit("f", "head\n", "head modifies f\n")

	err = e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy, e.commitObject(deleteCommit),
	)
	var conflict *CherryPickConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"f"}, conflict.Conflicts)

	stages := indexStages(t, e.r, "f")
	require.Len(t, stages, 2)
	require.Contains(t, stages, index.AncestorMode)
	require.Contains(t, stages, index.OurMode)
	require.NotContains(t, stages, index.TheirMode)

	// The worktree keeps our modified content.
	require.Equal(t, "head\n", e.readFile("f"))

	// Resolve by keeping our version, then continue.
	e.add("f")
	require.NoError(t, e.w.CherryPickContinue(nil))
	require.Equal(t, "head\n", e.headTreeFile("f"))
}

func TestCherryPickSequenceDeleteModifyConflict(t *testing.T) {
	t.Parallel()
	// We delete a file they modified: stages 1 and 3; resolve by accepting
	// the deletion.
	e := newCPEnv(t)
	init := e.writeCommit("f", "line\n", "init\n")

	e.checkout(init)
	theirMod := e.writeCommit("f", "theirs\n", "they modify f\n")

	e.checkout(init)
	_, err := e.w.Remove("f")
	require.NoError(t, err)
	e.commit("we delete f\n")

	err = e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy, e.commitObject(theirMod),
	)
	var conflict *CherryPickConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"f"}, conflict.Conflicts)

	stages := indexStages(t, e.r, "f")
	require.Len(t, stages, 2)
	require.Contains(t, stages, index.AncestorMode)
	require.Contains(t, stages, index.TheirMode)
	require.NotContains(t, stages, index.OurMode)

	// Resolve as a deletion: removing the worktree file and the stages.
	_, err = e.w.Remove("f")
	require.NoError(t, err)
	require.NoError(t, e.w.CherryPickContinue(nil))
	e.headTreeFileMissing("f")
}

func TestCherryPickSequenceAddAddBinaryConflict(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)

	init := e.writeCommit("seed", "seed\n", "init\n")
	e.checkout(init)
	cherryB := e.writeCommit("b", "bin\x00cherry", "cherry binary\n")

	e.checkout(init)
	e.writeCommit("b", "bin\x00head", "head binary\n")

	err := e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy, e.commitObject(cherryB),
	)
	var conflict *CherryPickConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, []string{"b"}, conflict.Conflicts)

	// Add/add only carries stages 2 and 3.
	stages := indexStages(t, e.r, "b")
	require.Len(t, stages, 2)
	require.Contains(t, stages, index.OurMode)
	require.Contains(t, stages, index.TheirMode)
	require.NotContains(t, stages, index.AncestorMode)

	// Resolve with new binary content.
	e.write("b", "bin\x00solved")
	e.add("b")
	require.NoError(t, e.w.CherryPickContinue(nil))
	require.Equal(t, "bin\x00solved", e.headTreeFile("b"))
}

func TestCherryPickSequenceMergeMainline(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)

	init := e.writeCommit("f", "line\n", "init\n")

	e.checkout(init)
	p1 := e.writeCommit("m1", "m1\n", "parent one\n")

	e.checkout(init)
	p2 := e.writeCommit("m2", "m2\n", "parent two\n")

	// Merge tree: parent one plus the merge result file "merged".
	e.checkout(p1)
	mergeTip := e.writeCommit("merged", "merged\n", "merge result tip\n")
	mergeTree := e.commitObject(mergeTip).TreeHash
	mergeCommit := e.rawMergeCommit(mergeTree, []plumbing.Hash{p1, p2}, "the merge\n")

	// Head side: modify f after init.
	e.checkout(init)
	e.writeCommit("f", "head\n", "head side\n")

	// Default mainline is parent 1: only "merged" is picked; f stays head
	// and neither side-branch file (m1, m2) is part of the pick delta.
	require.NoError(t, e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy, e.commitObject(mergeCommit),
	))
	require.Equal(t, "head\n", e.headTreeFile("f"))
	require.Equal(t, "merged\n", e.headTreeFile("merged"))
	e.headTreeFileMissing("m1")
	e.headTreeFileMissing("m2")

	// An out-of-range mainline is rejected.
	e.checkout(init)
	err := e.w.CherryPickWithOptions(&CherryPickOptions{
		CommitOptions:  &CommitOptions{Author: defaultSignature()},
		Strategy:       DefaultMergeStrategy,
		MainlineParent: 3,
	}, e.commitObject(mergeCommit))
	require.ErrorIs(t, err, ErrCherryPickMainline)
}

type cpFakeSigner struct{}

func (cpFakeSigner) Sign(_ context.Context, r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return append([]byte("fakesig:"), data[:8]...), nil
}

func TestCherryPickSequenceSigner(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)
	init := e.writeCommit("f", "line\n", "init\n")
	e.checkout(init)
	clean := e.writeCommit("g", "g\n", "clean signed pick\n")
	e.checkout(init)
	conf := e.writeCommit("f", "cherry\n", "conflict signed pick\n")

	e.checkout(init)
	e.writeCommit("f", "head\n", "head side\n")

	signer := cpFakeSigner{}
	err := e.w.CherryPick(
		&CommitOptions{
			Author:            defaultSignature(),
			Committer:         defaultSignature(),
			Signer:            signer,
			AllowEmptyCommits: true,
		},
		DefaultMergeStrategy,
		e.commitObject(clean), e.commitObject(conf),
	)
	require.ErrorAs(t, err, new(*CherryPickConflictError))

	// The first, clean pick is signed.
	first := e.commitObject(e.headHash())
	require.Contains(t, first.Signature, "fakesig:")

	// Resolve and continue, re-supplying the signer after a reopen.
	e.write("f", "resolved\n")
	e.add("f")
	r2, err := Open(e.r.Storer, e.fs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r2.Close() })
	w2, err := r2.Worktree()
	require.NoError(t, err)
	require.NoError(t, w2.CherryPickContinue(&CommitOptions{
		Author:    defaultSignature(),
		Committer: defaultSignature(),
		Signer:    signer,
	}))

	head := e.commitObject(e.headHash())
	require.Contains(t, head.Signature, "fakesig:")
	require.Equal(t, "conflict signed pick\n", head.Message)
}

func TestCherryPickSequenceReopenFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, err := PlainInit(dir, false)
	require.NoError(t, err)

	w, err := r.Worktree()
	require.NoError(t, err)
	fs := w.Filesystem()

	require.NoError(t, util.WriteFile(fs, "f", []byte("line\n"), 0o644))
	_, err = w.Add("f")
	require.NoError(t, err)
	init, err := w.Commit("init\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)

	// Cherry chain.
	require.NoError(t, w.Checkout(&CheckoutOptions{Hash: init}))
	require.NoError(t, util.WriteFile(fs, "g", []byte("g1\n"), 0o644))
	_, err = w.Add("g")
	require.NoError(t, err)
	c1, err := w.Commit("pick one\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)
	require.NoError(t, util.WriteFile(fs, "f", []byte("cherry\n"), 0o644))
	_, err = w.Add("f")
	require.NoError(t, err)
	cf, err := w.Commit("pick conflict\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)

	// Head side.
	require.NoError(t, w.Checkout(&CheckoutOptions{Hash: init}))
	require.NoError(t, util.WriteFile(fs, "f", []byte("head\n"), 0o644))
	_, err = w.Add("f")
	require.NoError(t, err)
	head, err := w.Commit("head side\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)

	cc1, err := r.CommitObject(c1)
	require.NoError(t, err)
	ccf, err := r.CommitObject(cf)
	require.NoError(t, err)

	err = w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy, cc1, ccf,
	)
	require.ErrorAs(t, err, new(*CherryPickConflictError))
	require.NoError(t, r.Close())

	// Reopen: the stopped sequence is visible.
	r2, err := PlainOpen(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r2.Close() })
	w2, err := r2.Worktree()
	require.NoError(t, err)

	st, err := w2.CherryPickState()
	require.NoError(t, err)
	require.True(t, st.Stopped)
	require.Equal(t, head, st.OriginalHead)
	require.Equal(t, cf, st.Current)

	// The sequencer state file exists on disk.
	dotGit := w2.r.Storer.(*filesystem.Storage).Filesystem()
	_, err = util.ReadFile(dotGit, "sequencer/cherry-pick")
	require.NoError(t, err)

	// Resolve in the reopened worktree and continue.
	fs2 := w2.Filesystem()
	require.NoError(t, util.WriteFile(fs2, "f", []byte("resolved\n"), 0o644))
	_, err = w2.Add("f")
	require.NoError(t, err)
	require.NoError(t, w2.CherryPickContinue(nil))

	st, err = w2.CherryPickState()
	require.NoError(t, err)
	require.Nil(t, st)

	final, err := r2.Head()
	require.NoError(t, err)
	require.NotEqual(t, head, final.Hash())
	commit, err := r2.CommitObject(final.Hash())
	require.NoError(t, err)
	require.Equal(t, "pick conflict\n", commit.Message)

	// The sequencer state was cleaned up, both through the storer API and
	// as a file inside the git directory.
	seqStore := w2.r.Storer.(storer.SequenceStateStorer)
	_, err = seqStore.SequenceState("cherry-pick")
	require.ErrorIs(t, err, storer.ErrSequenceStateNotFound)
	_, err = util.ReadFile(dotGit, "sequencer/cherry-pick")
	require.Error(t, err)
}

func TestCherryPickSequenceAbortAfterReopenFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, err := PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	fs := w.Filesystem()

	require.NoError(t, util.WriteFile(fs, "f", []byte("line\n"), 0o644))
	_, err = w.Add("f")
	require.NoError(t, err)
	init, err := w.Commit("init\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)

	require.NoError(t, util.WriteFile(fs, "f", []byte("cherry\n"), 0o644))
	_, err = w.Add("f")
	require.NoError(t, err)
	cf, err := w.Commit("pick conflict\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)

	require.NoError(t, w.Checkout(&CheckoutOptions{Hash: init}))
	require.NoError(t, util.WriteFile(fs, "f", []byte("head\n"), 0o644))
	_, err = w.Add("f")
	require.NoError(t, err)
	head, err := w.Commit("head side\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)

	ccf, err := r.CommitObject(cf)
	require.NoError(t, err)
	err = w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy, ccf,
	)
	require.ErrorAs(t, err, new(*CherryPickConflictError))
	require.NoError(t, r.Close())

	// Abort from a freshly opened repository.
	r2, err := PlainOpen(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r2.Close() })
	w2, err := r2.Worktree()
	require.NoError(t, err)
	require.NoError(t, w2.CherryPickAbort())

	require.Equal(t, head, mustHead(t, r2))
	b, err := util.ReadFile(w2.Filesystem(), "f")
	require.NoError(t, err)
	require.Equal(t, "head\n", string(b))

	st, err := w2.CherryPickState()
	require.NoError(t, err)
	require.Nil(t, st)
}

func mustHead(t *testing.T, r *Repository) plumbing.Hash {
	t.Helper()
	ref, err := r.Head()
	require.NoError(t, err)
	return ref.Hash()
}

func TestCherryPickSequenceCrashMidApply(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)

	init := e.writeCommit("f", "line\n", "init\n")
	c1, cf, _ := buildConflictChain(e, init, "cherry\n")

	e.checkout(init)
	e.writeCommit("f", "head\n", "head side\n")

	err := e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy,
		e.commitObject(c1), e.commitObject(cf),
	)
	require.ErrorAs(t, err, new(*CherryPickConflictError))

	// Simulate a process that died while applying cf: the durable state is
	// marked as running (not stopped) and the worktree is half-written.
	data, err := e.w.loadCherryPickData()
	require.NoError(t, err)
	data.Stopped = false
	require.NoError(t, e.w.saveCherryPickData(data))
	e.write("f", "garbage half-written content\n")

	// Continue rebuilds the clean pre-application state first, then retries
	// the commit and hits the same conflict deterministically.
	err = e.w.CherryPickContinue(nil)
	require.ErrorAs(t, err, new(*CherryPickConflictError))

	stages := indexStages(t, e.r, "f")
	require.Len(t, stages, 3)

	e.write("f", "resolved\n")
	e.add("f")
	require.NoError(t, e.w.CherryPickContinue(nil))
	require.Equal(t, "resolved\n", e.headTreeFile("f"))
	require.Equal(t, "g1\n", e.headTreeFile("g"))
}

func TestCherryPickSequenceLifecycleErrors(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)

	require.ErrorIs(t, e.w.CherryPickContinue(nil), ErrCherryPickNoSequence)
	require.ErrorIs(t, e.w.CherryPickAbort(), ErrCherryPickNoSequence)
	require.ErrorIs(t, e.w.CherryPickSkip(nil), ErrCherryPickNoSequence)

	st, err := e.w.CherryPickState()
	require.NoError(t, err)
	require.Nil(t, st)

	// Missing commit options.
	init := e.writeCommit("f", "line\n", "init\n")
	require.ErrorIs(t, e.w.CherryPick(nil, DefaultMergeStrategy, e.commitObject(init)),
		ErrCannotCherryPickWithoutCommitOptions)
}

func TestCherryPickSequenceStorageBackends(t *testing.T) {
	t.Parallel()
	// In-memory backend.
	mem := memory.NewStorage()
	var _ storer.SequenceStateStorer = mem

	require.NoError(t, mem.DeleteSequenceState("x"))
	_, err := mem.SequenceState("x")
	require.ErrorIs(t, err, storer.ErrSequenceStateNotFound)
	require.NoError(t, mem.SetSequenceState("x", []byte("data")))
	got, err := mem.SequenceState("x")
	require.NoError(t, err)
	require.Equal(t, "data", string(got))
	require.NoError(t, mem.DeleteSequenceState("x"))
	_, err = mem.SequenceState("x")
	require.ErrorIs(t, err, storer.ErrSequenceStateNotFound)

	// Filesystem backend.
	fsStore := filesystem.NewStorage(osfs.New(t.TempDir()), nil)
	var _ storer.SequenceStateStorer = fsStore
	require.NoError(t, fsStore.SetSequenceState("cherry-pick", []byte("{}")))
	got2, err := fsStore.SequenceState("cherry-pick")
	require.NoError(t, err)
	require.Equal(t, "{}", string(got2))
	require.NoError(t, fsStore.DeleteSequenceState("cherry-pick"))
	_, err = fsStore.SequenceState("cherry-pick")
	require.ErrorIs(t, err, storer.ErrSequenceStateNotFound)
}

func TestCherryPickSequenceStageHashes(t *testing.T) {
	t.Parallel()
	e := newCPEnv(t)

	init := e.writeCommit("f", "line\n", "init\n")
	e.checkout(init)
	cherry := e.writeCommit("f", "cherry\n", "cherry change\n")
	e.checkout(init)
	e.writeCommit("f", "head\n", "head change\n")

	err := e.w.CherryPick(
		&CommitOptions{Author: defaultSignature(), AllowEmptyCommits: true},
		DefaultMergeStrategy, e.commitObject(cherry),
	)
	require.ErrorAs(t, err, new(*CherryPickConflictError))

	idx, err := e.r.Storer.Index()
	require.NoError(t, err)
	byStage := map[index.Stage]*index.Entry{}
	for _, entry := range idx.Entries {
		if entry.Name == "f" {
			byStage[entry.Stage] = entry
		}
	}
	require.Len(t, byStage, 3)

	blobContent := func(h plumbing.Hash) string {
		blob, err := e.r.BlobObject(h)
		require.NoError(t, err)
		rc, err := blob.Reader()
		require.NoError(t, err)
		defer func() { _ = rc.Close() }()
		c, err := io.ReadAll(rc)
		require.NoError(t, err)
		return string(c)
	}

	require.Equal(t, "line\n", blobContent(byStage[index.AncestorMode].Hash))
	require.Equal(t, "head\n", blobContent(byStage[index.OurMode].Hash))
	require.Equal(t, "cherry\n", blobContent(byStage[index.TheirMode].Hash))
}
