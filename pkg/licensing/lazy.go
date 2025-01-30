// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package licensing

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Lazy represents a value whose computation is deferred until the first access
type Lazy[I, O any] interface {
	// Get returns the value, computing it if necessary.
	Get() (O, error)
	// SetInput declares the input parameters. This must be called before Get() is called or an error will be returned.
	// Must only be set once.
	SetInput(I) error
}

type lazyImpl[I, O any] struct {
	getter func(*I) (O, error)

	// Cached responses. Note: with retry enabled, this will be unset until a non-nil error
	res   O
	input *I
	err   error

	done uint32
	m    sync.Mutex
}

var _ Lazy[any, any] = &lazyImpl[any, any]{}

// New returns a new lazily computed value. The value is guaranteed to only be computed a single time.
func NewLazy[I, O any](f func(*I) (O, error)) Lazy[I, O] {
	return &lazyImpl[I, O]{getter: f}
}

func (l *lazyImpl[I, O]) Get() (O, error) {
	if atomic.LoadUint32(&l.done) == 0 {
		// Outlined slow-path to allow inlining of the fast-path.
		return l.doSlow()
	}
	return l.res, l.err
}

func (l *lazyImpl[I, O]) doSlow() (O, error) {
	l.m.Lock()
	defer l.m.Unlock()
	if l.done == 0 {
		done := uint32(1)
		// Defer in case of panic
		defer func() {
			atomic.StoreUint32(&l.done, done)
		}()

		res, err := l.getter(l.input)
		l.res, l.err = res, err
		return res, err
	}
	return l.res, l.err
}

func (l *lazyImpl[I, O]) SetInput(i I) error {
	l.m.Lock()
	defer l.m.Unlock()
	if l.input != nil {
		return fmt.Errorf("invalid: SetInput called twice")
	}
	l.input = &i
	return nil
}
