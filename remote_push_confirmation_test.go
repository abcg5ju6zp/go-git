package git

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	fixtures "github.com/go-git/go-git-fixtures/v6"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/plumbing/transport/file"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

var (
	casOld = plumbing.NewHash("1111111111111111111111111111111111111111")
	casNew = plumbing.NewHash("2222222222222222222222222222222222222222")
	casP   = plumbing.NewHash("3333333333333333333333333333333333333333")
)

// pushFixture clones the Basic fixture into a writable bare server
// storage and a second bare clone used as the pushing repository.
func pushFixture(t *testing.T) (url string, serverSto *filesystem.Storage, local *Repository) {
	t.Helper()

	fs, err := fixtures.Basic().One().DotGit(fixtures.WithTargetDir(t.TempDir))
	require.NoError(t, err)

	serverSto = filesystem.NewStorage(fs, cache.NewObjectLRUDefault())
	t.Cleanup(func() { _ = serverSto.Close() })

	local, err = PlainClone(t.TempDir(), &CloneOptions{URL: fs.Root(), Bare: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })

	return fs.Root(), serverSto, local
}

func refHash(t *testing.T, s storage.Storer, name plumbing.ReferenceName) plumbing.Hash {
	t.Helper()
	ref, err := s.Reference(name)
	require.NoError(t, err)
	return ref.Hash()
}

// countingTransport wraps a transport and counts the packfile bytes the
// client sends on pushes.
type countingTransport struct {
	inner      transport.Transport
	packPushes int32
	packBytes  atomic.Int64
}

func (c *countingTransport) Handshake(ctx context.Context, req *transport.Request) (transport.Session, error) {
	sess, err := c.inner.Handshake(ctx, req)
	if err != nil {
		return nil, err
	}
	return &countingSession{Session: sess, owner: c}, nil
}

type countingSession struct {
	transport.Session
	owner *countingTransport
}

func (s *countingSession) Push(ctx context.Context, st storage.Storer, req *transport.PushRequest) error {
	if req.Packfile != nil {
		s.owner.packPushes++
		req.Packfile = &countingReadCloser{ReadCloser: req.Packfile, owner: s.owner}
	}
	return s.Session.Push(ctx, st, req)
}

type countingReadCloser struct {
	io.ReadCloser
	owner *countingTransport
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		c.owner.packBytes.Add(int64(n))
	}
	return n, err
}

// truncatingWriteCloser forwards packets until after "ok refs/..."
// command status lines have been forwarded, then swallows the rest of the
// report (later status lines and the flush), modelling a server that
// disconnects mid-report.
type truncatingWriteCloser struct {
	inner    io.WriteCloser
	after    int
	seenOK   int
	dropping bool
	acc      []byte
}

func (w *truncatingWriteCloser) Write(p []byte) (int, error) {
	w.acc = append(w.acc, p...)
	for len(w.acc) >= 4 {
		n, err := strconv.ParseUint(string(w.acc[:4]), 16, 16)
		if err != nil || n != 0 && n < 4 {
			// Not a pkt-line boundary; stop framing and pass through.
			rest := w.acc
			w.acc = nil
			if !w.dropping {
				if _, werr := w.inner.Write(rest); werr != nil {
					return 0, werr
				}
			}
			break
		}
		// A non-flush packet that has not arrived completely yet: wait
		// for the rest before deciding whether to forward it.
		if n != 0 && uint64(len(w.acc)) < n {
			break
		}

		var packet []byte
		if n == 0 {
			packet = w.acc[:4]
			w.acc = w.acc[4:]
		} else {
			packet = w.acc[:n]
			w.acc = w.acc[n:]
		}

		if !w.dropping {
			if _, err := w.inner.Write(packet); err != nil {
				return 0, err
			}
		}

		if n > 0 && bytes.HasPrefix(packet[4:], []byte("ok refs/")) {
			w.seenOK++
			if w.seenOK >= w.after {
				// Drop every later packet (more status lines, flush) and
				// any half-buffered tail.
				w.dropping = true
				w.acc = nil
				break
			}
		}
	}
	return len(p), nil
}

func (w *truncatingWriteCloser) Close() error { return w.inner.Close() }

func truncatingReceivePack(okLines int) file.ServerFunc {
	return func(ctx context.Context, st storage.Storer, r io.ReadCloser, w io.WriteCloser, gitProtocol string) error {
		return transport.ReceivePack(ctx, st, r, &truncatingWriteCloser{inner: w, after: okLines},
			&transport.ReceivePackRequest{GitProtocol: gitProtocol})
	}
}

// hookMovingReceivePack runs a pre-receive hook that moves a reference
// while reporting success. The following compare-and-set for that
// reference then fails on the server ("ng") while other references in the
// same batch are accepted.
func hookMovingReceivePack(move plumbing.ReferenceName, to plumbing.Hash) file.ServerFunc {
	return func(ctx context.Context, st storage.Storer, r io.ReadCloser, w io.WriteCloser, gitProtocol string) error {
		return transport.ReceivePack(ctx, st, r, w, &transport.ReceivePackRequest{
			GitProtocol: gitProtocol,
			Hooks: transport.ReceivePackHooks{
				PreReceive: func(_ context.Context, info *transport.PreReceiveInfo) error {
					return info.Storer.SetReference(plumbing.NewHashReference(move, to))
				},
			},
		})
	}
}

func refspecsFromStrings(in []string) []config.RefSpec {
	out := make([]config.RefSpec, len(in))
	for i, s := range in {
		out[i] = config.RefSpec(s)
	}
	return out
}

// AC-1/AC-2: a batch with one accepted and one rejected reference updates
// only the accepted tracking reference, and the result exposes both
// outcomes while keeping the command error type.
func TestPushPartialAcceptRejectTracking(t *testing.T) {
	t.Parallel()
	_, serverSto, local := pushFixture(t)

	master := refHash(t, local.Storer, plumbing.ReferenceName("refs/heads/master"))
	oldBranch := refHash(t, serverSto, plumbing.ReferenceName("refs/heads/branch"))

	tr := &countingTransport{inner: file.NewTransport(file.Options{
		ReceivePack: hookMovingReceivePack(plumbing.ReferenceName("refs/heads/branch"), master),
	})}

	remote, err := local.Remote(DefaultRemoteName)
	require.NoError(t, err)

	result, err := remote.PushResult(&PushOptions{
		RefSpecs: refspecsFromStrings([]string{
			"refs/heads/master:refs/heads/newbranch",
			"+refs/heads/master:refs/heads/branch",
		}),
		ClientOptions: []client.Option{client.WithTransport("file", tr)},
	})
	require.Error(t, err)

	var cmdErr packp.CommandStatusErr
	require.ErrorAs(t, err, &cmdErr)
	require.Equal(t, plumbing.ReferenceName("refs/heads/branch"), cmdErr.ReferenceName)
	require.NotEmpty(t, cmdErr.Status)

	newBranch, ok := result.Status(plumbing.ReferenceName("refs/heads/newbranch"))
	require.True(t, ok)
	require.Equal(t, PushRefAccepted, newBranch.State)

	rejected, ok := result.Status(plumbing.ReferenceName("refs/heads/branch"))
	require.True(t, ok)
	require.Equal(t, PushRefRejected, rejected.State)
	require.Equal(t, cmdErr.Status, rejected.Reason)

	// The server created newbranch and refused (kept at the hook value)
	// branch.
	require.Equal(t, master, refHash(t, serverSto, plumbing.ReferenceName("refs/heads/newbranch")))
	require.Equal(t, master, refHash(t, serverSto, plumbing.ReferenceName("refs/heads/branch")))

	// Local tracking follows only the accepted reference; the rejected
	// one keeps its pre-push value.
	require.Equal(t, master, refHash(t, local.Storer, plumbing.ReferenceName("refs/remotes/origin/newbranch")))
	require.Equal(t, oldBranch, refHash(t, local.Storer, plumbing.ReferenceName("refs/remotes/origin/branch")))
}

// AC-3/AC-7: a report truncated after the first "ok" leaves one ref
// accepted, one pending, a durable journal with the pending ref, and the
// retry sends no pack.
func TestPushTruncatedReportThenRetrySendsNoPack(t *testing.T) {
	t.Parallel()
	url, serverSto, local := pushFixture(t)
	master := refHash(t, local.Storer, plumbing.ReferenceName("refs/heads/master"))

	specs := []string{
		"refs/heads/master:refs/heads/recover-a",
		"refs/heads/master:refs/heads/recover-b",
	}

	flaky := &countingTransport{inner: file.NewTransport(file.Options{
		ReceivePack: truncatingReceivePack(1),
	})}

	remote, err := local.Remote(DefaultRemoteName)
	require.NoError(t, err)

	result, err := remote.PushResult(&PushOptions{
		RefSpecs:      refspecsFromStrings(specs),
		ClientOptions: []client.Option{client.WithTransport("file", flaky)},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode report-status")

	accepted := result.Status
	gotA, ok := accepted(plumbing.ReferenceName("refs/heads/recover-a"))
	require.True(t, ok)
	require.Equal(t, PushRefAccepted, gotA.State)
	gotB, ok := accepted(plumbing.ReferenceName("refs/heads/recover-b"))
	require.True(t, ok)
	require.Equal(t, PushRefPending, gotB.State)

	require.Equal(t, int32(1), flaky.packPushes)

	// Accepted tracking ref applied; pending one untouched.
	require.Equal(t, master, refHash(t, local.Storer, plumbing.ReferenceName("refs/remotes/origin/recover-a")))
	_, err = local.Storer.Reference(plumbing.ReferenceName("refs/remotes/origin/recover-b"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)

	// The journal holds the still-pending reference.
	pss := local.Storer.(storer.PushStateStorer)
	txn, err := pss.LoadPushTransaction(DefaultRemoteName, url)
	require.NoError(t, err)
	require.NotNil(t, txn)
	require.Len(t, txn.Updates, 1)
	require.Equal(t, plumbing.ReferenceName("refs/heads/recover-b"), txn.Updates[0].Name)
	require.Equal(t, storer.RefUpdatePending, txn.Updates[0].State)
	require.Equal(t, master, txn.Updates[0].New)

	// The server applied both refs before answering, even though the
	// report only delivered the first "ok" line.
	require.Equal(t, master, refHash(t, serverSto, plumbing.ReferenceName("refs/heads/recover-a")))
	require.Equal(t, master, refHash(t, serverSto, plumbing.ReferenceName("refs/heads/recover-b")))

	// Same-process retry: the advertisement confirms both refs, nothing
	// is left to send, so no session Push (and no packfile) happens.
	retry := &countingTransport{inner: file.NewTransport(file.Options{})}
	result2, err2 := remote.PushResult(&PushOptions{
		RefSpecs:      refspecsFromStrings(specs),
		ClientOptions: []client.Option{client.WithTransport("file", retry)},
	})
	require.ErrorIs(t, err2, NoErrAlreadyUpToDate)
	require.Empty(t, result2.Refs)
	require.Equal(t, int32(0), retry.packPushes)
	require.Equal(t, int64(0), retry.packBytes.Load())

	require.Equal(t, master, refHash(t, local.Storer, plumbing.ReferenceName("refs/remotes/origin/recover-a")))
	require.Equal(t, master, refHash(t, local.Storer, plumbing.ReferenceName("refs/remotes/origin/recover-b")))

	txn, err = pss.LoadPushTransaction(DefaultRemoteName, url)
	require.NoError(t, err)
	require.Nil(t, txn)
}

// AC-5: after a restart, an accepted journal entry is replayed and a
// pending entry is confirmed from the fresh advertisement; replaying the
// whole recovery again is a no-op.
func TestPushJournalRecoveryAfterRestart(t *testing.T) {
	t.Parallel()
	url, _, pusher := pushFixture(t)
	master := refHash(t, pusher.Storer, plumbing.ReferenceName("refs/heads/master"))

	// The witness clone is created BEFORE the push, so it never observes
	// recover-a/recover-b: it models a client that crashed after the
	// server applied the refs but before it applied the report.
	witnessDir := t.TempDir()
	witness, err := PlainClone(witnessDir, &CloneOptions{URL: url, Bare: true})
	require.NoError(t, err)

	// First push against the truncating server so the server actually
	// has recover-a and recover-b.
	flaky := &countingTransport{inner: file.NewTransport(file.Options{
		ReceivePack: truncatingReceivePack(1),
	})}
	remote, err := pusher.Remote(DefaultRemoteName)
	require.NoError(t, err)
	_, err = remote.PushResult(&PushOptions{
		RefSpecs: refspecsFromStrings([]string{
			"refs/heads/master:refs/heads/recover-a",
			"refs/heads/master:refs/heads/recover-b",
		}),
		ClientOptions: []client.Option{client.WithTransport("file", flaky)},
	})
	require.Error(t, err)
	require.NoError(t, pusher.Close())

	// Write the journal by hand with one accepted and one pending
	// update; both tracking refs are absent in the witness clone.
	witnessPSS := witness.Storer.(storer.PushStateStorer)
	require.NoError(t, witnessPSS.SavePushTransaction(&storer.PushTransaction{
		RemoteName: DefaultRemoteName,
		RemoteURL:  url,
		Updates: []storer.PushRefUpdate{
			{
				Name:  plumbing.ReferenceName("refs/heads/recover-a"),
				New:   master,
				State: storer.RefUpdateAccepted,
			},
			{
				Name:  plumbing.ReferenceName("refs/heads/recover-b"),
				New:   master,
				State: storer.RefUpdatePending,
			},
		},
	}))
	require.NoError(t, witness.Close())

	reopenAndPush := func() {
		reopened, rerr := PlainOpen(witnessDir)
		require.NoError(t, rerr)

		retry := &countingTransport{inner: file.NewTransport(file.Options{})}
		rem, rerr := reopened.Remote(DefaultRemoteName)
		require.NoError(t, rerr)
		_, perr := rem.PushResult(&PushOptions{
			RefSpecs: refspecsFromStrings([]string{
				"refs/heads/master:refs/heads/recover-a",
				"refs/heads/master:refs/heads/recover-b",
			}),
			ClientOptions: []client.Option{client.WithTransport("file", retry)},
		})
		require.ErrorIs(t, perr, NoErrAlreadyUpToDate)
		require.Equal(t, int32(0), retry.packPushes)

		require.Equal(t, master, refHash(t, reopened.Storer, plumbing.ReferenceName("refs/remotes/origin/recover-a")))
		require.Equal(t, master, refHash(t, reopened.Storer, plumbing.ReferenceName("refs/remotes/origin/recover-b")))
		require.NoError(t, reopened.Close())
	}

	// First restart: journaled accepted replay + advertisement confirm.
	reopenAndPush()

	// A journal left behind means a replay must be idempotent; the
	// journal was deleted by the first recovered push, so a second
	// restart simply finds everything up to date and changes nothing.
	reopenAndPush()
}

// AC-2/AC-8: a fully accepted batch reports every reference as accepted,
// exposes the result through the Repository entry point, and leaves no
// journal behind.
func TestPushResultAllAcceptedAndNoJournal(t *testing.T) {
	t.Parallel()
	url, serverSto, local := pushFixture(t)
	master := refHash(t, local.Storer, plumbing.ReferenceName("refs/heads/master"))

	result, err := local.PushResult(&PushOptions{
		RefSpecs: refspecsFromStrings([]string{
			"refs/heads/master:refs/heads/zz-success",
		}),
	})
	require.NoError(t, err)
	require.Len(t, result.Refs, 1)
	require.Equal(t, PushRefAccepted, result.Refs[0].State)
	require.Equal(t, master, result.Refs[0].New)
	require.NoError(t, result.Error())
	require.Empty(t, result.Rejected())
	require.Empty(t, result.Pending())

	require.Equal(t, master, refHash(t, serverSto, plumbing.ReferenceName("refs/heads/zz-success")))
	require.Equal(t, master, refHash(t, local.Storer, plumbing.ReferenceName("refs/remotes/origin/zz-success")))

	pss := local.Storer.(storer.PushStateStorer)
	txn, err := pss.LoadPushTransaction(DefaultRemoteName, url)
	require.NoError(t, err)
	require.Nil(t, txn)
}

// AC-6: a journaled accepted create must not overwrite a tracking ref that
// already holds a different value.
func TestPushRecoveryDoesNotClobberAdvancedTracking(t *testing.T) {
	t.Parallel()
	url, serverSto, local := pushFixture(t)

	newer := refHash(t, serverSto, plumbing.ReferenceName("refs/heads/branch"))
	target := plumbing.NewHashReference("refs/remotes/origin/recover-x", newer)
	require.NoError(t, local.Storer.SetReference(target))

	cfg, err := local.Config()
	require.NoError(t, err)
	remote := NewRemote(local.Storer, cfg.Remotes[DefaultRemoteName])

	pss := local.Storer.(storer.PushStateStorer)
	require.NoError(t, pss.SavePushTransaction(&storer.PushTransaction{
		RemoteName: DefaultRemoteName,
		RemoteURL:  url,
		Updates: []storer.PushRefUpdate{
			{
				Name:  plumbing.ReferenceName("refs/heads/recover-x"),
				New:   refHash(t, local.Storer, plumbing.ReferenceName("refs/heads/master")),
				State: storer.RefUpdateAccepted,
			},
		},
	}))

	pending, _, err := remote.recoverPushTransaction(DefaultRemoteName, url)
	require.NoError(t, err)
	require.Empty(t, pending)

	require.Equal(t, newer, refHash(t, local.Storer, plumbing.ReferenceName("refs/remotes/origin/recover-x")))
	txn, err := pss.LoadPushTransaction(DefaultRemoteName, url)
	require.NoError(t, err)
	require.Nil(t, txn)
}

// Recovery compare-and-set branches that a live push never takes: an
// update whose tracking ref already moved must not roll it backwards
// (create/update) or delete it (delete), while matching values are
// applied and replays are idempotent.
func TestRecoveryTrackingCASBranches(t *testing.T) {
	t.Parallel()
	_, _, local := pushFixture(t)

	cfg, err := local.Config()
	require.NoError(t, err)
	remote := NewRemote(local.Storer, cfg.Remotes[DefaultRemoteName])

	tracking := plumbing.ReferenceName("refs/remotes/origin/cas")
	set := func(h plumbing.Hash) {
		require.NoError(t, local.Storer.SetReference(plumbing.NewHashReference(tracking, h)))
	}
	assertValue := func(h plumbing.Hash) {
		require.Equal(t, h, refHash(t, local.Storer, tracking))
	}
	assertMissing := func() {
		_, gerr := local.Storer.Reference(tracking)
		require.ErrorIs(t, gerr, plumbing.ErrReferenceNotFound)
	}

	name := plumbing.ReferenceName("refs/heads/cas")

	// Update, tracking holds an unrelated newer value: leave it.
	set(casP)
	require.NoError(t, remote.applyConfirmedTracking(storer.PushRefUpdate{
		Name: name, Old: casOld, New: casNew, State: storer.RefUpdateAccepted,
	}))
	assertValue(casP)

	// Update, tracking holds exactly Old: advance to New.
	set(casOld)
	require.NoError(t, remote.applyConfirmedTracking(storer.PushRefUpdate{
		Name: name, Old: casOld, New: casNew, State: storer.RefUpdateAccepted,
	}))
	assertValue(casNew)

	// Update, tracking absent: create.
	require.NoError(t, local.Storer.RemoveReference(tracking))
	require.NoError(t, remote.applyConfirmedTracking(storer.PushRefUpdate{
		Name: name, Old: casOld, New: casNew, State: storer.RefUpdateAccepted,
	}))
	assertValue(casNew)

	// Idempotent replay at New is a no-op.
	require.NoError(t, remote.applyConfirmedTracking(storer.PushRefUpdate{
		Name: name, Old: casOld, New: casNew, State: storer.RefUpdateAccepted,
	}))
	assertValue(casNew)

	// Delete, tracking moved: keep it.
	set(casP)
	require.NoError(t, remote.applyConfirmedTracking(storer.PushRefUpdate{
		Name: name, Old: casOld, State: storer.RefUpdateAccepted,
	}))
	assertValue(casP)

	// Delete, tracking holds exactly Old: remove.
	set(casOld)
	require.NoError(t, remote.applyConfirmedTracking(storer.PushRefUpdate{
		Name: name, Old: casOld, State: storer.RefUpdateAccepted,
	}))
	assertMissing()

	// Delete already missing: no-op.
	require.NoError(t, remote.applyConfirmedTracking(storer.PushRefUpdate{
		Name: name, Old: casOld, State: storer.RefUpdateAccepted,
	}))
	assertMissing()
}

// discardWriteCloser accepts the client's request bytes while a scripted
// session answers with an unpack failure without contacting the server.
type discardWriteCloser struct{}

func (discardWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriteCloser) Close() error                { return nil }

type unpackFailingTransport struct{ inner transport.Transport }

func (t *unpackFailingTransport) Handshake(ctx context.Context, req *transport.Request) (transport.Session, error) {
	sess, err := t.inner.Handshake(ctx, req)
	if err != nil {
		return nil, err
	}
	return &unpackFailingSession{Session: sess}, nil
}

type unpackFailingSession struct {
	transport.Session
}

func (s *unpackFailingSession) Push(ctx context.Context, st storage.Storer, req *transport.PushRequest) error {
	report := strings.NewReader("0012unpack failed\n" + "0000")
	return transport.SendPack(ctx, st, *s.Capabilities(), discardWriteCloser{}, io.NopCloser(report), req)
}

// AC-4 at the Remote level: an unpack failure report rejects every
// reference, updates no tracking ref, keeps the UnpackStatusErr type, and
// leaves no journal.
func TestPushUnpackFailureUpdatesNoTracking(t *testing.T) {
	t.Parallel()
	url, serverSto, local := pushFixture(t)

	tr := &unpackFailingTransport{inner: file.NewTransport(file.Options{})}
	remote, err := local.Remote(DefaultRemoteName)
	require.NoError(t, err)

	result, err := remote.PushResult(&PushOptions{
		RefSpecs:      refspecsFromStrings([]string{"refs/heads/master:refs/heads/zz-unpack"}),
		ClientOptions: []client.Option{client.WithTransport("file", tr)},
	})
	require.Error(t, err)
	var unpackErr packp.UnpackStatusErr
	require.ErrorAs(t, err, &unpackErr)
	require.Equal(t, "failed", unpackErr.Status)

	for _, ref := range result.Refs {
		require.Equal(t, PushRefRejected, ref.State)
	}

	_, err = local.Storer.Reference(plumbing.ReferenceName("refs/remotes/origin/zz-unpack"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
	_, err = serverSto.Reference(plumbing.ReferenceName("refs/heads/zz-unpack"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)

	txn, err := local.Storer.(storer.PushStateStorer).LoadPushTransaction(DefaultRemoteName, url)
	require.NoError(t, err)
	require.Nil(t, txn)
}

// plainPushStorer presents the standard storage.Storer surface while
// deliberately hiding PushStateStorer, modelling a third-party storer
// without journal support. Embedding the interface means none of the
// concrete storage's extra methods are promoted.
type plainPushStorer struct {
	storage.Storer
}

// AC-9: a storer without PushStateStorer still performs a fully working
// push, including per-accepted tracking updates.
func TestPushWorksWithoutPushStateStorer(t *testing.T) {
	t.Parallel()
	_, serverSto, local := pushFixture(t)
	master := refHash(t, local.Storer, plumbing.ReferenceName("refs/heads/master"))

	cfg, err := local.Config()
	require.NoError(t, err)
	plain := &plainPushStorer{Storer: local.Storer}
	remote := NewRemote(plain, cfg.Remotes[DefaultRemoteName])
	_, ok := remote.s.(storer.PushStateStorer)
	require.False(t, ok)

	_, err = remote.PushResult(&PushOptions{
		RefSpecs: refspecsFromStrings([]string{"refs/heads/master:refs/heads/zz-plain"}),
	})
	require.NoError(t, err)

	require.Equal(t, master, refHash(t, serverSto, plumbing.ReferenceName("refs/heads/zz-plain")))
	require.Equal(t, master, refHash(t, local.Storer, plumbing.ReferenceName("refs/remotes/origin/zz-plain")))
}
