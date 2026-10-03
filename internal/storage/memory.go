package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// NewMemory creates an isolated, concurrency-safe in-memory Store for demos and
// tests. Its ETags and condition handling are local to this process.
func NewMemory() Store {
	return &memoryStore{
		objects: make(map[string]memoryObject),
		dirs:    map[string]struct{}{"": {}},
	}
}

type memoryObject struct {
	data    []byte
	etag    string
	modTime time.Time
}

type memoryStore struct {
	mu      sync.RWMutex
	objects map[string]memoryObject
	dirs    map[string]struct{}
	version uint64
}

func (m *memoryStore) List(ctx context.Context, key string) ([]Entry, error) {
	key, err := normalizeKey(key, true)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.isDirLocked(key) {
		return nil, ErrNotFound
	}
	children := make(map[string]Entry)
	base := key
	if base != "" {
		base += "/"
	}
	add := func(full string, obj memoryObject, isDir bool) {
		rel := strings.TrimPrefix(full, base)
		if rel == full && base != "" || rel == "" {
			return
		}
		first, _, hasMore := strings.Cut(rel, "/")
		if first == "" {
			return
		}
		childKey := joinKey(key, first)
		if hasMore || isDir {
			children[childKey] = Entry{Path: childKey, Name: first, IsDir: true, Modified: obj.modTime}
			return
		}
		children[childKey] = Entry{Path: childKey, Name: first, Size: int64(len(obj.data)), Modified: obj.modTime, ETag: obj.etag}
	}
	for name := range m.dirs {
		if name != "" && name != key && strings.HasPrefix(name, base) {
			add(name, memoryObject{modTime: time.Now()}, true)
		}
	}
	for name, obj := range m.objects {
		if strings.HasPrefix(name, base) && name != key {
			add(name, obj, false)
		}
	}
	result := make([]Entry, 0, len(children))
	for _, e := range children {
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	if len(result) > maxListEntries {
		return nil, fmt.Errorf("listing exceeds %d entries", maxListEntries)
	}
	return result, nil
}

func (m *memoryStore) Stat(ctx context.Context, key string) (Entry, error) {
	key, err := normalizeKey(key, true)
	if err != nil {
		return Entry{}, err
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if obj, ok := m.objects[key]; ok {
		return Entry{Path: key, Name: path.Base(key), Size: int64(len(obj.data)), Modified: obj.modTime, ETag: obj.etag}, nil
	}
	if m.isDirLocked(key) {
		return Entry{Path: key, Name: path.Base(key), IsDir: true}, nil
	}
	return Entry{}, ErrNotFound
}

func (m *memoryStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, Entry, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return nil, Entry{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, Entry{}, err
	}
	m.mu.RLock()
	obj, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, Entry{}, ErrNotFound
	}
	if etag != "" && etag != obj.etag {
		return nil, Entry{}, ErrConflict
	}
	e := Entry{Path: key, Name: path.Base(key), Size: int64(len(obj.data)), Modified: obj.modTime, ETag: obj.etag}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), obj.data...))), e, nil
}

func (m *memoryStore) OpenRange(ctx context.Context, key, etag string, start, length int64) (io.ReadCloser, Entry, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return nil, Entry{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, Entry{}, err
	}
	if etag == "" || start < 0 || length <= 0 || start > int64(^uint64(0)>>1)-length {
		return nil, Entry{}, ErrInvalidRange
	}
	m.mu.RLock()
	obj, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, Entry{}, ErrNotFound
	}
	if obj.etag != etag {
		return nil, Entry{}, ErrConflict
	}
	end := start + length
	if end > int64(len(obj.data)) {
		return nil, Entry{}, ErrInvalidRange
	}
	e := Entry{Path: key, Name: path.Base(key), Size: length, Modified: obj.modTime, ETag: obj.etag}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), obj.data[start:end]...))), e, nil
}

func (m *memoryStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition Condition) (Entry, error) {
	key, err := normalizeKey(key, false)
	if err != nil {
		return Entry{}, err
	}
	if err := ensureNoConditionConflict(condition); err != nil {
		return Entry{}, err
	}
	if size < 0 || body == nil {
		return Entry{}, fmt.Errorf("invalid object body")
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	data, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil {
		return Entry{}, err
	}
	if int64(len(data)) != size {
		return Entry{}, io.ErrUnexpectedEOF
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, exists := m.objects[key]
	if condition.IfNoneMatch && exists || condition.IfMatch != "" && (!exists || condition.IfMatch != old.etag) {
		return Entry{}, ErrConflict
	}
	if _, isDir := m.dirs[key]; isDir {
		return Entry{}, ErrConflict
	}
	m.version++
	obj := memoryObject{data: data, etag: fmt.Sprintf("\"mem-%016x\"", m.version), modTime: time.Now().UTC()}
	m.objects[key] = obj
	e := Entry{Path: key, Name: path.Base(key), Size: int64(len(data)), Modified: obj.modTime, ETag: obj.etag}
	return e, nil
}

func (m *memoryStore) Delete(ctx context.Context, key string, condition Condition) error {
	key, err := normalizeKey(key, false)
	if err != nil {
		return err
	}
	if err := ensureNoConditionConflict(condition); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if obj, ok := m.objects[key]; ok {
		if condition.IfNoneMatch || condition.IfMatch != "" && condition.IfMatch != obj.etag {
			return ErrConflict
		}
		delete(m.objects, key)
		return nil
	}
	if _, ok := m.dirs[key]; ok {
		if condition.IfNoneMatch || condition.IfMatch != "" {
			return ErrConflict
		}
		prefix := key + "/"
		for name := range m.objects {
			if strings.HasPrefix(name, prefix) {
				return ErrConflict
			}
		}
		for name := range m.dirs {
			if name != key && strings.HasPrefix(name, prefix) {
				return ErrConflict
			}
		}
		delete(m.dirs, key)
		return nil
	}
	return ErrNotFound
}

func (m *memoryStore) Mkdir(ctx context.Context, key string) error {
	key, err := normalizeKey(key, true)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.objects[key]; exists {
		return ErrConflict
	}
	for dir := key; dir != ""; {
		m.dirs[dir] = struct{}{}
		parent := path.Dir(dir)
		if parent == "." {
			break
		}
		dir = parent
	}
	return nil
}

func (m *memoryStore) isDirLocked(key string) bool {
	if _, ok := m.dirs[key]; ok {
		return true
	}
	prefix := key
	if prefix != "" {
		prefix += "/"
	}
	for name := range m.objects {
		if strings.HasPrefix(name, prefix) && name != key {
			return true
		}
	}
	for name := range m.dirs {
		if strings.HasPrefix(name, prefix) && name != key {
			return true
		}
	}
	return key == ""
}

var _ Store = (*memoryStore)(nil)
var _ RangeStore = (*memoryStore)(nil)
