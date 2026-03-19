// Package store provides a simple LevelDB-backed key-value store for chain state.
package store

import (
	"fmt"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// Store wraps a LevelDB instance.
type Store struct {
	db *leveldb.DB
}

// Open opens (or creates) a LevelDB database at the given path.
func Open(path string) (*Store, error) {
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		return nil, fmt.Errorf("open store at %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// OpenMemory opens an in-memory LevelDB (useful in tests).
func OpenMemory() (*Store, error) {
	// LevelDB doesn't have a pure memory mode in this library; use a temp dir.
	// For tests, use os.MkdirTemp and defer os.RemoveAll.
	return nil, fmt.Errorf("use Open with a temp dir for in-memory stores")
}

// Get retrieves a value by key. Returns (nil, nil) if not found.
func (s *Store) Get(key []byte) ([]byte, error) {
	val, err := s.db.Get(key, nil)
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	return val, err
}

// Set stores a key-value pair.
func (s *Store) Set(key, value []byte) error {
	return s.db.Put(key, value, nil)
}

// Delete removes a key.
func (s *Store) Delete(key []byte) error {
	return s.db.Delete(key, nil)
}

// Batch returns a write batch for atomic multi-key updates.
func (s *Store) Batch() *Batch {
	return &Batch{b: new(leveldb.Batch), db: s.db}
}

// Scan iterates over keys with the given prefix, calling fn for each.
// Iteration stops if fn returns false.
func (s *Store) Scan(prefix []byte, fn func(key, val []byte) bool) error {
	iter := s.db.NewIterator(util.BytesPrefix(prefix), nil)
	defer iter.Release()
	for iter.Next() {
		if !fn(iter.Key(), iter.Value()) {
			break
		}
	}
	return iter.Error()
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Batch is an atomic write batch.
type Batch struct {
	b  *leveldb.Batch
	db *leveldb.DB
}

func (b *Batch) Set(key, value []byte) {
	b.b.Put(key, value)
}

func (b *Batch) Delete(key []byte) {
	b.b.Delete(key)
}

func (b *Batch) Flush() error {
	return b.db.Write(b.b, nil)
}
