package git

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	billyutil "github.com/go-git/go-billy/v6/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/storage/memory"
)

var errInjectedCheckout = errors.New("injected checkout failure")

// commitFiles writes (or deletes, when content is nil) the given paths and
// commits them on top of the current HEAD.
func commitFiles(t *testing.T, w *Worktree, files map[string]*string, msg string) plumbing.Hash {
	t.Helper()

	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)

	for _, name := range paths {
		content := files[name]
		if content == nil {
			require.NoError(t, w.Filesystem().Remove(name))
		} else {
			require.NoError(t, billyutil.WriteFile(w.Filesystem(), name, []byte(*content), 0o644))
		}
		_, err := w.Add(name)
		require.NoError(t, err)
	}

	h, err := w.Commit(msg, &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)
	return h
}

func newMemoryRepo(t *testing.T) (*Repository, *Worktree) {
	t.Helper()
	r, err := Init(memory.NewStorage(), WithWorkTree(memfs.New()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	w, err := r.Worktree()
	require.NoError(t, err)
	return r, w
}

func indexSnapshot(t *testing.T, r *Repository) map[string]index.Entry {
	t.Helper()
	idx, err := r.Storer.Index()
	require.NoError(t, err)
	snap := make(map[string]index.Entry, len(idx.Entries))
	for _, e := range idx.Entries {
		snap[e.Name] = *e
	}
	return snap
}

func requireSameIndex(t *testing.T, want map[string]index.Entry, r *Repository) {
	t.Helper()
	got := indexSnapshot(t, r)
	require.Len(t, got, len(want), "index entry count differs")
	for name, e := range want {
		g, ok := got[name]
		if !assert.Truef(t, ok, "index entry %q missing after rollback", name) {
			continue
		}
		assert.Equalf(t, e.Hash, g.Hash, "index entry %q hash differs", name)
		assert.Equalf(t, e.Mode, g.Mode, "index entry %q mode differs", name)
		assert.Equalf(t, e.SkipWorktree, g.SkipWorktree, "index entry %q skip-worktree differs", name)
	}
}

func worktreeSnapshot(t *testing.T, fs billy.Filesystem) map[string]string {
	t.Helper()
	snap := map[string]string{}
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := fs.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			name := e.Name()
			if dir != "" {
				name = dir + "/" + e.Name()
			}
			if strings.HasPrefix(name, journalPrefix) {
				continue
			}
			if e.IsDir() {
				walk(name)
				continue
			}
			f, err := fs.Open(name)
			require.NoError(t, err)
			b, err := io.ReadAll(f)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			snap[name] = string(b)
		}
	}
	walk("")
	return snap
}

func requireSameWorktree(t *testing.T, want map[string]string, fs billy.Filesystem) {
	t.Helper()
	got := worktreeSnapshot(t, fs)
	assert.Equal(t, want, got, "worktree content differs after rollback")
	entries, err := fs.ReadDir("")
	require.NoError(t, err)
	for _, e := range entries {
		assert.Falsef(t, strings.HasPrefix(e.Name(), journalPrefix),
			"journal backup dir %q left behind", e.Name())
	}
}

func divergentHistory(t *testing.T, w *Worktree) (master, topic plumbing.Hash) {
	t.Helper()
	a, b, c := "alpha\n", "beta\n", "gamma\n"
	master = commitFiles(t, w, map[string]*string{
		"a.txt":     &a,
		"b.txt":     &b,
		"sub/c.txt": &c,
	}, "master commit")

	require.NoError(t, w.Checkout(&CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("topic"),
		Create: true,
	}))

	a2, d := "alpha-two\n", "delta\n"
	topic = commitFiles(t, w, map[string]*string{
		"a.txt": &a2,
		"b.txt": nil,
		"d.txt": &d,
	}, "topic commit")

	require.NoError(t, w.Checkout(&CheckoutOptions{Branch: plumbing.Master}))
	return master, topic
}

// A failure in the middle of a checkout rolls HEAD, index and worktree
// back to their pre-checkout state, and retrying then succeeds.
func TestCheckoutTransactionFailureRollsBackAndRetries(t *testing.T) {
	t.Parallel()

	r, w := newMemoryRepo(t)
	master, topic := divergentHistory(t, w)

	beforeIdx := indexSnapshot(t, r)
	beforeWT := worktreeSnapshot(t, w.Filesystem())

	w.checkoutFault = &checkoutFault{afterOps: 1, err: errInjectedCheckout}
	err := w.Checkout(&CheckoutOptions{Branch: plumbing.NewBranchReferenceName("topic")})
	require.ErrorIs(t, err, errInjectedCheckout)

	// HEAD never moved.
	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, master, head.Hash())
	assert.Equal(t, plumbing.Master.String(), head.Name().String())

	requireSameIndex(t, beforeIdx, r)
	requireSameWorktree(t, beforeWT, w.Filesystem())
	assert.Zero(t, len(transientJournals), "in-memory journal must be gone after rollback")
	// Retry without the fault completes the checkout.
	w.checkoutFault = nil
	require.NoError(t, w.Checkout(&CheckoutOptions{Branch: plumbing.NewBranchReferenceName("topic")}))

	head, err = r.Head()
	require.NoError(t, err)
	assert.Equal(t, topic, head.Hash())

	st, err := w.Status()
	require.NoError(t, err)
	assert.True(t, st.IsClean(), st)

	assertFileContent(t, w.Filesystem(), "a.txt", "alpha-two\n")
	assertFileContent(t, w.Filesystem(), "d.txt", "delta\n")
	_, err = w.Filesystem().Stat("b.txt")
	assert.True(t, os.IsNotExist(err))
}

// Cancellation before the worktree phase completes is handled like a
// failure: everything is rolled back.
func TestCheckoutTransactionCancellationRollsBack(t *testing.T) {
	t.Parallel()

	r, w := newMemoryRepo(t)
	master, _ := divergentHistory(t, w)

	beforeIdx := indexSnapshot(t, r)
	beforeWT := worktreeSnapshot(t, w.Filesystem())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := w.checkoutContext(ctx, &CheckoutOptions{Branch: plumbing.NewBranchReferenceName("topic")})
	require.ErrorIs(t, err, context.Canceled)

	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, master, head.Hash())
	requireSameIndex(t, beforeIdx, r)
	requireSameWorktree(t, beforeWT, w.Filesystem())
}

// A fault at the index commit point leaves files updated but the old
// index and HEAD intact after rollback.
func TestCheckoutTransactionIndexFailureRollsBack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r, err := PlainInit(dir, false)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	w, err := r.Worktree()
	require.NoError(t, err)

	master, _ := divergentHistory(t, w)
	beforeIdx := indexSnapshot(t, r)

	w.checkoutFault = &checkoutFault{atStage: journalStageIndex, err: errInjectedCheckout}
	err = w.Checkout(&CheckoutOptions{Branch: plumbing.NewBranchReferenceName("topic"), Force: true})
	require.ErrorIs(t, err, errInjectedCheckout)

	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, master, head.Hash())
	requireSameIndex(t, beforeIdx, r)

	st, err := w.Status()
	require.NoError(t, err)
	assert.True(t, st.IsClean(), st)
	assertFileContent(t, w.Filesystem(), "a.txt", "alpha\n")
	assertFileContent(t, w.Filesystem(), "b.txt", "beta\n")
	assertNoJournalArtifacts(t, dir)
}

// TestCheckoutTransactionCrashResumeOnDisk simulates a killed process:
// the failing run leaves the durable journal and backups in place, and a
// fresh Repository over the same directory resumes the interrupted
// checkout to completion.
func TestCheckoutTransactionCrashResumeOnDisk(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink-free POSIX scenario")
	}

	dir := t.TempDir()
	r, err := PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)

	_, _ = divergentHistory(t, w)
	master, err := r.Head()
	require.NoError(t, err)
	require.Equal(t, plumbing.Master.String(), master.Name().String())

	// Kill the process after the first worktree operation.
	w.checkoutFault = &checkoutFault{afterOps: 1, err: errInjectedCheckout, crash: true}
	err = w.Checkout(&CheckoutOptions{Branch: plumbing.NewBranchReferenceName("topic"), Force: true})
	require.ErrorIs(t, err, errInjectedCheckout)
	require.NoError(t, r.Close())

	// HEAD and the on-disk index are still the master ones.
	entries := listJournalEntries(t, dir)
	require.Len(t, entries, 1, "expected one journal after crash")
	require.FileExists(t, filepath.Join(dir, ".git", journalRoot, entries[0], metaFileName))
	require.FileExists(t, filepath.Join(dir, ".git", journalRoot, entries[0], planFileName))
	require.FileExists(t, filepath.Join(dir, ".git", journalRoot, entries[0], doneFileName))

	backupDirs := listWorktreeBackups(t, dir)
	require.Len(t, backupDirs, 1, "expected one worktree backup dir after crash")

	// Fresh process: open the same directory and retry the same target.
	r2, err := PlainOpen(dir)
	require.NoError(t, err)
	defer func() { _ = r2.Close() }()
	w2, err := r2.Worktree()
	require.NoError(t, err)

	require.NoError(t, w2.Checkout(&CheckoutOptions{Branch: plumbing.NewBranchReferenceName("topic"), Force: true}))

	head, err := r2.Head()
	require.NoError(t, err)
	assert.Equal(t, plumbing.NewBranchReferenceName("topic"), head.Name())
	assert.Equal(t, "refs/heads/topic", head.Name().String())

	st, err := w2.Status()
	require.NoError(t, err)
	assert.True(t, st.IsClean(), st)
	assertFileContent(t, w2.Filesystem(), "a.txt", "alpha-two\n")
	assertFileContent(t, w2.Filesystem(), "d.txt", "delta\n")
	_, err = w2.Filesystem().Stat("b.txt")
	assert.True(t, os.IsNotExist(err))

	assertNoJournalArtifacts(t, dir)
}

// A journal for a different target is rolled back before the requested
// checkout proceeds.
func TestCheckoutTransactionCrashDifferentTargetAborts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r, err := PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)

	_, _ = divergentHistory(t, w)

	w.checkoutFault = &checkoutFault{afterOps: 1, err: errInjectedCheckout, crash: true}
	require.ErrorIs(t, w.Checkout(&CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("topic"), Force: true,
	}), errInjectedCheckout)
	require.NoError(t, r.Close())

	// New process asks for master instead of the crashed topic checkout.
	r2, err := PlainOpen(dir)
	require.NoError(t, err)
	defer func() { _ = r2.Close() }()
	w2, err := r2.Worktree()
	require.NoError(t, err)
	require.NoError(t, w2.Checkout(&CheckoutOptions{Branch: plumbing.Master, Force: true}))

	head, err := r2.Head()
	require.NoError(t, err)
	assert.Equal(t, plumbing.Master.String(), head.Name().String())

	st, err := w2.Status()
	require.NoError(t, err)
	assert.True(t, st.IsClean(), st)
	assertFileContent(t, w2.Filesystem(), "a.txt", "alpha\n")
	assertFileContent(t, w2.Filesystem(), "b.txt", "beta\n")
	_, err = w2.Filesystem().Stat("d.txt")
	assert.True(t, os.IsNotExist(err))
	assertNoJournalArtifacts(t, dir)
}

// Two killed processes in a row: each resume picks up the remaining work
// without redoing completed operations, and the second resume finishes.
func TestCheckoutTransactionRepeatedCrashResume(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	open := func(t *testing.T) (*Repository, *Worktree) {
		t.Helper()
		r, err := PlainOpen(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })
		w, err := r.Worktree()
		require.NoError(t, err)
		return r, w
	}

	r0, w0 := func() (*Repository, *Worktree) {
		r, err := PlainInit(dir, false)
		require.NoError(t, err)
		w, err := r.Worktree()
		require.NoError(t, err)
		_, _ = divergentHistory(t, w)
		return r, w
	}()

	topic := func() plumbing.ReferenceName { return plumbing.NewBranchReferenceName("topic") }
	force := func() *CheckoutOptions { return &CheckoutOptions{Branch: topic(), Force: true} }

	w0.checkoutFault = &checkoutFault{afterOps: 1, err: errInjectedCheckout, crash: true}
	require.ErrorIs(t, w0.Checkout(force()), errInjectedCheckout)
	require.NoError(t, r0.Close())

	// First resume process crashes after one more operation (the already
	// completed op is skipped, a new one is recorded in done.log).
	r1, w1 := open(t)
	w1.checkoutFault = &checkoutFault{afterOps: 2, err: errInjectedCheckout, crash: true}
	require.ErrorIs(t, w1.Checkout(force()), errInjectedCheckout)
	require.NoError(t, r1.Close())

	// Second resume process completes the checkout cleanly.
	r2, w2 := open(t)
	require.NoError(t, w2.Checkout(force()))

	head, err := r2.Head()
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/topic", head.Name().String())
	st, err := w2.Status()
	require.NoError(t, err)
	assert.True(t, st.IsClean(), st)
	assertFileContent(t, w2.Filesystem(), "a.txt", "alpha-two\n")
	assertFileContent(t, w2.Filesystem(), "d.txt", "delta\n")
	assertNoJournalArtifacts(t, dir)
}

// Symlinks replaced during a failed checkout are restored verbatim, and
// the following retry materialises the replacement symlink.
func TestCheckoutTransactionSymlinkRollback(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}

	dir := t.TempDir()
	r, err := PlainInit(dir, false)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	w, err := r.Worktree()
	require.NoError(t, err)

	require.NoError(t, w.Filesystem().Symlink("target-a", "link"))
	_, err = w.Add("link")
	require.NoError(t, err)
	_, err = w.Commit("link a\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)

	require.NoError(t, w.Checkout(&CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("topic"), Create: true,
	}))
	require.NoError(t, w.Filesystem().Remove("link"))
	require.NoError(t, w.Filesystem().Symlink("target-b", "link"))
	_, err = w.Add("link")
	require.NoError(t, err)
	_, err = w.Commit("link b\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)

	require.NoError(t, w.Checkout(&CheckoutOptions{Branch: plumbing.Master}))

	w.checkoutFault = &checkoutFault{afterOps: 1, err: errInjectedCheckout}
	err = w.Checkout(&CheckoutOptions{Branch: plumbing.NewBranchReferenceName("topic"), Force: true})
	require.ErrorIs(t, err, errInjectedCheckout)

	got, err := w.Filesystem().Readlink("link")
	require.NoError(t, err)
	assert.Equal(t, "target-a", got)

	w.checkoutFault = nil
	require.NoError(t, w.Checkout(&CheckoutOptions{Branch: plumbing.NewBranchReferenceName("topic"), Force: true}))
	got, err = w.Filesystem().Readlink("link")
	require.NoError(t, err)
	assert.Equal(t, "target-b", got)
	assertNoJournalArtifacts(t, dir)
}

// Executable-bit changes are removed-and-rewritten by checkout; rollback
// restores the original mode.
func TestCheckoutTransactionModeRollback(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission bits differ on windows")
	}

	dir := t.TempDir()
	r, err := PlainInit(dir, false)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	w, err := r.Worktree()
	require.NoError(t, err)

	require.NoError(t, billyutil.WriteFile(w.Filesystem(), "run.sh", []byte("echo a\n"), 0o644))
	_, err = w.Add("run.sh")
	require.NoError(t, err)
	_, err = w.Commit("non-exec\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)

	require.NoError(t, w.Checkout(&CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("topic"), Create: true,
	}))
	require.NoError(t, w.Filesystem().Remove("run.sh"))
	require.NoError(t, billyutil.WriteFile(w.Filesystem(), "run.sh", []byte("echo b\n"), 0o755))
	_, err = w.Add("run.sh")
	require.NoError(t, err)
	_, err = w.Commit("exec\n", &CommitOptions{Author: defaultSignature()})
	require.NoError(t, err)
	require.NoError(t, w.Checkout(&CheckoutOptions{Branch: plumbing.Master}))

	w.checkoutFault = &checkoutFault{afterOps: 1, err: errInjectedCheckout}
	err = w.Checkout(&CheckoutOptions{Branch: plumbing.NewBranchReferenceName("topic"), Force: true})
	require.ErrorIs(t, err, errInjectedCheckout)

	fi, err := w.Filesystem().Stat("run.sh")
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())
	assertFileContent(t, w.Filesystem(), "run.sh", "echo a\n")
}

// Untracked and ignored files are neither journaled nor touched: when a
// checkout fails mid-flight they survive the rollback exactly where they
// were, and a completed force checkout leaves them in place too.
func TestCheckoutTransactionKeepsUntrackedAndIgnored(t *testing.T) {
	t.Parallel()

	_, w := newMemoryRepo(t)
	a := "tracked\n"
	_ = commitFiles(t, w, map[string]*string{"a.txt": &a}, "master")
	require.NoError(t, w.Checkout(&CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("topic"), Create: true,
	}))
	a2 := "tracked-two\n"
	_ = commitFiles(t, w, map[string]*string{"a.txt": &a2}, "topic")
	require.NoError(t, w.Checkout(&CheckoutOptions{Branch: plumbing.Master}))

	// An ignored directory and a plain untracked file, both untouched by
	// tracked history.
	require.NoError(t, w.Filesystem().MkdirAll("node_modules", 0o755))
	require.NoError(t, billyutil.WriteFile(w.Filesystem(), "node_modules/lib.js", []byte("keep me"), 0o644))
	require.NoError(t, billyutil.WriteFile(w.Filesystem(), "untracked.txt", []byte("also keep"), 0o644))

	w.checkoutFault = &checkoutFault{afterOps: 1, err: errInjectedCheckout}
	require.ErrorIs(t, w.Checkout(&CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("topic"), Force: true,
	}), errInjectedCheckout)

	assertFileContent(t, w.Filesystem(), "node_modules/lib.js", "keep me")
	assertFileContent(t, w.Filesystem(), "untracked.txt", "also keep")

	// The successful retry preserves them as a force checkout must.
	w.checkoutFault = nil
	require.NoError(t, w.Checkout(&CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("topic"), Force: true,
	}))
	assertFileContent(t, w.Filesystem(), "node_modules/lib.js", "keep me")
	assertFileContent(t, w.Filesystem(), "untracked.txt", "also keep")
	assertFileContent(t, w.Filesystem(), "a.txt", "tracked-two\n")
}

func assertFileContent(t *testing.T, fs billy.Filesystem, name, want string) {
	t.Helper()
	f, err := fs.Open(name)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, want, string(b))
}

func listJournalEntries(t *testing.T, dir string) []string {
	t.Helper()
	p := filepath.Join(dir, ".git", journalRoot)
	entries, err := os.ReadDir(p)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func listWorktreeBackups(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), journalPrefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

func assertNoJournalArtifacts(t *testing.T, dir string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, ".git", journalRoot))
	assert.True(t, os.IsNotExist(err), "gitdir journal directory still exists")
	assert.Empty(t, listWorktreeBackups(t, dir), "worktree backup dirs still exist")
}
