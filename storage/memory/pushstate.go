package memory

import (
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// Compile-time assertion that the memory Storage implements the optional
// push transaction store. It only covers same-process retries: an
// in-memory storage has no medium that survives a restart.
var _ storer.PushStateStorer = (*Storage)(nil)

// LoadPushTransaction implements storer.PushStateStorer.
func (s *Storage) LoadPushTransaction(remoteName, remoteURL string) (*storer.PushTransaction, error) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()

	txn, ok := s.pushTxn[storer.PushTransactionKey(remoteName, remoteURL)]
	if !ok || txn == nil {
		return nil, nil
	}
	return clonePushTransaction(txn), nil
}

// SavePushTransaction implements storer.PushStateStorer.
func (s *Storage) SavePushTransaction(txn *storer.PushTransaction) error {
	if txn == nil {
		return nil
	}

	s.pushMu.Lock()
	defer s.pushMu.Unlock()

	if s.pushTxn == nil {
		// Tolerate a Storage created without NewStorage.
		s.pushTxn = make(map[string]*storer.PushTransaction)
	}
	record := clonePushTransaction(txn)
	record.Version = storer.PushTransactionVersion
	s.pushTxn[storer.PushTransactionKey(txn.RemoteName, txn.RemoteURL)] = record
	return nil
}

// DeletePushTransaction implements storer.PushStateStorer.
func (s *Storage) DeletePushTransaction(remoteName, remoteURL string) error {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()

	delete(s.pushTxn, storer.PushTransactionKey(remoteName, remoteURL))
	return nil
}

// clonePushTransaction returns an independent copy so callers cannot
// mutate stored records (or vice versa) without going through Save.
func clonePushTransaction(txn *storer.PushTransaction) *storer.PushTransaction {
	cp := *txn
	cp.Updates = append([]storer.PushRefUpdate(nil), txn.Updates...)
	return &cp
}
