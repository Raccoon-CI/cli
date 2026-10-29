package envsnap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is a snapshot filed under a key.
type Entry struct {
	Key      string    `json:"key"`
	Snapshot string    `json:"snapshot"` // the provider's id
	Base     string    `json:"base"`
	Created  time.Time `json:"created"`
	Used     time.Time `json:"used"`
}

// Index remembers which snapshot holds which environment. It is a file, so it
// outlives the process. Snapshots take space and every dependency change makes
// a new one, so it holds at most Max and hands back the least recently used
// when it is over, for the caller to delete at the provider.
type Index struct {
	path string
	max  int
	now  func() time.Time

	mu      sync.Mutex
	entries map[string]*Entry
}

// Open reads the index at path (it need not exist yet). max <= 0 means no limit.
func Open(path string, max int) (*Index, error) {
	x := &Index{path: path, max: max, now: time.Now, entries: map[string]*Entry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return x, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Entry
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("snapshot index %s: %w", path, err)
	}
	for _, e := range list {
		if e == nil || e.Key == "" || e.Snapshot == "" {
			return nil, fmt.Errorf("snapshot index %s: bad entry", path)
		}
		x.entries[e.Key] = e
	}
	return x, nil
}

// Lookup returns the snapshot for key and counts it as used.
func (x *Index) Lookup(key string) (Entry, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e := x.entries[key]
	if e == nil {
		return Entry{}, false
	}
	e.Used = x.now()
	return *e, true // saved with the next Put or Remove
}

// Put files snapshot under key and returns the entries that no longer fit.
func (x *Index) Put(key, snapshot, base string) (evicted []Entry, err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	now := x.now()
	if old := x.entries[key]; old != nil && old.Snapshot != snapshot {
		evicted = append(evicted, *old) // replaced: the old one is garbage
	}
	x.entries[key] = &Entry{Key: key, Snapshot: snapshot, Base: base, Created: now, Used: now}
	for x.max > 0 && len(x.entries) > x.max {
		var oldest *Entry
		for _, e := range x.entries {
			if e.Key != key && (oldest == nil || e.Used.Before(oldest.Used)) {
				oldest = e
			}
		}
		if oldest == nil {
			break
		}
		delete(x.entries, oldest.Key)
		evicted = append(evicted, *oldest)
	}
	return evicted, x.save()
}

// Remove forgets key, for a snapshot the provider no longer has.
func (x *Index) Remove(key string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.entries, key)
	return x.save()
}

// Len is how many snapshots are filed.
func (x *Index) Len() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.entries)
}

func (x *Index) save() error {
	list := make([]*Entry, 0, len(x.entries))
	for _, e := range x.entries {
		list = append(list, e)
	}
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(x.path), 0o700); err != nil {
		return err
	}
	tmp := x.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, x.path)
}
