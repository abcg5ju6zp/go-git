package storer

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestPushRefUpdateJSONRoundTrip(t *testing.T) {
	t.Parallel()

	sha1, ok := plumbing.FromHex("6ecf0ef2c2dffb796033e5a02219af86ec6584e5")
	require.True(t, ok)
	sha256, ok := plumbing.FromHex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.True(t, ok)

	cases := []struct {
		name string
		hash plumbing.Hash
	}{
		{name: "sha1", hash: sha1},
		{name: "sha256", hash: sha256},
		{name: "zero", hash: plumbing.ZeroHash},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			original := PushRefUpdate{
				Name:   plumbing.ReferenceName("refs/heads/main"),
				Old:    plumbing.ZeroHash,
				New:    tc.hash,
				State:  RefUpdateAccepted,
				Reason: "",
			}

			data, err := json.Marshal(original)
			require.NoError(t, err)

			var decoded PushRefUpdate
			require.NoError(t, json.Unmarshal(data, &decoded))
			assert.Equal(t, original, decoded)
			assert.True(t, decoded.New.Equal(tc.hash))
			assert.Equal(t, tc.hash.IsZero(), decoded.IsDelete())
		})
	}
}

func TestPushRefUpdateJSONRejectsBadHash(t *testing.T) {
	t.Parallel()

	var decoded PushRefUpdate
	err := json.Unmarshal([]byte(`{"name":"refs/heads/main","old":"","new":"not-a-hash","state":"pending"}`), &decoded)
	assert.Error(t, err)
}

func TestPushTransactionKeyStableAndDistinct(t *testing.T) {
	t.Parallel()

	a := PushTransactionKey("origin", "file:///repo")
	b := PushTransactionKey("origin", "file:///repo")
	c := PushTransactionKey("upstream", "file:///repo")

	assert.Equal(t, a, b)
	assert.NotEqual(t, a, c)
	assert.Len(t, a, 64) // sha256 hex
}
