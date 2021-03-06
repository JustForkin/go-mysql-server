package sql

import (
	"context"
	"fmt"
	"hash"
	"io"
	"reflect"
	"strings"
	"sync"

	"gopkg.in/src-d/go-errors.v1"
)

// IndexKeyValueIter is an iterator of index key values, that is, a tuple of
// the values that will be index keys.
type IndexKeyValueIter interface {
	// Next returns the next tuple of index key values. The length of the
	// returned slice will be the same as the number of columns used to
	// create this iterator. The second returned parameter is a repo's location.
	Next() ([]interface{}, []byte, error)
	io.Closer
}

// IndexValueIter is an iterator of index values.
type IndexValueIter interface {
	// Next returns the next value (repo's location) - see IndexKeyValueIter.
	Next() ([]byte, error)
	io.Closer
}

// Index is the basic representation of an index. It can be extended with
// more functionality by implementing more specific interfaces.
type Index interface {
	// Get returns an IndexLookup for the given key in the index.
	Get(key interface{}) (IndexLookup, error)
	// Has checks if the given key is present in the index.
	Has(key interface{}) (bool, error)
	// ID returns the identifier of the index.
	ID() string
	// Database returns the database name this index belongs to.
	Database() string
	// Table returns the table name this index belongs to.
	Table() string
	// Expressions returns the indexed expressions. If the result is more than
	// one expression, it means the index has multiple columns indexed. If it's
	// just one, it means it may be an expression or a column.
	ExpressionHashes() []hash.Hash
}

// AscendIndex is an index that is sorted in ascending order.
type AscendIndex interface {
	// AscendGreaterOrEqual returns an IndexLookup for keys that are greater
	// or equal to the given key.
	AscendGreaterOrEqual(key interface{}) (IndexLookup, error)
	// AscendLessThan returns an IndexLookup for keys that are less than the
	// given key.
	AscendLessThan(key interface{}) (IndexLookup, error)
	// AscendRange returns an IndexLookup for keys that are within the given
	// range.
	AscendRange(greaterOrEqual, lessThan interface{}) (IndexLookup, error)
}

// DescendIndex is an index that is sorted in descending order.
type DescendIndex interface {
	// DescendGreater returns an IndexLookup for keys that are greater
	// than the given key.
	DescendGreater(key interface{}) (IndexLookup, error)
	// DescendLessOrEqual returns an IndexLookup for keys that are less than or
	// equal to the given key.
	DescendLessOrEqual(key interface{}) (IndexLookup, error)
	// DescendRange returns an IndexLookup for keys that are within the given
	// range.
	DescendRange(lessOrEqual, greaterThan interface{}) (IndexLookup, error)
}

// IndexLookup is a subset of an index. More specific interfaces can be
// implemented to grant more capabilities to the index lookup.
type IndexLookup interface {
	// Values returns the values in the subset of the index.
	Values() IndexValueIter
}

// SetOperations is a specialization of IndexLookup that enables set operations
// between several IndexLookups.
type SetOperations interface {
	// Intersection returns a new index subset with the intersection of the
	// current IndexLookup and the ones given.
	Intersection(...IndexLookup) IndexLookup
	// Union returns a new index subset with the union of the current
	// IndexLookup and the ones given.
	Union(...IndexLookup) IndexLookup
	// Difference returns a new index subset with the difference between the
	// current IndexLookup and the ones given.
	Difference(...IndexLookup) IndexLookup
}

// Mergeable is a specialization of IndexLookup to check if an IndexLookup can
// be merged with another one.
type Mergeable interface {
	// IsMergeable checks whether the current IndexLookup can be merged with
	// the given one.
	IsMergeable(IndexLookup) bool
}

// IndexDriver manages the coordination between the indexes and their
// representation in disk.
type IndexDriver interface {
	// ID returns the unique name of the driver.
	ID() string
	// Create a new index. If exprs is more than one expression, it means the
	// index has multiple columns indexed. If it's just one, it means it may
	// be an expression or a column.
	Create(db, table, id string, expressionHashes []hash.Hash, config map[string]string) (Index, error)
	// Load the index at the given path.
	Load(db, table string) ([]Index, error)
	// Save the given index at the given path.
	Save(ctx context.Context, index Index, iter IndexKeyValueIter) error
	// Delete the index with the given path.
	Delete(index Index) error
}

type indexKey struct {
	db, id string
}

// IndexRegistry keeps track of all indexes in the engine.
type IndexRegistry struct {
	// Root path where all the data of the indexes is stored on disk.
	Root string

	mut      sync.RWMutex
	indexes  map[indexKey]Index
	statuses map[indexKey]IndexStatus

	driversMut sync.RWMutex
	drivers    map[string]IndexDriver

	rcmut            sync.RWMutex
	refCounts        map[indexKey]int
	deleteIndexQueue map[indexKey]chan<- struct{}
}

// NewIndexRegistry returns a new Index Registry.
func NewIndexRegistry() *IndexRegistry {
	return &IndexRegistry{
		indexes:          make(map[indexKey]Index),
		statuses:         make(map[indexKey]IndexStatus),
		drivers:          make(map[string]IndexDriver),
		refCounts:        make(map[indexKey]int),
		deleteIndexQueue: make(map[indexKey]chan<- struct{}),
	}
}

// IndexDriver returns the IndexDriver with the given ID.
func (r *IndexRegistry) IndexDriver(id string) IndexDriver {
	r.driversMut.RLock()
	defer r.driversMut.RUnlock()
	return r.drivers[id]
}

// RegisterIndexDriver registers a new index driver.
func (r *IndexRegistry) RegisterIndexDriver(driver IndexDriver) {
	r.driversMut.Lock()
	defer r.driversMut.Unlock()
	r.drivers[driver.ID()] = driver
}

func (r *IndexRegistry) retainIndex(db, id string) {
	r.rcmut.Lock()
	defer r.rcmut.Unlock()
	key := indexKey{db, id}
	r.refCounts[key] = r.refCounts[key] + 1
}

// CanUseIndex returns whether the given index is ready to use or not.
func (r *IndexRegistry) CanUseIndex(idx Index) bool {
	r.mut.RLock()
	defer r.mut.RUnlock()
	return bool(r.statuses[indexKey{idx.Database(), idx.ID()}])
}

// setStatus is not thread-safe, it should be guarded using mut.
func (r *IndexRegistry) setStatus(idx Index, status IndexStatus) {
	r.statuses[indexKey{idx.Database(), idx.ID()}] = status
}

// ReleaseIndex releases an index after it's been used.
func (r *IndexRegistry) ReleaseIndex(idx Index) {
	r.rcmut.Lock()
	defer r.rcmut.Unlock()
	key := indexKey{idx.Database(), idx.ID()}
	r.refCounts[key] = r.refCounts[key] - 1

	if r.refCounts[key] > 0 {
		return
	}

	if ch, ok := r.deleteIndexQueue[key]; ok {
		close(ch)
		delete(r.deleteIndexQueue, key)
	}
}

// Index returns the index with the given id. It may return nil if the index is
// not found.
func (r *IndexRegistry) Index(db, id string) Index {
	r.mut.RLock()
	defer r.mut.RUnlock()
	return r.indexes[indexKey{db, strings.ToLower(id)}]
}

// IndexByExpression returns an index by the given expression. It will return
// nil it the index is not found. If more than one expression is given, all
// of them must match for the index to be matched.
func (r *IndexRegistry) IndexByExpression(db string, expr ...Expression) Index {
	r.mut.RLock()
	defer r.mut.RUnlock()

	var expressionHashes []hash.Hash
	for _, e := range expr {
		expressionHashes = append(expressionHashes, NewExpressionHash(e))
	}

	for _, idx := range r.indexes {
		if idx.Database() == db {
			if exprListsEqual(idx.ExpressionHashes(), expressionHashes) {
				r.retainIndex(db, idx.ID())
				return idx
			}
		}
	}

	return nil
}

var (
	// ErrIndexIDAlreadyRegistered is the error returned when there is already
	// an index with the same ID.
	ErrIndexIDAlreadyRegistered = errors.NewKind("an index with id %q has already been registered")

	// ErrIndexExpressionAlreadyRegistered is the error returned when there is
	// already an index with the same expression.
	ErrIndexExpressionAlreadyRegistered = errors.NewKind("there is already an index registered for the expressions: %s")

	// ErrIndexNotFound is returned when the index could not be found.
	ErrIndexNotFound = errors.NewKind("index %q	was not found")

	// ErrIndexDeleteInvalidStatus is returned when the index trying to delete
	// does not have a ready state.
	ErrIndexDeleteInvalidStatus = errors.NewKind("can't delete index %q because it's not ready for usage")
)

func (r *IndexRegistry) validateIndexToAdd(idx Index) error {
	r.mut.RLock()
	defer r.mut.RUnlock()

	for _, i := range r.indexes {
		if i.Database() != idx.Database() {
			continue
		}

		if i.ID() == idx.ID() {
			return ErrIndexIDAlreadyRegistered.New(idx.ID())
		}

		if exprListsEqual(i.ExpressionHashes(), idx.ExpressionHashes()) {
			var exprs = make([]string, len(idx.ExpressionHashes()))
			for i, e := range idx.ExpressionHashes() {
				exprs[i] = fmt.Sprintf("%x", e.Sum(nil))
			}
			return ErrIndexExpressionAlreadyRegistered.New(strings.Join(exprs, ", "))
		}
	}

	return nil
}

func exprListsEqual(a, b []hash.Hash) bool {
	var visited = make([]bool, len(b))
	for _, va := range a {
		found := false
		for j, vb := range b {
			if visited[j] {
				continue
			}

			if reflect.DeepEqual(va.Sum(nil), vb.Sum(nil)) {
				visited[j] = true
				found = true
				break
			}
		}

		if !found {
			return false
		}
	}

	return true
}

// AddIndex adds the given index to the registry. The added index will be
// marked as creating, so nobody can't register two indexes with the same
// expression or id while the other is still being created.
// When something is sent through the returned channel, it means the index has
// finished it's creation and will be marked as ready.
func (r *IndexRegistry) AddIndex(idx Index) (chan<- struct{}, error) {
	if err := r.validateIndexToAdd(idx); err != nil {
		return nil, err
	}

	r.mut.Lock()
	r.setStatus(idx, IndexNotReady)
	r.indexes[indexKey{idx.Database(), idx.ID()}] = idx
	r.mut.Unlock()

	var created = make(chan struct{})
	go func() {
		<-created
		r.mut.Lock()
		defer r.mut.Unlock()
		r.setStatus(idx, IndexReady)
	}()

	return created, nil
}

// DeleteIndex deletes an index from the registry by its id. First, it marks
// the index for deletion but does not remove it, so queries that are using it
// may still do so. The returned channel will send a message when the index can
// be deleted from disk.
func (r *IndexRegistry) DeleteIndex(db, id string) (<-chan struct{}, error) {
	r.mut.RLock()
	var key indexKey
	for k, idx := range r.indexes {
		if strings.ToLower(id) == idx.ID() {
			if !r.CanUseIndex(idx) {
				r.mut.RUnlock()
				return nil, ErrIndexDeleteInvalidStatus.New(id)
			}
			r.setStatus(idx, IndexNotReady)
			key = k
			break
		}
	}
	r.mut.RUnlock()

	if key.id == "" {
		return nil, ErrIndexNotFound.New(id)
	}

	var done = make(chan struct{}, 1)

	r.rcmut.Lock()
	// If no query is using this index just delete it right away
	if r.refCounts[key] == 0 {
		r.mut.Lock()
		defer r.mut.Unlock()
		defer r.rcmut.Unlock()

		delete(r.indexes, key)
		close(done)
		return done, nil
	}

	var onReadyToDelete = make(chan struct{})
	r.deleteIndexQueue[key] = onReadyToDelete
	r.rcmut.Unlock()

	go func() {
		<-onReadyToDelete
		r.mut.Lock()
		defer r.mut.Unlock()
		delete(r.indexes, key)

		done <- struct{}{}
	}()

	return done, nil
}

// IndexStatus represents the current status in which the index is.
type IndexStatus bool

const (
	// IndexNotReady means the index is not ready to be used.
	IndexNotReady IndexStatus = false
	// IndexReady means the index can be used.
	IndexReady IndexStatus = true
)

// IsUsable returns whether the index can be used or not based on the status.
func (s IndexStatus) IsUsable() bool {
	return s == IndexReady
}

func (s IndexStatus) String() string {
	switch s {
	case IndexReady:
		return "ready"
	default:
		return "not ready"
	}
}
