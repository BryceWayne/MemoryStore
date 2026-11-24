module github.com/BryceWayne/MemoryStore/benchmarks

go 1.27.0

toolchain go1.27.4

replace github.com/BryceWayne/MemoryStore/v2 => ..

require (
	github.com/BryceWayne/MemoryStore/v2 v2.0.0-00010101000000-000000000000
	github.com/allegro/bigcache/v3 v3.2.0
	github.com/coocood/freecache v1.2.7
	github.com/dgraph-io/ristretto/v2 v2.4.2
	github.com/maypok86/otter/v2 v2.4.0
	github.com/patrickmn/go-cache v2.1.0+incompatible
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/sys v0.50.0 // indirect
)
