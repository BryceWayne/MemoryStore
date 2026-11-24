package memorystore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var errGoexit = errors.New("singleflight: loader exited via runtime.Goexit")

type loadOptions struct {
	staleGrace    time.Duration
	hasStaleGrace bool
	err           error
}

// LoadOption configures per-call behavior for GetOrLoadWithOptions and
// GetOrLoadJSONWithOptions.
type LoadOption func(*loadOptions)

// WithStaleWhileRevalidate configures the Stale-While-Revalidate (SWR) grace
// window for a load call, overriding any store-wide DefaultStaleWhileRevalidate
// or per-entry WithStaleWindow. Negative grace durations fail the load call
// with a validation error.
func WithStaleWhileRevalidate(grace time.Duration) LoadOption {
	return func(o *loadOptions) {
		if grace < 0 {
			if o.err == nil {
				o.err = fmt.Errorf("invalid stale-while-revalidate grace %v: must be >= 0", grace)
			}
			return
		}
		o.staleGrace = grace
		o.hasStaleGrace = true
	}
}

func swrSetOptions(opts loadOptions, entryGrace int64) setOptions {
	if opts.hasStaleGrace {
		return setOptions{
			staleGrace:    opts.staleGrace,
			hasStaleGrace: true,
		}
	}
	if entryGrace > 0 {
		return setOptions{
			staleGrace:    time.Duration(entryGrace),
			hasStaleGrace: true,
		}
	}
	return setOptions{}
}

// singleflightCall represents an in-flight cache load for a single key.
type singleflightCall struct {
	done     chan struct{}
	val      []byte
	err      error
	panicked bool
	panicVal any
}

// singleflightGroup coalesces concurrent cache-miss loads for identical keys
// without external dependencies.
type singleflightGroup struct {
	mu    sync.Mutex
	calls map[string]*singleflightCall
}

func (g *singleflightGroup) do(ctx context.Context, key string, fn func(context.Context) ([]byte, error)) (val []byte, shared bool, err error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*singleflightCall)
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-c.done:
			if c.panicked {
				panic(c.panicVal)
			}
			return c.val, true, c.err
		case <-ctx.Done():
			return nil, true, ctx.Err()
		}
	}

	c := &singleflightCall{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	normalReturn := false
	recovered := false
	defer func() {
		if !normalReturn && !recovered {
			c.err = errGoexit
		}
		g.mu.Lock()
		if cur, ok := g.calls[key]; ok && cur == c {
			delete(g.calls, key)
		}
		g.mu.Unlock()
		close(c.done)
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				c.panicked = true
				c.panicVal = r
				recovered = true
			}
		}()
		c.val, c.err = fn(ctx)
		normalReturn = true
	}()

	if c.panicked {
		panic(c.panicVal)
	}
	return c.val, false, c.err
}

func (g *singleflightGroup) startAsync(key string) (*singleflightCall, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.calls == nil {
		g.calls = make(map[string]*singleflightCall)
	}
	if _, ok := g.calls[key]; ok {
		return nil, false
	}
	c := &singleflightCall{done: make(chan struct{})}
	g.calls[key] = c
	return c, true
}

func (g *singleflightGroup) finishAsync(key string, c *singleflightCall) {
	g.mu.Lock()
	if cur, ok := g.calls[key]; ok && cur == c {
		delete(g.calls, key)
	}
	g.mu.Unlock()
	close(c.done)
}

// GetOrLoad returns the cached byte slice for key if present and unexpired.
// If the entry is within its Stale-While-Revalidate grace window, GetOrLoad
// returns the stale value immediately and coalesces a detached-context
// background refresh. On a cache miss, concurrent callers for the same key are
// coalesced via singleflight so loader executes only once; the returned value is
// stored with ttl when loader succeeds.
//
// If loader panics on a foreground miss, the panic is propagated to the leader
// and all coalesced waiters while cleaning up in-flight singleflight state so
// subsequent calls succeed. Returns ErrStoreStopped if the store has been stopped.
//
// Unless WithCopyOnRead(true) is enabled, returned byte slices alias internal
// cache storage directly; callers must not mutate them (use bytes.Clone if
// modification is required).
func (m *MemoryStore) GetOrLoad(ctx context.Context, key string, ttl time.Duration, loader func(context.Context) ([]byte, error)) ([]byte, error) {
	return m.getOrLoad(ctx, key, ttl, loader, loadOptions{})
}

// GetOrLoadWithOptions behaves like GetOrLoad while applying per-call LoadOptions
// such as WithStaleWhileRevalidate.
func (m *MemoryStore) GetOrLoadWithOptions(ctx context.Context, key string, ttl time.Duration, loader func(context.Context) ([]byte, error), opts ...LoadOption) ([]byte, error) {
	if m.stopped.Load() {
		return nil, ErrStoreStopped
	}
	var cfg loadOptions
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&cfg)
	}
	if cfg.err != nil {
		return nil, cfg.err
	}
	return m.getOrLoad(ctx, key, ttl, loader, cfg)
}

func (m *MemoryStore) getOrLoad(ctx context.Context, key string, ttl time.Duration, loader func(context.Context) ([]byte, error), opts loadOptions) ([]byte, error) {
	if m.stopped.Load() {
		return nil, ErrStoreStopped
	}
	val, entryGrace, status := m.getWithStale(key, m.copyOnRead, opts.hasStaleGrace, opts.staleGrace)
	switch status {
	case hitFresh:
		return val, nil
	case hitStale:
		m.triggerSWRRefresh(key, ttl, loader, opts, entryGrace)
		return val, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var start time.Time
	if m.recorder != nil {
		start = time.Now()
	}

	setOpts := swrSetOptions(opts, 0)
	val, shared, err := m.sf.do(ctx, key, func(callCtx context.Context) ([]byte, error) {
		if m.stopped.Load() {
			return nil, ErrStoreStopped
		}
		// Re-check cache without double-counting miss/hit metrics.
		if existing, ok := m.getRaw(key); ok {
			return existing, nil
		}
		loaded, loadErr := loader(callCtx)
		if loadErr != nil {
			return nil, loadErr
		}
		if setErr := m.setInternal(key, loaded, ttl, setOpts, m.copyOnWrite, true); setErr != nil {
			return nil, setErr
		}
		return loaded, nil
	})

	if m.recorder != nil {
		m.recorder.RecordLoad(time.Since(start), shared, err)
	}
	if err != nil {
		return nil, err
	}
	if m.copyOnRead {
		return bytes.Clone(val), nil
	}
	return val, nil
}

func (m *MemoryStore) triggerSWRRefresh(
	key string,
	ttl time.Duration,
	loader func(context.Context) ([]byte, error),
	opts loadOptions,
	entryGrace int64,
) {
	if loader == nil {
		return
	}
	m.lifecycleMu.RLock()
	if m.stopped.Load() || m.cancelFunc == nil {
		m.lifecycleMu.RUnlock()
		return
	}
	c, started := m.sf.startAsync(key)
	if !started {
		m.lifecycleMu.RUnlock()
		return
	}
	setOpts := swrSetOptions(opts, entryGrace)
	m.wg.Go(func() {
		defer m.sf.finishAsync(key, c)
		refreshCtx, cancel := context.WithTimeout(m.ctx, defaultSWRRefreshTimeout)
		defer cancel()

		var start time.Time
		if m.recorder != nil {
			start = time.Now()
		}

		var loaded []byte
		var loadErr error
		normalReturn := false
		recovered := false

		func() {
			defer func() {
				if r := recover(); r != nil {
					loadErr = fmt.Errorf("swr background refresh panic: %v", r)
					c.err = loadErr
					recovered = true
				} else if !normalReturn {
					c.err = errGoexit
				}
			}()
			loaded, loadErr = loader(refreshCtx)
			normalReturn = true
		}()

		if loadErr != nil {
			c.err = loadErr
			if m.recorder != nil {
				m.recorder.RecordLoad(time.Since(start), false, loadErr)
			}
			if recovered {
				m.logger.Warn("stale-while-revalidate background refresh panicked",
					"key", key, "error", loadErr)
			} else {
				m.logger.Warn("stale-while-revalidate background refresh failed",
					"key", key, "error", loadErr)
			}
			return
		}

		if setErr := m.setInternal(key, loaded, ttl, setOpts, m.copyOnWrite, true); setErr != nil {
			c.err = setErr
			if m.recorder != nil {
				m.recorder.RecordLoad(time.Since(start), false, setErr)
			}
			m.logger.Warn("stale-while-revalidate background store failed",
				"key", key, "error", setErr)
			return
		}

		c.val = loaded
		if m.recorder != nil {
			m.recorder.RecordLoad(time.Since(start), false, nil)
		}
	})
	m.lifecycleMu.RUnlock()
}
