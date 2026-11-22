package memorystore

import (
	"context"
	json "encoding/json/v2"
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
	data, exists := m.Get(key)
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
// cache. On a cache miss, concurrent callers for the same key are coalesced via
// singleflight so loader executes only once; the loaded value is marshaled with
// encoding/json/v2 and stored with ttl.
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
	var zero T
	if val, ok, err := m.GetJSONAs[T](key, opts...); ok {
		return val, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}

	raw, err := m.sf.do(ctx, key, func(callCtx context.Context) ([]byte, error) {
		if existing, ok := m.Get(key); ok {
			return existing, nil
		}
		loaded, loadErr := loader(callCtx)
		if loadErr != nil {
			return nil, loadErr
		}
		encoded, marshalErr := json.Marshal(&loaded, opts...)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if setErr := m.Set(key, encoded, ttl); setErr != nil {
			return nil, setErr
		}
		return encoded, nil
	})
	if err != nil {
		return zero, err
	}

	var out T
	if err := json.Unmarshal(raw, &out, opts...); err != nil {
		return zero, err
	}
	return out, nil
}
