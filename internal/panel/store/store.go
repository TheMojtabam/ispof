// Package store provides a small file-based JSON store with a key-value API.
//
// Each "bucket" is a single JSON file under {dataDir}/store/{bucket}.json.
// All operations take an exclusive RWMutex per bucket. This is suitable for
// panel-scale data (hundreds of users, dozens of inbounds). For massive
// scale, swap this for SQLite or BoltDB and keep the same Put/Get/List/Delete
// API surface.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Buckets
const (
	BucketUsers     = "users"
	BucketInbounds  = "inbounds"
	BucketRouting   = "routing"
	BucketForwards  = "forwards"
	BucketBackups   = "backups"
	BucketAudit     = "audit"
	BucketSettings  = "settings"
	BucketAPITokens = "api_tokens"
)

var allBuckets = []string{
	BucketUsers, BucketInbounds, BucketRouting, BucketForwards,
	BucketBackups, BucketAudit, BucketSettings, BucketAPITokens,
}

// ErrNotFound is returned by Get/Delete when the key doesn't exist.
var ErrNotFound = errors.New("store: not found")

// Store is the panel's persistent KV store, file-backed JSON per bucket.
type Store struct {
	root    string
	mu      sync.RWMutex
	buckets map[string]map[string]json.RawMessage
}

// Open initializes (or loads) the store under dataDir.
func Open(dataDir string) (*Store, error) {
	root := filepath.Join(dataDir, "store")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	s := &Store{root: root, buckets: map[string]map[string]json.RawMessage{}}
	for _, b := range allBuckets {
		if err := s.loadBucket(b); err != nil {
			return nil, fmt.Errorf("load bucket %s: %w", b, err)
		}
	}
	return s, nil
}

// Close is a no-op (kept for API parity with database-backed stores).
func (s *Store) Close() error { return nil }

func (s *Store) loadBucket(name string) error {
	path := filepath.Join(s.root, name+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.buckets[name] = map[string]json.RawMessage{}
			return nil
		}
		return err
	}
	m := map[string]json.RawMessage{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
	}
	s.buckets[name] = m
	return nil
}

// flushBucket writes a bucket atomically (write to .tmp then rename).
func (s *Store) flushBucket(name string) error {
	path := filepath.Join(s.root, name+".json")
	b, err := json.MarshalIndent(s.buckets[name], "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) bucket(name string) (map[string]json.RawMessage, error) {
	b, ok := s.buckets[name]
	if !ok {
		return nil, fmt.Errorf("bucket %q not registered", name)
	}
	return b, nil
}

// Put stores a JSON-marshalable value under key in the given bucket.
func (s *Store) Put(bucket, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket(bucket)
	if err != nil {
		return err
	}
	b[key] = raw
	return s.flushBucket(bucket)
}

// Get fetches a value by key into out (must be pointer).
func (s *Store) Get(bucket, key string, out any) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := s.bucket(bucket)
	if err != nil {
		return err
	}
	raw, ok := b[key]
	if !ok {
		return ErrNotFound
	}
	return json.Unmarshal(raw, out)
}

// Delete removes a key.
func (s *Store) Delete(bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket(bucket)
	if err != nil {
		return err
	}
	if _, ok := b[key]; !ok {
		return nil
	}
	delete(b, key)
	return s.flushBucket(bucket)
}

// List iterates a bucket in deterministic key order.
func (s *Store) List(bucket string, fn func(key string, raw []byte) error) error {
	s.mu.RLock()
	b, err := s.bucket(bucket)
	if err != nil {
		s.mu.RUnlock()
		return err
	}
	keys := make([]string, 0, len(b))
	for k := range b {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// snapshot raw bytes so handlers can run without holding the lock
	snap := make([]json.RawMessage, len(keys))
	for i, k := range keys {
		snap[i] = b[k]
	}
	s.mu.RUnlock()
	for i, k := range keys {
		if err := fn(k, snap[i]); err != nil {
			return err
		}
	}
	return nil
}
