package git

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v6"

	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/utils/merkletrie"
)

// Transactional checkout implementation. See the Checkout doc comment in
// worktree.go for the phase contract.
//
// Durable state layout (only present while a checkout is in flight):
//
//   - <gitdir>/go-git-checkout/<id>/meta.json  — target, mode, captured
//     old HEAD, reached stage and worktree backup location.
//   - <gitdir>/go-git-checkout/<id>/plan.json  — the ordered, deterministic
//     list of worktree operations.
//   - <gitdir>/go-git-checkout/<id>/done.log   — seq numbers of completed
//     worktree operations, appended after each one.
//   - <gitdir>/go-git-checkout/<id>/index.old  — raw bytes of the pre-
//     checkout index (when one existed).
//   - <worktree>/.gogit-checkout-<id>/wt/<seq>/<path> — files and symlinks
//     moved aside before being replaced or deleted, kept in the worktree
//     filesystem so every move is a same-filesystem rename.
//
// Storers without a git directory filesystem (the in-memory storage) get
// the same coordination in memory, which covers failures and retries
// inside the process; with no persistent media, cross-process recovery is
// not a property those storers can provide.
const (
	journalRoot   = "go-git-checkout"
	metaFileName  = "meta.json"
	planFileName  = "plan.json"
	doneFileName  = "done.log"
	indexOldName  = "index.old"
	journalPrefix = ".gogit-checkout-"
	journalWT     = "wt"
	indexFileName = "index"

	journalStageWorktree = "worktree"
	journalStageIndex    = "index"
	journalStageHead     = "head"

	journalVersion = 1

	opTreeDelete   = "tree-delete"
	opChange       = "change"
	opSkipWorktree = "skip-worktree"
)

// journalMeta is the durable description of a running checkout.
type journalMeta struct {
	Version         int       `json:"version"`
	ID              string    `json:"id"`
	WorktreeRoot    string    `json:"worktree_root"`
	Target          string    `json:"target"`
	Mode            int       `json:"mode"`
	SparseDirs      []string  `json:"sparse_dirs,omitempty"`
	Create          bool      `json:"create,omitempty"`
	WantHash        string    `json:"want_hash,omitempty"`
	Branch          string    `json:"branch,omitempty"`
	OldHead         string    `json:"old_head"`
	OldIndexExisted bool      `json:"old_index_existed,omitempty"`
	CreatedBranch   string    `json:"created_branch,omitempty"`
	BackupDir       string    `json:"backup_dir"`
	Stage           string    `json:"stage"`
	StartedAt       time.Time `json:"started_at"`
}

// journalPlanOp is one deterministic worktree step.
type journalPlanOp struct {
	Seq    int    `json:"seq"`
	Kind   string `json:"kind"`
	Action string `json:"action,omitempty"`
	Path   string `json:"path"`
}

// checkoutJournal owns the recovery state of one checkout transaction.
type checkoutJournal struct {
	w          *Worktree
	id         string
	persistent bool
	gitdir     billy.Filesystem // nil for in-memory storers
	dir        string           // <journalRoot>/<id> on the gitdir fs
	backupRoot string           // worktree-relative temp directory
	underlying billy.Filesystem // worktree fs, unwrapped from validators

	meta journalMeta
	ops  []journalPlanOp
	done map[int]struct{}

	currentSeq int
	oldIndex   *index.Index // transient storers only
}

// transientJournals keeps live journals for storers without a git
// directory, keyed by storer identity. It enables retry/resume within the
// process, which is the strongest guarantee an in-memory worktree has.
var (
	transientJournalsMu sync.Mutex
	transientJournals   = map[string]*checkoutJournal{}
)

func transientJournalKey(s any) string {
	return fmt.Sprintf("%T:%p", s, s)
}

// gitdirFilesystem is implemented by storers that expose the directory
// holding the index and references (storage/filesystem.Storage).
type gitdirFilesystem interface {
	Filesystem() billy.Filesystem
}

// checkoutTx is a single checkout run.
type checkoutTx struct {
	w    *Worktree
	ctx  context.Context
	opts *CheckoutOptions
	cfg  *config.Config

	hash       plumbing.Hash
	mode       ResetMode
	sparseDirs []string

	fromTree *object.Tree
	toTree   *object.Tree

	oldIdx        *index.Index
	oldHead       *plumbing.Reference
	oldBranchHash plumbing.Hash

	idxNew       *index.Index
	changedNames []string
	j            *checkoutJournal

	resumed bool
}

// checkoutFault injects failures into a Checkout run. It is unexported and
// only set by the transaction tests in this package; nil means no fault.
type checkoutFault struct {
	// afterOps fails the run once this many worktree operations completed.
	afterOps int
	// atStage fails immediately before committing the named stage
	// ("index" or "head").
	atStage string
	err     error
	// crash leaves journal, backups and staged index on disk instead of
	// rolling back, modelling a killed process for recovery tests.
	crash bool
}

func (w *Worktree) fault() *checkoutFault { return w.checkoutFault }

func (tx *checkoutTx) faultSimulatesCrash() bool {
	f := tx.w.fault()
	return f != nil && f.crash
}

// newCheckoutTx validates everything and creates the journal, without
// applying any change to the worktree, index or HEAD.
func (w *Worktree) newCheckoutTx(ctx context.Context, opts *CheckoutOptions, c plumbing.Hash, mode ResetMode) (*checkoutTx, error) {
	cfg, err := w.r.Config()
	if err != nil {
		return nil, err
	}

	tx := &checkoutTx{
		w:          w,
		ctx:        ctx,
		opts:       opts,
		cfg:        cfg,
		hash:       c,
		mode:       mode,
		sparseDirs: opts.SparseCheckoutDirectories,
	}

	if err := tx.prepare(); err != nil {
		return nil, err
	}

	return tx, nil
}

func (tx *checkoutTx) prepare() error {
	w := tx.w

	if err := tx.captureOldHEAD(); err != nil {
		return err
	}

	// SoftReset (Checkout Keep) moves HEAD only: it never needs the target
	// tree, the index or a worktree plan, matching Reset's SoftReset path.
	if tx.mode == SoftReset {
		j, err := w.newCheckoutJournal(tx.metaForJournal(), nil, nil)
		if err != nil {
			return err
		}
		tx.j = j
		return nil
	}

	toTree, err := w.r.getTreeFromCommitHash(tx.hash)
	if err != nil {
		return err
	}
	tx.toTree = toTree

	if tx.mode == HardReset || tx.mode == KeepReset {
		prev, err := w.headTree()
		if err != nil {
			return err
		}
		tx.fromTree = prev
	}

	if len(tx.sparseDirs) > 0 {
		if !treeContainsDirs(toTree, tx.sparseDirs) {
			return ErrSparseResetDirectoryNotFound
		}
	}

	oldIdx, err := w.r.Storer.Index()
	if err != nil {
		return err
	}
	tx.oldIdx = oldIdx

	// Validate the current modifications against the target tree before
	// planning anything materialised.
	switch tx.mode {
	case MergeReset:
		unstaged, err := w.containsUnstagedChanges(tx.cfg)
		if err != nil {
			return err
		}
		if unstaged {
			return ErrUnstagedChanges
		}
	case KeepReset:
		if err := w.checkKeepResetConflicts(tx.fromTree, toTree, tx.sparseDirs, nil); err != nil {
			return err
		}
	}

	tx.idxNew, tx.changedNames, err = buildResetIndex(oldIdx, toTree, tx.sparseDirs, nil)
	if err != nil {
		return err
	}

	ops, err := tx.planOps()
	if err != nil {
		return err
	}

	j, err := w.newCheckoutJournal(tx.metaForJournal(), ops, oldIdx)
	if err != nil {
		return err
	}
	tx.j = j
	return nil
}

func (tx *checkoutTx) captureOldHEAD() error {
	head, err := tx.w.r.Reference(plumbing.HEAD, false)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil
		}
		return err
	}

	tx.oldHead = head
	if head.Type() == plumbing.SymbolicReference {
		branch, err := tx.w.r.Reference(head.Target(), false)
		if err == nil {
			tx.oldBranchHash = branch.Hash()
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
	}
	return nil
}

// planOps computes the ordered, fully deterministic list of worktree
// operations, mirroring resetWorktree (MergeReset) and
// resetWorktreeToTree (Hard/Keep) exactly.
func (tx *checkoutTx) planOps() ([]journalPlanOp, error) {
	var ops []journalPlanOp
	seq := 0
	add := func(kind, action, name string) journalPlanOp {
		seq++
		return journalPlanOp{Seq: seq, Kind: kind, Action: action, Path: name}
	}

	switch tx.mode {
	case MergeReset:
		// resetWorktree applies every index-vs-worktree change at a path
		// resetIndex touched; changedNames reports exactly those paths.
		changed := buildFilePathMap(tx.changedNames)

		changes, err := tx.w.diffStagingWithWorktreeIndex(tx.cfg, tx.idxNew, true, false)
		if err != nil {
			return nil, err
		}
		for _, ch := range changes {
			a, err := ch.Action()
			if err != nil {
				return nil, err
			}
			name := ch.From.String()
			if a != merkletrie.Delete {
				name = ch.To.String()
			}
			if _, ok := changed[name]; !ok {
				continue
			}
			ops = append(ops, add(opChange, a.String(), name))
		}

	case HardReset, KeepReset:
		// Step 1: delete files the target tree no longer tracks.
		treeChanges, err := diffTrees(tx.fromTree, tx.toTree)
		if err != nil {
			return nil, err
		}
		for _, ch := range treeChanges {
			a, err := ch.Action()
			if err != nil {
				return nil, err
			}
			if a == merkletrie.Delete {
				ops = append(ops, add(opTreeDelete, "", ch.From.String()))
			}
		}

		// Step 2: materialise inserts and modifies; deletes here are
		// untracked paths and are preserved.
		changes, err := tx.w.diffStagingWithWorktreeIndex(tx.cfg, tx.idxNew, true, true)
		if err != nil {
			return nil, err
		}
		for _, ch := range changes {
			a, err := ch.Action()
			if err != nil {
				return nil, err
			}
			if a == merkletrie.Delete {
				continue
			}
			ops = append(ops, add(opChange, a.String(), ch.To.String()))
		}

		// Step 3: tracked entries excluded by the sparse contract must
		// not remain on disk.
		for _, e := range tx.idxNew.Entries {
			if e.SkipWorktree {
				ops = append(ops, add(opSkipWorktree, "", e.Name))
			}
		}
	}

	return ops, nil
}

func (tx *checkoutTx) metaForJournal() journalMeta {
	return journalMeta{
		Version:      journalVersion,
		WorktreeRoot: tx.w.filesystem.Root(),
		Target:       tx.hash.String(),
		Mode:         int(tx.mode),
		SparseDirs:   tx.sparseDirs,
		Create:       tx.opts.Create,
		WantHash:     tx.opts.Hash.String(),
		Branch:       tx.opts.Branch.String(),
		OldHead:      encodeOldHead(tx.oldHead, tx.oldBranchHash),
		Stage:        journalStageWorktree,
		StartedAt:    time.Now().UTC(),
	}
}

// run executes the phases forward.
func (tx *checkoutTx) run() error {
	if tx.resumed {
		switch tx.j.meta.Stage {
		case journalStageHead:
			return tx.j.complete()
		case journalStageIndex:
			// Worktree and index already durable; verify and move HEAD.
			if err := tx.verifyWorktree(); err != nil {
				return err
			}
			return tx.commitHEADAndComplete()
		}
	}

	if err := tx.applyWorktree(); err != nil {
		return err
	}

	if err := tx.verifyWorktree(); err != nil {
		return err
	}

	if err := tx.commitIndex(); err != nil {
		return err
	}

	return tx.commitHEADAndComplete()
}

func (tx *checkoutTx) applyWorktree() error {
	if tx.mode == SoftReset {
		return nil
	}

	base, closeFS := tx.w.reusableRootFS()
	defer closeFS()

	jfs := newJournalFilesystem(base.Filesystem, tx.j)
	fs := newWorktreeFilesystem(jfs, base.protectNTFS, base.protectHFS)
	b := newIndexBuilder(tx.idxNew)

	for _, op := range tx.j.ops {
		if tx.j.isDone(op.Seq) {
			continue
		}
		if err := tx.ctx.Err(); err != nil {
			return err
		}

		tx.j.setSeq(op.Seq)
		if err := tx.executeOp(fs, op, b); err != nil {
			return err
		}
		if err := tx.j.markDone(op.Seq); err != nil {
			return err
		}

		if f := tx.w.fault(); f != nil && f.afterOps > 0 && tx.j.doneCount() == f.afterOps {
			return f.err
		}
	}

	b.Write(tx.idxNew)
	// The worktree files were just written, so the prospective index is
	// stamped "now": the status machinery trusts entry metadata only when
	// the index is newer than the file (racy-git rule), and it must not
	// fall back to hashing file content during verification before the
	// index file itself exists on disk.
	tx.idxNew.ModTime = time.Now()
	return nil
}

// executeOp runs one planned step through the existing checkout primitives,
// so CRLF conversion, symlink handling, blocking-symlink removal, sparse
// and submodule semantics are identical to Reset's paths.
func (tx *checkoutTx) executeOp(fs *worktreeFilesystem, op journalPlanOp, b *indexBuilder) error {
	w := tx.w

	switch op.Kind {
	case opTreeDelete:
		return rmFileAndDirsIfEmpty(fs, op.Path)

	case opSkipWorktree:
		if _, err := fs.Lstat(op.Path); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		return rmFileAndDirsIfEmpty(fs, op.Path)

	case opChange:
		action, err := parseAction(op.Action)
		if err != nil {
			return err
		}
		if action == merkletrie.Delete {
			return rmFileAndDirsIfEmpty(fs, op.Path)
		}

		e, err := tx.toTree.FindEntry(op.Path)
		if err != nil {
			return err
		}
		if e.Mode.IsMalformed() { // defensive, FindEntry already gates paths
			return fmt.Errorf("invalid mode for %q: %v", op.Path, e.Mode)
		}
		if e.Mode == filemode.Submodule {
			return w.checkoutChangeSubmodule(fs, op.Path, action, e, b)
		}
		return w.checkoutChangeRegularFile(tx.cfg, fs, op.Path, action, tx.toTree, e, b)
	}

	return fmt.Errorf("unknown checkout journal op %q", op.Kind)
}

func parseAction(s string) (merkletrie.Action, error) {
	switch s {
	case "Insert":
		return merkletrie.Insert, nil
	case "Delete":
		return merkletrie.Delete, nil
	case "Modify":
		return merkletrie.Modify, nil
	default:
		return 0, fmt.Errorf("unknown checkout action %q", s)
	}
}

// verifyWorktree is the gate between the worktree and index phases: the
// prospective index and the materialised worktree must agree on every
// tracked, non-sparse path. Delete actions are disk-only (untracked)
// entries and, as in Reset, never block checkout.
func (tx *checkoutTx) verifyWorktree() error {
	if tx.mode == SoftReset {
		return nil
	}

	excludeIgnored := tx.mode == HardReset || tx.mode == KeepReset
	changes, err := tx.w.diffStagingWithWorktreeIndex(tx.cfg, tx.idxNew, true, excludeIgnored)
	if err != nil {
		return err
	}

	for _, ch := range changes {
		a, err := ch.Action()
		if err != nil {
			return err
		}
		if a == merkletrie.Delete {
			continue
		}
		name := ch.To.String()
		return fmt.Errorf("%w: %s was not materialised", ErrWorktreeNotClean, name)
	}

	if excludeIgnored {
		fs := tx.w.filesystem
		for _, e := range tx.idxNew.Entries {
			if !e.SkipWorktree {
				continue
			}
			if _, err := fs.Lstat(e.Name); err == nil {
				return fmt.Errorf("%w: sparse-excluded path %s still on disk", ErrWorktreeNotClean, e.Name)
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}

	return nil
}

func (tx *checkoutTx) commitIndex() error {
	if tx.mode == SoftReset {
		return nil
	}

	if f := tx.w.fault(); f != nil && f.atStage == journalStageIndex {
		return f.err
	}

	if !tx.resumed || !tx.j.stageReached(journalStageIndex) {
		if err := tx.j.writeIndex(tx.cfg, tx.idxNew); err != nil {
			return err
		}
		if err := tx.j.setStage(journalStageIndex); err != nil {
			return err
		}
	}
	return nil
}

func (tx *checkoutTx) commitHEADAndComplete() error {
	if f := tx.w.fault(); f != nil && f.atStage == journalStageHead {
		return f.err
	}

	if !tx.resumed || !tx.j.stageReached(journalStageHead) {
		if err := tx.applyHEAD(); err != nil {
			return err
		}
		if err := tx.j.setStage(journalStageHead); err != nil {
			return err
		}
	}
	return tx.j.complete()
}

// applyHEAD performs the reference updates in the same order Reset and the
// old Checkout used: create the -b branch, point HEAD at it (or at the
// detached commit), then advance the branch reference itself.
func (tx *checkoutTx) applyHEAD() error {
	w := tx.w

	if tx.opts.Create {
		if _, err := w.r.Storer.Reference(tx.opts.Branch); err != nil {
			if !errors.Is(err, plumbing.ErrReferenceNotFound) {
				return err
			}
			if err := w.createBranchReference(tx.opts, tx.hash); err != nil {
				return err
			}
			tx.j.meta.CreatedBranch = tx.opts.Branch.String()
		}
	}

	var err error
	if !tx.opts.Hash.IsZero() && !tx.opts.Create {
		err = w.setHEADToCommit(tx.opts.Hash)
	} else {
		err = w.setHEADToBranch(tx.opts.Branch, tx.hash)
	}
	if err != nil {
		return err
	}
	return w.setHEADCommit(tx.hash)
}

// rollback restores the pre-checkout worktree, index and HEAD. It is
// state-driven: each step checks what the disk/storage currently holds, so
// it is safe to call after a failure at any point and safe to repeat.
func (tx *checkoutTx) rollback() error {
	var errs []error

	if tx.j != nil {
		if err := tx.j.rollbackWorktree(); err != nil {
			errs = append(errs, err)
		}
		if tx.j.stageReached(journalStageIndex) {
			if err := tx.j.restoreIndex(); err != nil {
				errs = append(errs, err)
			}
		}
		if err := tx.j.restoreHEAD(); err != nil {
			errs = append(errs, err)
		}
		if len(errs) == 0 {
			if err := tx.j.complete(); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Journal lifecycle
// ---------------------------------------------------------------------------

func (w *Worktree) newCheckoutJournal(meta journalMeta, ops []journalPlanOp, oldIdx *index.Index) (*checkoutJournal, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}

	j := &checkoutJournal{
		w:          w,
		id:         id,
		backupRoot: journalPrefix + id,
		underlying: w.filesystem.Filesystem,
		meta:       meta,
		ops:        ops,
		done:       map[int]struct{}{},
		oldIndex:   oldIdx,
	}
	j.meta.ID = id
	j.meta.BackupDir = j.backupRoot

	if gdfs, ok := w.r.Storer.(gitdirFilesystem); ok && gdfs.Filesystem() != nil {
		j.gitdir = gdfs.Filesystem()
		j.persistent = true
		j.dir = path.Join(journalRoot, id)
		if fi, err := j.gitdir.Stat(indexFileName); err == nil && !fi.IsDir() {
			j.meta.OldIndexExisted = true
		}
	}

	if j.persistent {
		if err := j.gitdir.MkdirAll(j.dir, 0o700); err != nil {
			return nil, err
		}
		if err := j.writeJSON(path.Join(j.dir, metaFileName), j.meta); err != nil {
			return nil, err
		}
		if len(ops) > 0 {
			if err := j.writeJSON(path.Join(j.dir, planFileName), j.ops); err != nil {
				return nil, err
			}
		}
	} else {
		transientJournalsMu.Lock()
		transientJournals[transientJournalKey(w.r.Storer)] = j
		transientJournalsMu.Unlock()
	}

	return j, nil
}

func (j *checkoutJournal) stageReached(stage string) bool {
	order := map[string]int{
		journalStageWorktree: 0,
		journalStageIndex:    1,
		journalStageHead:     2,
	}
	return order[j.meta.Stage] >= order[stage]
}

func (j *checkoutJournal) setSeq(seq int) { j.currentSeq = seq }

func (j *checkoutJournal) isDone(seq int) bool {
	_, ok := j.done[seq]
	return ok
}

func (j *checkoutJournal) doneCount() int { return len(j.done) }

func (j *checkoutJournal) markDone(seq int) error {
	j.done[seq] = struct{}{}
	if !j.persistent {
		return nil
	}
	f, err := j.gitdir.OpenFile(path.Join(j.dir, doneFileName),
		os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = fmt.Fprintf(f, "%d\n", seq)
	return err
}

func (j *checkoutJournal) setStage(stage string) error {
	j.meta.Stage = stage
	if !j.persistent {
		return nil
	}
	return j.writeJSON(path.Join(j.dir, metaFileName), j.meta)
}

func (j *checkoutJournal) writeJSON(name string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return writeFileAtomic(j.gitdir, name, buf.Bytes())
}

func writeFileAtomic(fs billy.Filesystem, name string, data []byte) error {
	tmp := name + ".tmp"
	f, err := fs.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return fs.Rename(tmp, name)
}

// complete removes all journal artifacts after a successful checkout or a
// fully successful rollback.
func (j *checkoutJournal) complete() error {
	var errs []error

	if err := removeAll(j.underlying, j.backupRoot); err != nil {
		errs = append(errs, err)
	}

	if j.persistent {
		if err := removeAll(j.gitdir, j.dir); err != nil {
			errs = append(errs, err)
		}
		// Remove the journal root as well when this was the last entry.
		if entries, err := j.gitdir.ReadDir(journalRoot); err == nil && len(entries) == 0 {
			if err := j.gitdir.Remove(journalRoot); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
		// Staging file left behind by a crash during the index rename.
		if err := j.gitdir.Remove(j.tempIndexName()); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	} else {
		transientJournalsMu.Lock()
		delete(transientJournals, transientJournalKey(j.w.r.Storer))
		transientJournalsMu.Unlock()
	}

	return errors.Join(errs...)
}

func (j *checkoutJournal) tempIndexName() string {
	return indexFileName + ".tx." + j.id
}

// ---------------------------------------------------------------------------
// Index commit/restore
// ---------------------------------------------------------------------------

// writeIndex publishes the new index: encoded into a sibling temporary
// file, then renamed over "index" so the on-disk index is always either
// the old or the new complete file. The old bytes are copied to the
// journal first.
func (j *checkoutJournal) writeIndex(cfg *config.Config, idx *index.Index) error {
	if !j.persistent {
		return j.w.r.Storer.SetIndex(idx)
	}

	if j.meta.OldIndexExisted {
		exists, err := billyExists(j.gitdir, path.Join(j.dir, indexOldName))
		if err != nil {
			return err
		}
		if !exists {
			if err := copyBillyFile(j.gitdir, indexFileName, j.gitdir, path.Join(j.dir, indexOldName)); err != nil {
				return err
			}
		}
	}

	var buf bytes.Buffer
	var encOpts []index.Option
	if cfg.Index.SkipHash.IsTrue() {
		encOpts = append(encOpts, index.WithSkipHash())
	}
	objectFormat := cfg.Extensions.ObjectFormat
	if objectFormat == "" {
		objectFormat = formatcfg.SHA1
	}
	hasher := plumbing.NewHasher(objectFormat, plumbing.AnyObject, 0)
	if err := index.NewEncoder(&buf, hasher.Hash, encOpts...).Encode(idx); err != nil {
		return err
	}

	tmp := j.tempIndexName()
	f, err := j.gitdir.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return j.gitdir.Rename(tmp, indexFileName)
}

func (j *checkoutJournal) restoreIndex() error {
	if !j.persistent {
		if j.oldIndex == nil {
			return nil
		}
		return j.w.r.Storer.SetIndex(j.oldIndex)
	}

	if j.meta.OldIndexExisted {
		if err := j.gitdir.Remove(indexFileName); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := j.gitdir.Rename(path.Join(j.dir, indexOldName), indexFileName); err != nil {
			return err
		}
		return nil
	}

	if err := j.gitdir.Remove(indexFileName); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func billyExists(fs billy.Filesystem, name string) (bool, error) {
	if _, err := fs.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func copyBillyFile(src billy.Filesystem, srcName string, dst billy.Filesystem, dstName string) error {
	in, err := src.Open(srcName)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := dst.MkdirAll(path.Dir(dstName), 0o700); err != nil {
		return err
	}
	out, err := dst.Create(dstName)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)
	return err
}

// ---------------------------------------------------------------------------
// HEAD encoding/restore
// ---------------------------------------------------------------------------

func encodeOldHead(head *plumbing.Reference, branchHash plumbing.Hash) string {
	if head == nil {
		return ""
	}
	if head.Type() == plumbing.HashReference {
		return "hash:" + head.Hash().String()
	}
	return "sym:" + head.Target().String() + ":" + branchHash.String()
}

// restoreHEAD puts HEAD (and any advanced branch ref) back where it was.
// Re-applying the old values is idempotent, so partial reference updates
// converge on the captured state.
func (j *checkoutJournal) restoreHEAD() error {
	old := j.meta.OldHead
	switch {
	case old == "":
		if err := j.w.r.Storer.RemoveReference(plumbing.HEAD); err != nil &&
			!errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
	case strings.HasPrefix(old, "hash:"):
		h := plumbing.NewHash(strings.TrimPrefix(old, "hash:"))
		if err := j.w.r.Storer.SetReference(plumbing.NewHashReference(plumbing.HEAD, h)); err != nil {
			return err
		}
	default: // symbolic HEAD captured as "sym:<branch>:<old-branch-hash>"
		name, hex, _ := strings.Cut(strings.TrimPrefix(old, "sym:"), ":")
		target := plumbing.ReferenceName(name)
		if err := j.w.r.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, target)); err != nil {
			return err
		}
		if hex != "" && hex != plumbing.ZeroHash.String() {
			bh := plumbing.NewHash(hex)
			if err := j.w.r.Storer.SetReference(plumbing.NewHashReference(target, bh)); err != nil {
				return err
			}
		}
	}

	if j.meta.CreatedBranch != "" {
		name := plumbing.ReferenceName(j.meta.CreatedBranch)
		if err := j.w.r.Storer.RemoveReference(name); err != nil &&
			!errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Worktree rollback
// ---------------------------------------------------------------------------

// rollbackWorktree reverses every planned operation using the nodes moved
// into the journal. Rollback is state-driven rather than marker-driven: a
// backup is restored whenever it exists, and planned creations are removed
// when present, so an operation interrupted between its mutation and its
// completion marker is still undone correctly.
func (j *checkoutJournal) rollbackWorktree() error {
	var errs []error

	for _, op := range slices.Backward(j.ops) {
		seqDir := path.Join(j.backupRoot, journalWT, strconv.Itoa(op.Seq))

		rels, err := listBackupLeaves(j.underlying, seqDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, rel := range rels {
			if err := restoreBackupNode(j.underlying, rel, path.Join(seqDir, rel)); err != nil {
				errs = append(errs, err)
			}
		}

		// With no backup the op did not replace anything. Only inserts
		// need undoing in that case (a node they created or half-wrote):
		// deletes and modifications either have a backup or never ran, so
		// touching the path there would destroy the original node.
		if len(rels) == 0 && op.Kind == opChange && op.Action == "Insert" {
			undoCreatedNode(j.underlying, op.Path)
		}
	}

	return errors.Join(errs...)
}

// listBackupLeaves returns slash-relative paths of all files and symlinks
// stored under seqDir, deepest first. Directories in the backup area are
// just scaffold created for nested files and are not returned.
func listBackupLeaves(fs billy.Filesystem, seqDir string) ([]string, error) {
	var leaves []string
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		entries, err := fs.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			childRel := e.Name()
			if rel != "" {
				childRel = rel + "/" + e.Name()
			}
			if e.IsDir() {
				if err := walk(path.Join(dir, e.Name()), childRel); err != nil {
					return err
				}
				continue
			}
			leaves = append(leaves, childRel)
		}
		return nil
	}
	if err := walk(seqDir, ""); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(leaves, func(a, b int) bool {
		return strings.Count(leaves[a], "/") > strings.Count(leaves[b], "/") ||
			(strings.Count(leaves[a], "/") == strings.Count(leaves[b], "/") && leaves[a] > leaves[b])
	})
	return leaves, nil
}

// restoreBackupNode replaces the worktree node at rel with the node kept in
// the journal. A file, symlink or an *empty* directory currently in the
// way is the node this checkout created and is removed first.
func restoreBackupNode(fs billy.Filesystem, rel, backup string) error {
	if fi, err := fs.Lstat(rel); err == nil {
		isSymlink := fi.Mode()&os.ModeSymlink != 0
		if fi.IsDir() && !isSymlink {
			entries, rerr := fs.ReadDir(rel)
			if rerr != nil {
				return rerr
			}
			if len(entries) != 0 {
				return fmt.Errorf("cannot restore %q: directory is not empty", rel)
			}
		}
		if err := fs.Remove(rel); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if dir := path.Dir(rel); dir != "." && dir != "" {
		if err := fs.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return fs.Rename(backup, rel)
}

// undoCreatedNode removes a node the checkout created when the plan had no
// previous node to restore (plain insert, submodule directory). Non-empty
// directories (initialised submodules, pre-existing content) are kept.
func undoCreatedNode(fs billy.Filesystem, name string) {
	fi, err := fs.Lstat(name)
	if err != nil {
		return
	}
	if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		// Remove only an empty directory: a non-empty one is either an
		// initialised submodule or pre-existing content and must stay.
		if entries, rerr := fs.ReadDir(name); rerr == nil && len(entries) == 0 {
			_ = fs.Remove(name)
		}
	} else {
		_ = fs.Remove(name)
	}
	pruneEmptyParents(fs, name)
}

func pruneEmptyParents(fs billy.Filesystem, name string) {
	dir := path.Dir(name)
	for dir != "." && dir != "" && dir != "/" {
		entries, err := fs.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := fs.Remove(dir); err != nil {
			return
		}
		dir = path.Dir(dir)
	}
}

func removeAll(fs billy.Filesystem, p string) error {
	entries, err := fs.ReadDir(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		child := path.Join(p, e.Name())
		if e.IsDir() {
			if err := removeAll(fs, child); err != nil {
				return err
			}
			continue
		}
		if err := fs.Remove(child); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := fs.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Journaling filesystem
// ---------------------------------------------------------------------------

// journalFilesystem sits below the validating worktreeFilesystem. It moves
// aside every file or symlink that a checkout is about to replace or
// delete, into the current operation's backup directory, and leaves the
// primitives' error semantics intact otherwise.
type journalFilesystem struct {
	billy.Filesystem
	j *checkoutJournal
}

func newJournalFilesystem(fs billy.Filesystem, j *checkoutJournal) *journalFilesystem {
	return &journalFilesystem{fs, j}
}

func (s *journalFilesystem) Remove(name string) error {
	if fi, err := s.Lstat(name); err == nil {
		// Real directories are never backed up: checkout only removes
		// empty ones, which are disposable. Symlinks are non-dir nodes and
		// are moved aside like regular files.
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			if err := s.j.backupNode(name); err != nil {
				return err
			}
		}
	}
	if err := s.Filesystem.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *journalFilesystem) OpenFile(name string, flag int, perm fs.FileMode) (billy.File, error) {
	if flag&os.O_TRUNC != 0 {
		s.backupIfPresent(name)
	}
	return s.Filesystem.OpenFile(name, flag, perm)
}

func (s *journalFilesystem) Create(name string) (billy.File, error) {
	s.backupIfPresent(name)
	return s.Filesystem.Create(name)
}

func (s *journalFilesystem) Symlink(target, link string) error {
	s.backupIfPresent(link)
	return s.Filesystem.Symlink(target, link)
}

func (s *journalFilesystem) backupIfPresent(name string) {
	fi, err := s.Lstat(name)
	if err != nil {
		return
	}
	if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		return
	}
	_ = s.j.backupNode(name)
}

// backupNode atomically renames the worktree node name into this
// operation's backup directory. It is idempotent across retries: if the
// backup already exists, the node was already moved aside.
func (j *checkoutJournal) backupNode(name string) error {
	under := j.underlying
	dest := path.Join(j.backupRoot, journalWT, strconv.Itoa(j.currentSeq), filepath.ToSlash(name))
	if _, err := under.Lstat(dest); err == nil {
		return nil
	}
	if dir := path.Dir(dest); dir != "." && dir != "" {
		if err := under.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return under.Rename(name, dest)
}

// ---------------------------------------------------------------------------
// Recovery of interrupted checkouts
// ---------------------------------------------------------------------------

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	// Both lists are small option slices compared verbatim, order
	// significant (like SparseCheckoutDirectories).
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m journalMeta) matchesTarget(c plumbing.Hash, mode ResetMode, opts *CheckoutOptions) bool {
	return m.Target == c.String() &&
		m.Mode == int(mode) &&
		sameStringSlice(m.SparseDirs, opts.SparseCheckoutDirectories) &&
		m.Create == opts.Create &&
		m.WantHash == opts.Hash.String() &&
		m.Branch == opts.Branch.String()
}

// recoverInterruptedCheckout finishes or rolls back journals left by a
// previous run before starting a new one. A journal for the requested
// target is resumed; any other journal for this worktree is rolled back.
func (w *Worktree) recoverInterruptedCheckout(ctx context.Context, c plumbing.Hash, mode ResetMode, opts *CheckoutOptions) error {
	j, err := w.findInterruptedJournal()
	if err != nil {
		return err
	}
	if j == nil {
		return nil
	}

	if !j.meta.matchesTarget(c, mode, opts) {
		return j.abort()
	}
	return w.resumeCheckout(ctx, j, c, mode, opts)
}

func (w *Worktree) findInterruptedJournal() (*checkoutJournal, error) {
	root := w.filesystem.Root()

	if gdfs, ok := w.r.Storer.(gitdirFilesystem); ok && gdfs.Filesystem() != nil {
		gd := gdfs.Filesystem()
		entries, err := gd.ReadDir(journalRoot)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		var found *checkoutJournal
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			j, err := loadCheckoutJournal(w, gd, path.Join(journalRoot, e.Name()))
			if err != nil {
				return nil, err
			}
			if j == nil || j.meta.WorktreeRoot != root {
				continue
			}
			if found == nil || j.meta.StartedAt.After(found.meta.StartedAt) {
				found = j
			}
		}
		return found, nil
	}

	transientJournalsMu.Lock()
	j := transientJournals[transientJournalKey(w.r.Storer)]
	transientJournalsMu.Unlock()
	if j != nil && j.meta.WorktreeRoot != root {
		return nil, nil
	}
	return j, nil
}

func loadCheckoutJournal(w *Worktree, gd billy.Filesystem, dir string) (*checkoutJournal, error) {
	j := &checkoutJournal{
		w:          w,
		id:         path.Base(dir),
		persistent: true,
		gitdir:     gd,
		dir:        dir,
		done:       map[int]struct{}{},
		underlying: w.filesystem.Filesystem,
	}

	if err := readJSON(gd, path.Join(dir, metaFileName), &j.meta); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if j.meta.Version != journalVersion {
		return nil, fmt.Errorf("unsupported checkout journal version %d in %s", j.meta.Version, dir)
	}
	j.backupRoot = j.meta.BackupDir
	j.id = j.meta.ID

	if err := readJSON(gd, path.Join(dir, planFileName), &j.ops); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if f, err := gd.Open(path.Join(dir, doneFileName)); err == nil {
		data, rerr := io.ReadAll(f)
		_ = f.Close()
		if rerr != nil {
			return nil, rerr
		}
		for line := range strings.FieldsSeq(string(data)) {
			if seq, perr := strconv.Atoi(line); perr == nil {
				j.done[seq] = struct{}{}
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	return j, nil
}

func readJSON(fs billy.Filesystem, name string, v any) error {
	f, err := fs.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return json.NewDecoder(f).Decode(v)
}

// abort rolls back a journal targeting a different commit (or one left by
// an aborted run) and removes its artifacts.
func (j *checkoutJournal) abort() error {
	var errs []error
	if err := j.rollbackWorktree(); err != nil {
		errs = append(errs, err)
	}
	if j.stageReached(journalStageIndex) {
		if err := j.restoreIndex(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := j.restoreHEAD(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		if err := j.complete(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// resumeCheckout continues an interrupted checkout for the same target.
func (w *Worktree) resumeCheckout(ctx context.Context, j *checkoutJournal, c plumbing.Hash, mode ResetMode, opts *CheckoutOptions) error {
	cfg, err := w.r.Config()
	if err != nil {
		return err
	}
	toTree, err := w.r.getTreeFromCommitHash(c)
	if err != nil {
		return err
	}

	idxNew, err := buildResetIndexFromStorer(w, toTree, opts.SparseCheckoutDirectories)
	if err != nil && mode != SoftReset {
		return err
	}

	tx := &checkoutTx{
		w:          w,
		ctx:        ctx,
		opts:       opts,
		cfg:        cfg,
		hash:       c,
		mode:       mode,
		sparseDirs: opts.SparseCheckoutDirectories,
		toTree:     toTree,
		idxNew:     idxNew,
		j:          j,
		resumed:    true,
	}

	if runErr := tx.run(); runErr != nil {
		if tx.faultSimulatesCrash() {
			return runErr
		}
		if rbErr := tx.rollback(); rbErr != nil {
			return errors.Join(runErr, fmt.Errorf("checkout rollback failed: %w", rbErr))
		}
		return runErr
	}
	return nil
}

func buildResetIndexFromStorer(w *Worktree, t *object.Tree, dirs []string) (*index.Index, error) {
	oldIdx, err := w.r.Storer.Index()
	if err != nil {
		return nil, err
	}
	idx, _, err := buildResetIndex(oldIdx, t, dirs, nil)
	return idx, err
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
