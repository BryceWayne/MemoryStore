package memorystore

import (
	"context"
	"sync"
	"time"
)

// singleflightCall represents an in-flight cache load for a single key.
type singleflightCall struct {
	done chan struct{}
	val  []byte
	err  error
}

// singleflightGroup coalesces concurrent cache-miss loads for identical keys
// without external dependencies.
type singleflightGroup struct {
	mu    sync.Mutex
	calls map[string]*singleflightCall
}

func (g *singleflightGroup) do(ctx context.Context, key string, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*singleflightCall)
	}
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-c.done:
			return c.val, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	c := &singleflightCall{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		close(c.done)
	}()

	c.val, c.err = fn(ctx)
	return c.val, c.err
}

// GetOrLoad returns the cached byte slice for key if present and unexpired.
// On a cache miss, concurrent callers for the same key are coalesced via
// singleflight so loader executes only once; the returned value is stored with
// ttl when loader succeeds.
func (m *MemoryStore) GetOrLoad(ctx context.Context, key string, ttl time.Duration, loader func(context.Context) ([]byte, error)) ([]byte, error) {
	if val, ok := m.Get(key); ok {
		return val, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return m.sf.do(ctx, key, func(callCtx context.Context) ([]byte, error) {
		// Re-check cache in case a previous flight just populated the key.
		if val, ok := m.Get(key); ok {
			return val, nil
		}
		val, err := loader(callCtx)
		if err != nil {
			return nil, err
		}
		if err := m.Set(key, val, ttl); err != nil {
			return nil, err
		}
		return val, nil
	})
}
