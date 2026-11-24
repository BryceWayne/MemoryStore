package memorystore

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"time"
)

// GetJSONAs retrieves and deserializes a JSON value for key into a typed value
// of type T using Go 1.27 generic methods and encoding/json/v2.
//
// Example:
//
//	user, ok, err := store.GetJSONAs[User]("user:123")
func (m *MemoryStore) GetJSONAs[T any](key string, opts ...json.Options) (T, bool, error) {
	var zero T
	data, exists := m.get(key, false, true)
	if !exists {
		return zero, false, nil
	}

	var out T
	if err := json.Unmarshal(data, &out, opts...); err != nil {
		return zero, true, err
	}
	return out, true, nil
}

// GetOrLoadJSON retrieves and deserializes a JSON value of type T from the
// cache. If the entry is within its Stale-While-Revalidate grace window,
// GetOrLoadJSON returns the stale value immediately and coalesces a
// detached-context background refresh. On a cache miss, concurrent callers for
// the same key are coalesced via singleflight so loader executes only once;
// the loaded value is marshaled with encoding/json/v2 and stored with ttl.
//
// If loader panics on a foreground miss, the panic is propagated to the leader
// and all coalesced waiters while cleaning up in-flight singleflight state so
// subsequent calls succeed. Returns ErrStoreStopped if the store has been stopped.
//
// Example:
//
//	user, err := store.GetOrLoadJSON[User](ctx, "user:123", time.Hour, func(ctx context.Context) (User, error) {
//	    return fetchUserFromDB(ctx, "123")
//	})
func (m *MemoryStore) GetOrLoadJSON[T any](
	ctx context.Context,
	key string,
	ttl time.Duration,
	loader func(context.Context) (T, error),
	opts ...json.Options,
) (T, error) {
	return getOrLoadJSON(m, ctx, key, ttl, loader, loadOptions{}, opts...)
}

// GetOrLoadJSONWithOptions behaves like GetOrLoadJSON while applying per-call
// LoadOptions (such as WithStaleWhileRevalidate) alongside encoding/json/v2
// options.
func (m *MemoryStore) GetOrLoadJSONWithOptions[T any](
	ctx context.Context,
	key string,
	ttl time.Duration,
	loader func(context.Context) (T, error),
	loadOpts []LoadOption,
	jsonOpts ...json.Options,
) (T, error) {
	var zero T
	if m.stopped.Load() {
		return zero, ErrStoreStopped
	}
	var cfg loadOptions
	for _, opt := range loadOpts {
		if opt == nil {
			continue
		}
		opt(&cfg)
	}
	if cfg.err != nil {
		return zero, cfg.err
	}
	return getOrLoadJSON(m, ctx, key, ttl, loader, cfg, jsonOpts...)
}

func getOrLoadJSON[T any](
	m *MemoryStore,
	ctx context.Context,
	key string,
	ttl time.Duration,
	loader func(context.Context) (T, error),
	opts loadOptions,
	jsonOpts ...json.Options,
) (T, error) {
	var zero T
	if m.stopped.Load() {
		return zero, ErrStoreStopped
	}

	raw, entryGrace, status := m.getWithStale(key, false, opts.hasStaleGrace, opts.staleGrace)
	switch status {
	case hitFresh:
		var out T
		if err := json.Unmarshal(raw, &out, jsonOpts...); err != nil {
			return zero, err
		}
		return out, nil
	case hitStale:
		triggerSWRRefreshJSON(m, key, ttl, loader, opts, entryGrace, jsonOpts...)
		var out T
		if err := json.Unmarshal(raw, &out, jsonOpts...); err != nil {
			return zero, err
		}
		return out, nil
	}

	if err := ctx.Err(); err != nil {
		return zero, err
	}

	var start time.Time
	if m.recorder != nil {
		start = time.Now()
	}

	var leaderVal T
	var isLeader bool
	setOpts := swrSetOptions(opts, 0)

	loadedRaw, shared, err := m.sf.do(ctx, key, func(callCtx context.Context) ([]byte, error) {
		if m.stopped.Load() {
			return nil, ErrStoreStopped
		}
		if existing, ok := m.getRaw(key); ok {
			return existing, nil
		}
		loaded, loadErr := loader(callCtx)
		if loadErr != nil {
			return nil, loadErr
		}
		encoded, marshalErr := json.Marshal(&loaded, jsonOpts...)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if setErr := m.setInternal(key, encoded, ttl, setOpts, false, true); setErr != nil {
			return nil, setErr
		}
		leaderVal = loaded
		isLeader = true
		return encoded, nil
	})
	if err != nil {
		if m.recorder != nil {
			m.recorder.RecordLoad(time.Since(start), shared, err)
		}
		return zero, err
	}
	if isLeader {
		if m.recorder != nil {
			m.recorder.RecordLoad(time.Since(start), false, nil)
		}
		return leaderVal, nil
	}

	var out T
	unmarshalErr := json.Unmarshal(loadedRaw, &out, jsonOpts...)
	if m.recorder != nil {
		m.recorder.RecordLoad(time.Since(start), shared, unmarshalErr)
	}
	if unmarshalErr != nil {
		return zero, unmarshalErr
	}
	return out, nil
}

func triggerSWRRefreshJSON[T any](
	m *MemoryStore,
	key string,
	ttl time.Duration,
	loader func(context.Context) (T, error),
	opts loadOptions,
	entryGrace int64,
	jsonOpts ...json.Options,
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

		var loaded T
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

		encoded, marshalErr := json.Marshal(&loaded, jsonOpts...)
		if marshalErr != nil {
			c.err = marshalErr
			if m.recorder != nil {
				m.recorder.RecordLoad(time.Since(start), false, marshalErr)
			}
			m.logger.Warn("stale-while-revalidate background marshal failed",
				"key", key, "error", marshalErr)
			return
		}

		if setErr := m.setInternal(key, encoded, ttl, setOpts, false, true); setErr != nil {
			c.err = setErr
			if m.recorder != nil {
				m.recorder.RecordLoad(time.Since(start), false, setErr)
			}
			m.logger.Warn("stale-while-revalidate background store failed",
				"key", key, "error", setErr)
			return
		}

		c.val = encoded
		if m.recorder != nil {
			m.recorder.RecordLoad(time.Since(start), false, nil)
		}
	})
	m.lifecycleMu.RUnlock()
}
