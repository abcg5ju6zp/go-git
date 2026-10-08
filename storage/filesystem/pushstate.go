package filesystem

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/go-git/go-billy/v6"

	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/utils/ioutil"
	"github.com/go-git/go-git/v6/utils/trace"
)

// pushStateDir is the dotgit-relative directory holding one durable
// transaction file per remote. It is go-git private state: git ignores
// unknown files inside the dotgit directory, and nothing here is ever
// scanned as a reference.
const pushStateDir = "go-git/push"

// Compile-time assertion that the filesystem Storage implements the
// optional push transaction store.
var _ storer.PushStateStorer = (*Storage)(nil)

// LoadPushTransaction implements storer.PushStateStorer. A missing or
// unreadable (corrupt, wrong version) record is treated as absent so a
// damaged journal can never block pushes.
func (s *Storage) LoadPushTransaction(remoteName, remoteURL string) (*storer.PushTransaction, error) {
	f, err := s.fs.Open(s.pushStatePath(remoteName, remoteURL))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var txn storer.PushTransaction
	if err := json.NewDecoder(f).Decode(&txn); err != nil {
		trace.General.Printf("ignoring corrupt push transaction %s: %s", f.Name(), err)
		return nil, nil
	}
	if txn.Version != storer.PushTransactionVersion {
		trace.General.Printf("ignoring push transaction %s with unsupported version %d", f.Name(), txn.Version)
		return nil, nil
	}
	return &txn, nil
}

// SavePushTransaction implements storer.PushStateStorer. The record is
// written to a temporary file in the same directory and renamed into
// place, so a crash leaves either the previous record or the new one.
func (s *Storage) SavePushTransaction(txn *storer.PushTransaction) error {
	if txn == nil {
		return nil
	}

	record := *txn
	record.Version = storer.PushTransactionVersion

	if err := s.fs.MkdirAll(pushStateDir, 0o755); err != nil {
		return err
	}

	tmp, err := s.fs.TempFile(pushStateDir, "txn-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = s.fs.Remove(tmpName) }

	if err := json.NewEncoder(tmp).Encode(&record); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}

	target := s.pushStatePath(txn.RemoteName, txn.RemoteURL)
	if err := s.fs.Rename(tmpName, target); err == nil {
		return nil
	} else if !errors.Is(err, billy.ErrNotSupported) {
		cleanup()
		return err
	}

	// Filesystems without rename (e.g. sivafs): fall back to a full copy.
	dst, err := s.fs.Create(target)
	if err != nil {
		cleanup()
		return err
	}
	src, err := s.fs.Open(tmpName)
	if err != nil {
		_ = dst.Close()
		cleanup()
		return err
	}
	if _, copyErr := ioutil.CopyBufferPool(dst, src); copyErr != nil {
		_ = src.Close()
		_ = dst.Close()
		cleanup()
		return copyErr
	}
	if err := src.Close(); err != nil {
		_ = dst.Close()
		cleanup()
		return err
	}
	if err := dst.Close(); err != nil {
		cleanup()
		return err
	}
	cleanup()
	return nil
}

// DeletePushTransaction implements storer.PushStateStorer. A missing
// record is not an error.
func (s *Storage) DeletePushTransaction(remoteName, remoteURL string) error {
	err := s.fs.Remove(s.pushStatePath(remoteName, remoteURL))
	if err != nil && (errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist)) {
		return nil
	}
	return err
}

func (s *Storage) pushStatePath(remoteName, remoteURL string) string {
	return fmt.Sprintf("%s/%s.json", pushStateDir, storer.PushTransactionKey(remoteName, remoteURL))
}
