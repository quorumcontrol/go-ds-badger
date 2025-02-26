package badger

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v2"
	ds "github.com/ipfs/go-datastore"
	dsq "github.com/ipfs/go-datastore/query"
	logger "github.com/ipfs/go-log"
	goprocess "github.com/jbenet/goprocess"
)

var log = logger.Logger("badger")

var ErrClosed = errors.New("datastore closed")

type Datastore struct {
	DB *badger.DB

	closeLk   sync.RWMutex
	closed    bool
	closeOnce sync.Once
	closing   chan struct{}

	gcDiscardRatio float64
}

// Implements the datastore.Txn interface, enabling transaction support for
// the badger Datastore.
type txn struct {
	ds  *Datastore
	txn *badger.Txn

	// Whether this transaction has been implicitly created as a result of a direct Datastore
	// method invocation.
	implicit bool
}

// Options are the badger datastore options, reexported here for convenience.
type Options struct {
	gcDiscardRatio float64

	badger.Options
}

// DefaultOptions are the default options for the badger datastore.
var DefaultOptions Options

func init() {
	DefaultOptions = Options{
		gcDiscardRatio: 0.1,
		Options:        badger.DefaultOptions,
	}
	DefaultOptions.Options.CompactL0OnClose = false
	DefaultOptions.Options.Truncate = true
}

var _ ds.Datastore = (*Datastore)(nil)
var _ ds.TxnDatastore = (*Datastore)(nil)
var _ ds.TTLDatastore = (*Datastore)(nil)

// NewDatastore creates a new badger datastore.
//
// DO NOT set the Dir and/or ValuePath fields of opt, they will be set for you.
func NewDatastore(path string, options *Options) (*Datastore, error) {
	// Copy the options because we modify them.
	var opt badger.Options
	var gcDiscardRatio float64
	if options == nil {
		opt = badger.DefaultOptions
		gcDiscardRatio = DefaultOptions.gcDiscardRatio
	} else {
		opt = options.Options
		gcDiscardRatio = options.gcDiscardRatio
	}

	opt.Dir = path
	opt.ValueDir = path
	opt.Logger = log

	kv, err := badger.Open(opt)
	if err != nil {
		if strings.HasPrefix(err.Error(), "manifest has unsupported version:") {
			err = fmt.Errorf("unsupported badger version, use github.com/ipfs/badgerds-upgrade to upgrade: %s", err.Error())
		}
		return nil, err
	}

	return &Datastore{
		DB:             kv,
		closing:        make(chan struct{}),
		gcDiscardRatio: gcDiscardRatio,
	}, nil
}

// NewTransaction starts a new transaction. The resulting transaction object
// can be mutated without incurring changes to the underlying Datastore until
// the transaction is Committed.
func (d *Datastore) NewTransaction(readOnly bool) (ds.Txn, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}

	return &txn{d, d.DB.NewTransaction(!readOnly), false}, nil
}

// newImplicitTransaction creates a transaction marked as 'implicit'.
// Implicit transactions are created by Datastore methods performing single operations.
func (d *Datastore) newImplicitTransaction(readOnly bool) *txn {
	return &txn{d, d.DB.NewTransaction(!readOnly), true}
}

func (d *Datastore) Put(key ds.Key, value []byte) error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return ErrClosed
	}

	txn := d.newImplicitTransaction(false)
	defer txn.discard()

	if err := txn.put(key, value); err != nil {
		return err
	}

	return txn.commit()
}

func (d *Datastore) PutWithTTL(key ds.Key, value []byte, ttl time.Duration) error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return ErrClosed
	}

	txn := d.newImplicitTransaction(false)
	defer txn.discard()

	if err := txn.putWithTTL(key, value, ttl); err != nil {
		return err
	}

	return txn.commit()
}

func (d *Datastore) SetTTL(key ds.Key, ttl time.Duration) error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return ErrClosed
	}

	txn := d.newImplicitTransaction(false)
	defer txn.discard()

	if err := txn.setTTL(key, ttl); err != nil {
		return err
	}

	return txn.commit()
}

func (d *Datastore) GetExpiration(key ds.Key) (time.Time, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return time.Time{}, ErrClosed
	}

	txn := d.newImplicitTransaction(false)
	defer txn.discard()

	return txn.getExpiration(key)
}

func (d *Datastore) Get(key ds.Key) (value []byte, err error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}

	txn := d.newImplicitTransaction(true)
	defer txn.discard()

	return txn.get(key)
}

func (d *Datastore) Has(key ds.Key) (bool, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return false, ErrClosed
	}

	txn := d.newImplicitTransaction(true)
	defer txn.discard()

	return txn.has(key)
}

func (d *Datastore) GetSize(key ds.Key) (size int, err error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return -1, ErrClosed
	}

	txn := d.newImplicitTransaction(true)
	defer txn.discard()

	return txn.getSize(key)
}

func (d *Datastore) Delete(key ds.Key) error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()

	txn := d.newImplicitTransaction(false)
	defer txn.discard()

	err := txn.delete(key)
	if err != nil {
		return err
	}

	return txn.commit()
}

func (d *Datastore) Query(q dsq.Query) (dsq.Results, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()

	txn := d.newImplicitTransaction(true)
	// We cannot defer txn.Discard() here, as the txn must remain active while the iterator is open.
	// https://github.com/dgraph-io/badger/commit/b1ad1e93e483bbfef123793ceedc9a7e34b09f79
	// The closing logic in the query goprocess takes care of discarding the implicit transaction.
	return txn.query(q)
}

// DiskUsage implements the PersistentDatastore interface.
// It returns the sum of lsm and value log files sizes in bytes.
func (d *Datastore) DiskUsage() (uint64, error) {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return 0, ErrClosed
	}
	lsm, vlog := d.DB.Size()
	return uint64(lsm + vlog), nil
}

func (d *Datastore) Close() error {
	d.closeOnce.Do(func() {
		close(d.closing)
	})
	d.closeLk.Lock()
	defer d.closeLk.Unlock()
	if d.closed {
		return ErrClosed
	}
	d.closed = true
	return d.DB.Close()
}

func (d *Datastore) Batch() (ds.Batch, error) {
	tx, _ := d.NewTransaction(false)
	return tx, nil
}

func (d *Datastore) CollectGarbage() error {
	d.closeLk.RLock()
	defer d.closeLk.RUnlock()
	if d.closed {
		return ErrClosed
	}

	err := d.DB.RunValueLogGC(d.gcDiscardRatio)
	if err == badger.ErrNoRewrite {
		err = nil
	}
	return err
}

var _ ds.Datastore = (*txn)(nil)
var _ ds.TTLDatastore = (*txn)(nil)

func (t *txn) Put(key ds.Key, value []byte) error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}
	return t.put(key, value)
}

func (t *txn) put(key ds.Key, value []byte) error {
	return t.txn.Set(key.Bytes(), value)
}

func (t *txn) PutWithTTL(key ds.Key, value []byte, ttl time.Duration) error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}
	return t.putWithTTL(key, value, ttl)
}

func (t *txn) putWithTTL(key ds.Key, value []byte, ttl time.Duration) error {
	return t.txn.SetEntry(badger.NewEntry(key.Bytes(), value).WithTTL(ttl))
}

func (t *txn) GetExpiration(key ds.Key) (time.Time, error) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return time.Time{}, ErrClosed
	}

	return t.getExpiration(key)
}

func (t *txn) getExpiration(key ds.Key) (time.Time, error) {
	item, err := t.txn.Get(key.Bytes())
	if err == badger.ErrKeyNotFound {
		return time.Time{}, ds.ErrNotFound
	} else if err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(item.ExpiresAt()), 0), nil
}

func (t *txn) SetTTL(key ds.Key, ttl time.Duration) error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}

	return t.setTTL(key, ttl)
}

func (t *txn) setTTL(key ds.Key, ttl time.Duration) error {
	item, err := t.txn.Get(key.Bytes())
	if err != nil {
		return err
	}
	return item.Value(func(data []byte) error {
		return t.putWithTTL(key, data, ttl)
	})

}

func (t *txn) Get(key ds.Key) ([]byte, error) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return nil, ErrClosed
	}

	return t.get(key)
}

func (t *txn) get(key ds.Key) ([]byte, error) {
	item, err := t.txn.Get(key.Bytes())
	if err == badger.ErrKeyNotFound {
		err = ds.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	return item.ValueCopy(nil)
}

func (t *txn) Has(key ds.Key) (bool, error) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return false, ErrClosed
	}

	return t.has(key)
}

func (t *txn) has(key ds.Key) (bool, error) {
	_, err := t.txn.Get(key.Bytes())
	switch err {
	case badger.ErrKeyNotFound:
		return false, nil
	case nil:
		return true, nil
	default:
		return false, err
	}
}

func (t *txn) GetSize(key ds.Key) (int, error) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return -1, ErrClosed
	}

	return t.getSize(key)
}

func (t *txn) getSize(key ds.Key) (int, error) {
	item, err := t.txn.Get(key.Bytes())
	switch err {
	case nil:
		return int(item.ValueSize()), nil
	case badger.ErrKeyNotFound:
		return -1, ds.ErrNotFound
	default:
		return -1, err
	}
}

func (t *txn) Delete(key ds.Key) error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}

	return t.delete(key)
}

func (t *txn) delete(key ds.Key) error {
	return t.txn.Delete(key.Bytes())
}

func (t *txn) Query(q dsq.Query) (dsq.Results, error) {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return nil, ErrClosed
	}

	return t.query(q)
}

func (t *txn) query(q dsq.Query) (dsq.Results, error) {
	prefix := []byte(q.Prefix)
	opt := badger.DefaultIteratorOptions
	opt.PrefetchValues = !q.KeysOnly

	// Special case order by key.
	orders := q.Orders
	if len(orders) > 0 {
		switch q.Orders[0].(type) {
		case dsq.OrderByKey, *dsq.OrderByKey:
			// Already ordered by key.
			orders = nil
		case dsq.OrderByKeyDescending, *dsq.OrderByKeyDescending:
			orders = nil
			opt.Reverse = true
		}
	}

	txn := t.txn

	it := txn.NewIterator(opt)
	it.Seek(prefix)

	if q.Offset > 0 {
		for j := 0; j < q.Offset; j++ {
			it.Next()
		}
	}

	qrb := dsq.NewResultBuilder(q)

	qrb.Process.Go(func(worker goprocess.Process) {
		t.ds.closeLk.RLock()
		closedEarly := false
		defer func() {
			t.ds.closeLk.RUnlock()
			if closedEarly {
				select {
				case qrb.Output <- dsq.Result{
					Error: ErrClosed,
				}:
				case <-qrb.Process.Closing():
				}
			}

		}()
		if t.ds.closed {
			closedEarly = true
			return
		}

		// this iterator is part of an implicit transaction, so when
		// we're done we must discard the transaction. It's safe to
		// discard the txn it because it contains the iterator only.
		if t.implicit {
			defer t.discard()
		}

		defer it.Close()

		for sent := 0; it.ValidForPrefix(prefix); sent++ {
			if qrb.Query.Limit > 0 && sent >= qrb.Query.Limit {
				break
			}

			item := it.Item()

			k := string(item.Key())
			e := dsq.Entry{Key: k}

			var result dsq.Result
			if !q.KeysOnly {
				b, err := item.ValueCopy(nil)
				if err != nil {
					result = dsq.Result{Error: err}
				} else {
					e.Value = b
					result = dsq.Result{Entry: e}
				}
			} else {
				result = dsq.Result{Entry: e}
			}

			if q.ReturnExpirations {
				result.Expiration = time.Unix(int64(item.ExpiresAt()), 0)
			}

			select {
			case qrb.Output <- result:
			case <-t.ds.closing: // datastore closing.
				closedEarly = true
				return
			case <-worker.Closing(): // client told us to close early
				return
			}

			it.Next()
		}

		return
	})

	go qrb.Process.CloseAfterChildren()

	// Now, apply remaining things (filters, order)
	qr := qrb.Results()
	for _, f := range q.Filters {
		qr = dsq.NaiveFilter(qr, f)
	}
	if len(orders) > 0 {
		qr = dsq.NaiveOrder(qr, orders...)
	}

	return qr, nil
}

func (t *txn) Commit() error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}

	return t.commit()
}

func (t *txn) commit() error {
	return t.txn.Commit()
}

// Alias to commit
func (t *txn) Close() error {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return ErrClosed
	}
	return t.close()
}

func (t *txn) close() error {
	return t.txn.Commit()
}

func (t *txn) Discard() {
	t.ds.closeLk.RLock()
	defer t.ds.closeLk.RUnlock()
	if t.ds.closed {
		return
	}

	t.discard()
}

func (t *txn) discard() {
	t.txn.Discard()
}
