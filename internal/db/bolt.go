package db

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// type boltStore struct {
// 	db *bbolt.DB
// }

// var (
// 	keysBucket  = []byte("keys")
// 	stateBucket = []byte("state")
// 	metaBucket  = []byte("meta")
// )

// func Open(cfg types.PersistenceConfig) (types.Store, error) {
// 	if cfg.Driver != "bolt" {
// 		return nil, errors.New("only bolt driver supported in v1")
// 	}
// 	db, err := bbolt.Open(cfg.DSN, 0600, &bbolt.Options{Timeout: 1 * time.Second})
// 	if err != nil {
// 		return nil, err
// 	}
// 	if err := db.Update(func(tx *bbolt.Tx) error {
// 		if _, e := tx.CreateBucketIfNotExists(keysBucket); e != nil {
// 			return e
// 		}
// 		if _, e := tx.CreateBucketIfNotExists(stateBucket); e != nil {
// 			return e
// 		}
// 		if _, e := tx.CreateBucketIfNotExists(metaBucket); e != nil {
// 			return e
// 		}
// 		return nil
// 	}); err != nil {
// 		_ = db.Close()
// 		return nil, err
// 	}
// 	return &boltStore{db: db}, nil
// }

// func (b *boltStore) Close() error { return b.db.Close() }

// func (b *boltStore) LoadAll(ctx context.Context) ([]*types.KeyState, error) {
// 	var out []*types.KeyState
// 	err := b.db.View(func(tx *bbolt.Tx) error {
// 		bkt := tx.Bucket(stateBucket)
// 		return bkt.ForEach(func(k, v []byte) error {
// 			var ks types.KeyState
// 			if err := json.Unmarshal(v, &ks); err != nil {
// 				return err
// 			}
// 			out = append(out, &ks)
// 			return nil
// 		})
// 	})
// 	return out, err
// }

// func (b *boltStore) Upsert(ctx context.Context, ks *types.KeyState) error {
// 	ks.UpdatedAt = time.Now()
// 	buf, err := json.Marshal(ks)
// 	if err != nil {
// 		return err
// 	}
// 	return b.db.Update(func(tx *bbolt.Tx) error {
// 		bkt := tx.Bucket(stateBucket)
// 		return bkt.Put([]byte(ks.ID), buf)
// 	})
// }

// package db

// import (
// 	"bytes"
// 	"context"
// 	"encoding/binary"
// 	"errors"
// 	"fmt"
// 	"log"
// 	"sync"
// 	"time"

// )

const (
	// Bucket that stores key readiness.
	rlBucket = "key_rl_state"
)

// BoltStore implements Store with a single background writer.
type BoltStore struct {
	db   *bolt.DB
	ch   chan item
	done chan struct{}
	wg   sync.WaitGroup

	flushEvery time.Duration
}

// item is a single upsert.
type item struct {
	id   string
	when time.Time
}

// OpenPath opens/creates the db at path and starts the async writer.
func OpenPath(path string) (*BoltStore, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{
		Timeout: 1 * time.Second, // don't block forever if someone else holds the file
	})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists([]byte(rlBucket))
		return e
	}); err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &BoltStore{
		db:         db,
		ch:         make(chan item, 1024), // bounded: drop old rather than block
		done:       make(chan struct{}),
		flushEvery: 50 * time.Millisecond, // small latency, good batching
	}

	s.wg.Add(1)
	go s.writer()
	return s, nil
}

func (s *BoltStore) Close() error {
	close(s.done)
	s.wg.Wait()
	return s.db.Close()
}

// LoadAllReady reads all saved ready times.
func (s *BoltStore) LoadAllReady(ctx context.Context) (map[string]time.Time, error) {
	out := make(map[string]time.Time)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(rlBucket))
		if b == nil {
			return errors.New("bucket missing")
		}
		return b.ForEach(func(k, v []byte) error {
			if len(v) != 8 {
				return nil
			}
			ns := int64(binary.BigEndian.Uint64(v))
			out[string(k)] = time.Unix(0, ns)
			return nil
		})
	})
	return out, err
}

// UpsertReadyAtAsync never blocks the caller.
// If the queue is full, we overwrite the oldest element with the latest value for this key.
func (s *BoltStore) UpsertReadyAtAsync(keyID string, when time.Time) {
	select {
	case s.ch <- item{id: keyID, when: when}:
	default:
		// queue full: drop one and push the latest
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- item{id: keyID, when: when}:
		default:
			// if still full, log and give up (never block the request)
			log.Printf("[bolt] queue full; dropping update key=%s", keyID)
		}
	}
}

// writer drains the channel, batches + dedupes by key, and writes in one bolt.Update.
func (s *BoltStore) writer() {
	defer s.wg.Done()

	pending := make(map[string]time.Time)
	timer := time.NewTimer(s.flushEvery)
	defer timer.Stop()

	flush := func() {
		if len(pending) == 0 {
			return
		}
		// take a snapshot and clear pending
		snap := make(map[string]time.Time, len(pending))
		for k, v := range pending {
			snap[k] = v
		}
		for k := range pending {
			delete(pending, k)
		}

		err := s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(rlBucket))
			if b == nil {
				return fmt.Errorf("bucket %s missing", rlBucket)
			}
			buf := make([]byte, 8)
			for id, when := range snap {
				binary.BigEndian.PutUint64(buf, uint64(when.UnixNano()))
				if err := b.Put([]byte(id), bytes.Clone(buf)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("[bolt] batch write error: %v", err)
		}
	}

	for {
		select {
		case <-s.done:
			flush()
			return
		case it := <-s.ch:
			// keep the latest (max) time for each key
			if cur, ok := pending[it.id]; !ok || it.when.After(cur) {
				pending[it.id] = it.when
			}
		case <-timer.C:
			flush()
		}
		timer.Reset(s.flushEvery)
	}
}
