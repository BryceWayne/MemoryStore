# MemoryStore Head-to-Head Benchmarks

This isolated Go sub-module benchmarks **MemoryStore** against popular Go in-memory cache libraries without adding any third-party dependencies to the core `MemoryStore` module:

- `github.com/BryceWayne/MemoryStore/memorystore`
- `github.com/patrickmn/go-cache`
- `github.com/allegro/bigcache/v3`
- `github.com/coocood/freecache`
- `github.com/maypok86/otter/v2`
- `github.com/dgraph-io/ristretto/v2`

## Running

```bash
cd benchmarks
go test -run='^$' -bench=. -benchmem -benchtime=500ms -count=5 .
```
