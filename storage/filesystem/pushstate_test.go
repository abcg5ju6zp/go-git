package filesystem

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-billy/v6/osfs"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

func newPushStateStorage(t *testing.T) *Storage {
	t.Helper()
	return NewStorage(osfs.New(t.TempDir()), cache.NewObjectLRUDefault())
}

func TestPushTransactionRoundTrip(t *testing.T) {
	t.Parallel()
	s := newPushStateStorage(t)

	txn, err := s.LoadPushTransaction("origin", "file:///repo")
	require.NoError(t, err)
	assert.Nil(t, txn)

	started := time.Now().UTC().Truncate(time.Second)
	original := &storer.PushTransaction{
		RemoteName: "origin",
		RemoteURL:  "file:///repo",
		Started:    started,
		Updates: []storer.PushRefUpdate{
			{
				Name:  plumbing.ReferenceName("refs/heads/a"),
				Old:   plumbing.NewHash("1111111111111111111111111111111111111111"),
				New:   plumbing.NewHash("2222222222222222222222222222222222222222"),
				State: storer.RefUpdateAccepted,
			},
			{
				Name:  plumbing.ReferenceName("refs/heads/b"),
				New:   plumbing.ZeroHash,
				Old:   plumbing.NewHash("3333333333333333333333333333333333333333"),
				State: storer.RefUpdatePending,
			},
		},
	}
	require.NoError(t, s.SavePushTransaction(original))

	loaded, err := s.LoadPushTransaction("origin", "file:///repo")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, storer.PushTransactionVersion, loaded.Version)
	assert.Equal(t, "origin", loaded.RemoteName)
	assert.Equal(t, "file:///repo", loaded.RemoteURL)
	assert.Equal(t, started, loaded.Started)
	require.Len(t, loaded.Updates, 2)
	assert.Equal(t, storer.RefUpdateAccepted, loaded.Updates[0].State)
	assert.True(t, loaded.Updates[1].IsDelete())
	assert.Equal(t, storer.RefUpdatePending, loaded.Updates[1].State)
}

func TestPushTransactionSurvivesStorageReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first := NewStorage(osfs.New(dir), cache.NewObjectLRUDefault())
	require.NoError(t, first.SavePushTransaction(&storer.PushTransaction{
		RemoteName: "origin",
		RemoteURL:  "file:///repo",
		Started:    time.Now(),
		Updates: []storer.PushRefUpdate{
			{
				Name:  plumbing.ReferenceName("refs/heads/a"),
				New:   plumbing.NewHash("2222222222222222222222222222222222222222"),
				State: storer.RefUpdateAccepted,
			},
		},
	}))

	// A second Storage over the same dotgit directory models reopening the
	// repository after a process restart.
	second := NewStorage(osfs.New(dir), cache.NewObjectLRUDefault())
	loaded, err := second.LoadPushTransaction("origin", "file:///repo")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Len(t, loaded.Updates, 1)
	assert.Equal(t, plumbing.ReferenceName("refs/heads/a"), loaded.Updates[0].Name)

	files, err := filepath.Glob(filepath.Join(dir, "go-git", "push", "*.json"))
	require.NoError(t, err)
	require.Len(t, files, 1)
}

func TestPushTransactionDelete(t *testing.T) {
	t.Parallel()
	s := newPushStateStorage(t)

	require.NoError(t, s.SavePushTransaction(&storer.PushTransaction{
		RemoteName: "origin",
		RemoteURL:  "file:///repo",
	}))
	require.NoError(t, s.DeletePushTransaction("origin", "file:///repo"))

	loaded, err := s.LoadPushTransaction("origin", "file:///repo")
	require.NoError(t, err)
	assert.Nil(t, loaded)

	// Deleting a missing record is not an error.
	assert.NoError(t, s.DeletePushTransaction("origin", "file:///repo"))
}

func TestPushTransactionCorruptRecordIgnored(t *testing.T) {
	t.Parallel()
	s := newPushStateStorage(t)

	require.NoError(t, s.SavePushTransaction(&storer.PushTransaction{
		RemoteName: "origin",
		RemoteURL:  "file:///repo",
	}))

	path := s.pushStatePath("origin", "file:///repo")
	f, err := s.fs.Create(path)
	require.NoError(t, err)
	_, err = f.Write([]byte("{not json"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	loaded, err := s.LoadPushTransaction("origin", "file:///repo")
	require.NoError(t, err)
	assert.Nil(t, loaded)
}

func TestPushTransactionKeysAreDistinct(t *testing.T) {
	t.Parallel()
	s := newPushStateStorage(t)

	require.NoError(t, s.SavePushTransaction(&storer.PushTransaction{
		RemoteName: "origin",
		RemoteURL:  "file:///a",
	}))
	require.NoError(t, s.SavePushTransaction(&storer.PushTransaction{
		RemoteName: "origin",
		RemoteURL:  "file:///b",
	}))

	a, err := s.LoadPushTransaction("origin", "file:///a")
	require.NoError(t, err)
	assert.NotNil(t, a)
	b, err := s.LoadPushTransaction("origin", "file:///b")
	require.NoError(t, err)
	assert.NotNil(t, b)
}
