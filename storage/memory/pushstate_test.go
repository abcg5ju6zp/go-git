package memory

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

func TestMemoryPushTransactionRoundTrip(t *testing.T) {
	t.Parallel()
	s := NewStorage()

	txn, err := s.LoadPushTransaction("origin", "file:///repo")
	require.NoError(t, err)
	assert.Nil(t, txn)

	original := &storer.PushTransaction{
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
	}
	require.NoError(t, s.SavePushTransaction(original))

	loaded, err := s.LoadPushTransaction("origin", "file:///repo")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Len(t, loaded.Updates, 1)
	assert.Equal(t, plumbing.ReferenceName("refs/heads/a"), loaded.Updates[0].Name)

	// Mutating a loaded record does not change the stored one.
	loaded.Updates[0].State = storer.RefUpdateRejected
	again, err := s.LoadPushTransaction("origin", "file:///repo")
	require.NoError(t, err)
	assert.Equal(t, storer.RefUpdateAccepted, again.Updates[0].State)

	require.NoError(t, s.DeletePushTransaction("origin", "file:///repo"))
	loaded, err = s.LoadPushTransaction("origin", "file:///repo")
	require.NoError(t, err)
	assert.Nil(t, loaded)
}

func TestMemoryPushTransactionConcurrentAccess(t *testing.T) {
	t.Parallel()
	s := NewStorage()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			_ = s.SavePushTransaction(&storer.PushTransaction{
				RemoteName: "origin",
				RemoteURL:  "file:///repo",
			})
			_, _ = s.LoadPushTransaction("origin", "file:///repo")
		})
	}
	wg.Wait()

	require.NoError(t, s.DeletePushTransaction("origin", "file:///repo"))
}

func TestMemoryPushTransactionZeroValueStorage(t *testing.T) {
	t.Parallel()
	// A Storage not built with NewStorage must not panic on first save.
	s := &Storage{}
	err := s.SavePushTransaction(&storer.PushTransaction{
		RemoteName: "origin",
		RemoteURL:  "file:///repo",
	})
	require.NoError(t, err)
	loaded, err := s.LoadPushTransaction("origin", "file:///repo")
	require.NoError(t, err)
	assert.NotNil(t, loaded)
}
