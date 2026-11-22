package memorystore

import (
	"bytes"
	jsonv1 "encoding/json"
	json "encoding/json/v2"
	"fmt"
	"math"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

func TestMemoryStore_JSONv2Defaults(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	// In encoding/json/v2, nil slices marshal as [] and nil maps marshal as {}.
	type payload struct {
		Items []string          `json:"items"`
		Meta  map[string]string `json:"meta"`
		When  time.Time         `json:"when,omitzero"`
	}

	if err := ms.SetJSON("v2:defaults", payload{}, time.Minute); err != nil {
		t.Fatalf("SetJSON() error = %v", err)
	}

	raw, ok := ms.Get("v2:defaults")
	if !ok {
		t.Fatal("expected stored key to exist")
	}

	want := `{"items":[],"meta":{}}`
	if string(raw) != want {
		t.Errorf("SetJSON() raw = %s, want %s", string(raw), want)
	}

	var decoded payload
	exists, err := ms.GetJSON("v2:defaults", &decoded)
	if err != nil {
		t.Fatalf("GetJSON() error = %v", err)
	}
	if !exists {
		t.Fatal("GetJSON() expected exists = true")
	}
	if decoded.Items == nil || len(decoded.Items) != 0 {
		t.Errorf("expected empty non-nil slice, got %#v", decoded.Items)
	}
	if decoded.Meta == nil || len(decoded.Meta) != 0 {
		t.Errorf("expected empty non-nil map, got %#v", decoded.Meta)
	}
	if !decoded.When.IsZero() {
		t.Errorf("expected zero time, got %v", decoded.When)
	}
}

func TestMemoryStore_JSONv2StrictValidation(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	type sample struct {
		Name string `json:"name"`
	}

	// Duplicate object member names are rejected by encoding/json/v2 by default.
	if err := ms.Set("dup-key", []byte(`{"name":"first","name":"second"}`), time.Minute); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	var out sample
	exists, err := ms.GetJSON("dup-key", &out)
	if !exists {
		t.Fatal("expected key to exist")
	}
	if err == nil {
		t.Error("GetJSON() expected error for duplicate JSON object keys in v2")
	}

	// Invalid UTF-8 in JSON strings is rejected by encoding/json/v2 by default.
	if err := ms.Set("bad-utf8", []byte("{\"name\":\"\xff\"}"), time.Minute); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	exists, err = ms.GetJSON("bad-utf8", &out)
	if !exists {
		t.Fatal("expected key to exist")
	}
	if err == nil {
		t.Error("GetJSON() expected error for invalid UTF-8 in v2")
	}
}

func TestMemoryStore_JSONWithOptions(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	// Deterministic map marshaling
	input := map[string]int{"z": 1, "a": 2, "m": 3}
	if err := ms.SetJSONWithOptions("det", input, time.Minute, json.Deterministic(true)); err != nil {
		t.Fatalf("SetJSONWithOptions() error = %v", err)
	}
	raw, ok := ms.Get("det")
	if !ok {
		t.Fatal("expected key to exist")
	}
	if string(raw) != `{"a":2,"m":3,"z":1}` {
		t.Errorf("deterministic JSON = %s, want %s", string(raw), `{"a":2,"m":3,"z":1}`)
	}

	// RejectUnknownMembers on unmarshal
	type strictStruct struct {
		A int `json:"a"`
	}
	var strict strictStruct
	exists, err := ms.GetJSONWithOptions("det", &strict, json.RejectUnknownMembers(true))
	if !exists {
		t.Fatal("expected key to exist")
	}
	if err == nil {
		t.Error("expected error when unknown members are rejected")
	}

	// Non-existent key returns (false, nil)
	exists, err = ms.GetJSONWithOptions("missing", &strict, json.RejectUnknownMembers(true))
	if exists || err != nil {
		t.Errorf("GetJSONWithOptions(missing) = (%v, %v), want (false, nil)", exists, err)
	}

	// Unmarshal error for unserializable value in SetJSONWithOptions
	if err := ms.SetJSONWithOptions("bad", make(chan int), time.Minute); err == nil {
		t.Error("expected error for channel value")
	}

	// Legacy v1 compatibility via encoding/json.DefaultOptionsV1()
	type v1Struct struct {
		Items []string `json:"items"`
	}
	if err := ms.SetJSONWithOptions("v1compat", v1Struct{}, time.Minute, jsonv1.DefaultOptionsV1()); err != nil {
		t.Fatalf("SetJSONWithOptions(v1) error = %v", err)
	}
	rawV1, ok := ms.Get("v1compat")
	if !ok {
		t.Fatal("expected v1compat key to exist")
	}
	if string(rawV1) != `{"items":null}` {
		t.Errorf("v1 compat JSON = %s, want %s", string(rawV1), `{"items":null}`)
	}
	var gotV1 v1Struct
	exists, err = ms.GetJSONWithOptions("v1compat", &gotV1, jsonv1.DefaultOptionsV1())
	if err != nil || !exists {
		t.Fatalf("GetJSONWithOptions(v1) = (%v, %v), want (true, nil)", exists, err)
	}
	if gotV1.Items != nil {
		t.Errorf("expected nil slice with v1 semantics, got %#v", gotV1.Items)
	}
}

func TestMemoryStore_BatchEdgeCases(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	// Empty SetMulti and GetMulti
	if err := ms.SetMulti(nil, time.Minute); err != nil {
		t.Fatalf("SetMulti(nil) error = %v", err)
	}
	if res := ms.GetMulti(nil); res == nil || len(res) != 0 {
		t.Errorf("GetMulti(nil) = %#v, want non-nil empty map", res)
	}

	// Large batch exceeding numShards (256) so multiple keys collide in the same shard
	const count = 600
	items := make(map[string][]byte, count)
	keys := make([]string, 0, count+2)
	for i := range count {
		k := fmt.Sprintf("shard-collision-key-%d", i)
		items[k] = []byte(fmt.Sprintf("val-%d", i))
		keys = append(keys, k)
	}
	if err := ms.SetMulti(items, time.Minute); err != nil {
		t.Fatalf("SetMulti(large) error = %v", err)
	}

	// Include expired key and missing key
	if err := ms.Set("expired-batch-key", []byte("gone"), 0); err != nil {
		t.Fatalf("Set(expired) error = %v", err)
	}
	keys = append(keys, "expired-batch-key", "does-not-exist")

	got := ms.GetMulti(keys)
	if len(got) != count {
		t.Fatalf("GetMulti() returned %d items, want %d", len(got), count)
	}
	for k, want := range items {
		if !bytes.Equal(got[k], want) {
			t.Errorf("GetMulti()[%q] = %q, want %q", k, got[k], want)
		}
	}
}

func TestMemoryStore_ExpirationOverflowAndNegative(t *testing.T) {
	ms := NewMemoryStore()
	defer func() {
		_ = ms.Stop()
	}()

	if err := ms.Set("neg", []byte("v"), -time.Second); err != nil {
		t.Fatalf("Set(neg) error = %v", err)
	}
	if _, ok := ms.Get("neg"); ok {
		t.Error("expected negative duration item to be immediately expired")
	}

	// Max duration should saturate rather than overflow to a negative timestamp.
	if err := ms.Set("max", []byte("forever"), time.Duration(math.MaxInt64)); err != nil {
		t.Fatalf("Set(max) error = %v", err)
	}
	if v, ok := ms.Get("max"); !ok || string(v) != "forever" {
		t.Errorf("Get(max) = (%q, %v), want (%q, true)", string(v), ok, "forever")
	}
}

func TestMatchesTopic_EdgeCases(t *testing.T) {
	tests := []struct {
		topic   string
		pattern string
		want    bool
	}{
		{"", "", true},
		{"", "*", true},
		{"a:", "a:*", true},
		{":b", "*:b", true},
		{"a::c", "a:*:c", true},
		{"a:b:", "a:b", false},
		{"a:b", "a:b:", false},
		{":", "*:*", true},
		{":", "*", false},
	}

	for _, tt := range tests {
		if got := matchesTopic(tt.topic, tt.pattern); got != tt.want {
			t.Errorf("matchesTopic(%q, %q) = %v, want %v", tt.topic, tt.pattern, got, tt.want)
		}
	}
}

func TestMemoryStore_NoGoroutineLeaks(t *testing.T) {
	prof := pprof.Lookup("goroutineleak")
	if prof == nil {
		t.Skip("goroutineleak profile not available")
	}

	ms := NewMemoryStore()
	ch, err := ms.Subscribe("leak:test")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if err := ms.Publish("leak:test", []byte("hello")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	<-ch
	if err := ms.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	var buf bytes.Buffer
	if err := prof.WriteTo(&buf, 1); err != nil {
		t.Fatalf("goroutineleak WriteTo() error = %v", err)
	}
	if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/memorystore") {
		t.Errorf("detected leaked goroutine in memorystore:\n%s", buf.String())
	}
}
