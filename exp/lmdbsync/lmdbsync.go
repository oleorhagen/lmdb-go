/*
Package lmdbsync provides advanced synchronization for LMDB environments at the
cost of performance.  The package provides a drop-in replacement for *lmdb.Env
that can be used in situations where the database may be resized or where the
flag lmdb.NoLock is used.

Bypassing an Env's methods to access the underlying lmdb.Env is safe.  The
severity of such usage depends such behavior should be strictly avoided as it
may produce undefined behavior from the LMDB C library.

Resizing the environment

The Env type synchronizes all calls to Env.SetMapSize so that it may be safely
called in the presence of concurrent transactinos after an environment has been
opened.  All running transactions complete before the method is called on the
underlying lmdb.Env.

However, appplications are recommended against attempting to change the memory
map size for an open database.  It requires careful synchronization by all
processes accessing the database file.  And, a large memory map will not affect
disk usage on operating systems that support sparse files (e.g. Linux, not OS
X).

See mdb_env_set_mapsize.

Multi-processing

The Env type intercepts any MapResized error returned from a transaction and
transparently handles it, retrying the transaction after the new size has been
adopted.  All synchronization is handled so running transactions complete
before SetMapSize is called on the underlying lmdb.Env.

See mdb_txn_begin and MDB_MAP_RESIZED.

NoLock

The lmdb.NoLock flag performs all transaction synchronization with Go
structures and is an experimental feature.  It is unclear what benefits this
provides.

See mdb_env_open and MDB_NOLOCK.

MapFull

The Env type does no special handling of the MapFull error.  If a call to
Txn.Put() or Cursor.Put() returns lmdb.MapFull it is the application's
prerogative to detect the error, call Env.SetMapSize, and retry the transaction
as necessary.

See mdb_env_set_mapsize and MDB_MAP_FULL.
*/
package lmdbsync

import (
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/bmatsuo/lmdb-go/lmdb"
)

type envBagKey int

func BagEnv(b Bag) *Env {
	env, _ := b.Value(envBagKey(0)).(*Env)
	return env
}

func bagWithEnv(b Bag, env *Env) Bag {
	return BagWith(b, envBagKey(0), env)
}

// Env wraps an *lmdb.Env, receiving all the same methods and proxying some to
// provide transaction management.  Transactions run by an Env handle
// lmdb.MapResized error transparently through additional synchronization.
// Additionally, Env is safe to use on environments setting the lmdb.NoLock
// flag.  When in NoLock mode write transactions block all read transactions
// from running (in addition to blocking other write transactions like a normal
// lmdb.Env would).
//
// Env proxies several methods to provide synchronization required for safe
// operation in some scenarios.  It is important not byprass proxies and call
// the methods directly on the underlying lmdb.Env or synchronization may be
// interfered with.  Calling proxied methods directly on the lmdb.Env may
// result in poor transaction performance or unspecified behavior in from the C
// library.
type Env struct {
	*lmdb.Env
	Handlers HandlerChain
	bag      Bag
	noLock   bool
	txnlock  sync.RWMutex
}

// NewEnv returns an newly allocated Env that wraps env.  If env is nil then
// lmdb.NewEnv() will be called to allocate an lmdb.Env.
func NewEnv(env *lmdb.Env, h ...Handler) (*Env, error) {
	var err error
	if env == nil {
		env, err = lmdb.NewEnv()
		if err != nil {
			return nil, err
		}
	}

	flags, err := env.Flags()
	if lmdb.IsErrnoSys(err, syscall.EINVAL) {
		err = nil
	} else if err != nil {
		return nil, err
	}
	noLock := flags&lmdb.NoLock != 0

	chain := append(HandlerChain(nil), h...)

	_env := &Env{
		Env:      env,
		Handlers: chain,
		noLock:   noLock,
		bag:      Background(),
	}
	return _env, nil
}

// Open is a proxy for r.Env.Open() that detects the lmdb.NoLock flag to
// properly manage transaction synchronization.
func (r *Env) Open(path string, flags uint, mode os.FileMode) error {
	err := r.Env.Open(path, flags, mode)
	if err != nil {
		// no update to flags occurred
		return err
	}

	if flags&lmdb.NoLock != 0 {
		r.noLock = true
	}

	return nil
}

// SetFlags is a proxy for r.Env.SetFlags() that detects the lmdb.NoLock flag
// to properly manage transaction synchronization.
func (r *Env) SetFlags(flags uint) error {
	err := r.Env.SetFlags(flags)
	if err != nil {
		// no update to flags occurred
		return err
	}

	if flags&lmdb.NoLock != 0 {
		r.noLock = true
	}

	return nil
}

// UnsetFlags is a proxy for r.Env.UnsetFlags() that detects the lmdb.NoLock flag
// to properly manage transaction synchronization.
func (r *Env) UnsetFlags(flags uint) error {
	err := r.Env.UnsetFlags(flags)
	if err != nil {
		// no update to flags occurred
		return err
	}

	if flags&lmdb.NoLock != 0 {
		r.noLock = false
	}

	return nil
}

// SetMapSize is a proxy for r.Env.SetMapSize() that blocks while concurrent
// transactions are in progress.
func (r *Env) SetMapSize(size int64) error {
	r.txnlock.Lock()
	err := r.setMapSize(size, 0)
	r.txnlock.Unlock()
	return err
}

func (r *Env) setMapSize(size int64, delay time.Duration) error {
	r.txnlock.Lock()
	if delay > 0 {
		// wait before adopting a map size set from another process. hold on to
		// the transaction lock so that other transactions don't attempt to
		// begin while waiting.
		time.Sleep(delay)
	}
	err := r.Env.SetMapSize(size)
	r.txnlock.Unlock()
	return err
}

// RunTxn is a proxy for r.Env.RunTxn().
//
// If lmdb.NoLock is set on r.Env then RunTxn will block while other updates
// are in progress, regardless of flags.
//
// If RunTxn returns MapResized it means another process(es) was writing too
// fast to the database and the calling process could not get a valid
// transaction handle.
func (r *Env) RunTxn(flags uint, op lmdb.TxnOp) (err error) {
	readonly := flags&lmdb.Readonly != 0
	return r.runHandler(readonly, func() error { return r.Env.RunTxn(flags, op) }, r.Handlers)
}

// View is a proxy for r.Env.RunTxn().
//
// If lmdb.NoLock is set on r.Env then View will block until any running update
// completes.
//
// If View returns MapResized it means another process(es) was writing too fast
// to the database and the calling process could not get a valid transaction
// handle.
func (r *Env) View(op lmdb.TxnOp) error {
	return r.runHandler(true, func() error { return r.Env.View(op) }, r.Handlers)
}

// Update is a proxy for r.Env.RunTxn().
//
// If lmdb.NoLock is set on r.Env then Update blocks until all other
// transactions have terminated and blocks all other transactions from running
// while in progress (including readonly transactions).
//
// If Update returns MapResized it means another process(es) was writing too
// fast to the database and the calling process could not get a valid
// transaction handle.
func (r *Env) Update(op lmdb.TxnOp) error {
	return r.runHandler(false, func() error { return r.Env.Update(op) }, r.Handlers)
}

// UpdateLocked is a proxy for r.Env.RunTxn().
//
// If lmdb.NoLock is set on r.Env then UpdateLocked blocks until all other
// transactions have terminated and blocks all other transactions from running
// while in progress (including readonly transactions).
//
// If UpdateLocked returns MapResized it means another process(es) was writing
// too fast to the database and the calling process could not get a valid
// transaction handle.
func (r *Env) UpdateLocked(op lmdb.TxnOp) error {
	return r.runHandler(false, func() error { return r.Env.UpdateLocked(op) }, r.Handlers)
}

// WithHandler returns a TxnRunner than handles transaction errors r.Handlers
// chained with h.
func (r *Env) WithHandler(h Handler) TxnRunner {
	return &handlerRunner{
		env: r,
		h:   r.Handlers.Append(h),
	}
}

func (r *Env) runHandler(readonly bool, fn func() error, h Handler) error {
	b := bagWithEnv(r.bag, r)
	for {
		err := r.run(readonly, fn)
		b, err = h.HandleTxnErr(b, err)
		if err != RetryTxn {
			return err
		}
	}
}
func (r *Env) run(readonly bool, fn func() error) error {
	var err error
	if r.noLock && !readonly {
		r.txnlock.Lock()
		err = fn()
		r.txnlock.Unlock()
	} else {
		r.txnlock.RLock()
		err = fn()
		r.txnlock.RUnlock()
	}
	return err
}
