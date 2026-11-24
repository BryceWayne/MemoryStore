package memorystore

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func matchesTopicSplit(topic, pattern string) bool {
	if topic == pattern {
		return true
	}
	topicParts := strings.Split(topic, ":")
	patternParts := strings.Split(pattern, ":")
	if len(topicParts) != len(patternParts) {
		return false
	}
	for i := range patternParts {
		if patternParts[i] != "*" && patternParts[i] != topicParts[i] {
			return false
		}
	}
	return true
}

func FuzzMatchesTopic(f *testing.F) {
	seeds := [][2]string{
		{"user:123", "user:123"},
		{"user:123", "user:*"},
		{"user:123:profile", "user:*:profile"},
		{"user:123:profile", "user:123:*"},
		{"user:123", "post:123"},
		{"user:123", "user:123:profile"},
		{"user:123:profile", "user:123"},
		{"user:123", "*"},
		{"a", "*"},
		{"a:b", "*:*"},
		{"a:b", "a:*"},
		{"a:b", "*:b"},
		{"a:b:c", "a:*:c"},
		{"", ""},
		{"", "*"},
		{"a:", "a:*"},
		{":b", "*:b"},
		{"a::c", "a:*:c"},
		{"a:b:", "a:b"},
		{"a:b", "a:b:"},
		{":", "*:*"},
		{":", "*"},
		{"**:*", "**:*"},
		{"a:*:b:*", "a:*:*:*"},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1])
	}

	f.Fuzz(func(t *testing.T, topic, pattern string) {
		got := matchesTopic(topic, pattern)
		want := matchesTopicSplit(topic, pattern)
		if got != want {
			t.Fatalf("matchesTopic(%q, %q) = %v, want %v", topic, pattern, got, want)
		}
		if !matchesTopic(topic, topic) {
			t.Fatalf("matchesTopic(%q, %q) = false, want reflexive true", topic, topic)
		}
	})
}

func FuzzInvalidationUnmarshal(f *testing.F) {
	ms, err := New()
	if err != nil {
		f.Fatalf("New() error = %v", err)
	}
	defer func() {
		_ = ms.Stop()
	}()

	f.Add([]byte(`{"o":"other-node","k":["k1","k2"]}`))
	f.Add([]byte(`{"o":"other-node","m":1,"e":[{"k":"wt1","v":"d2FybQ==","t":60000000000,"s":true,"c":60000000000,"g":5000000}]}`))
	f.Add(fmt.Appendf(nil, `{"o":%q,"k":["sentinel"]}`, ms.InstanceID()))
	f.Add([]byte(`{"o":"","k":[]}`))
	f.Add([]byte(`{"o":"x","k":null}`))
	f.Add([]byte(`{malformed`))
	f.Add([]byte(`{"o":123,"k":"bad"}`))
	f.Add([]byte{0xff, 0xfe, 0x00})
	f.Add([]byte{})
	f.Add([]byte(nil))

	f.Fuzz(func(t *testing.T, payload []byte) {
		var evt invalidationEvent
		isSelfEcho := json.Unmarshal(payload, &evt) == nil && evt.Origin == ms.InstanceID()
		if isSelfEcho {
			if err := ms.Set("sentinel", []byte("keep"), time.Minute); err != nil {
				t.Fatalf("Set(sentinel) error = %v", err)
			}
		}

		ms.handleInvalidationMessage(payload)

		if isSelfEcho {
			if v, ok := ms.Get("sentinel"); !ok || string(v) != "keep" {
				t.Fatalf("self-echo invalidation deleted sentinel key: got (%q, %v)", string(v), ok)
			}
		}
	})
}

func FuzzSnapshotLoad(f *testing.F) {
	seedStore, err := New(WithPreciseClock())
	if err != nil {
		f.Fatalf("New(seedStore) error = %v", err)
	}
	_ = seedStore.Set("k1", []byte("val1"), time.Minute)
	_ = seedStore.Set("perm", []byte("forever"), NoExpiration)
	_ = seedStore.SetWithOptions("swr", []byte("s"), time.Minute, WithSlidingExpiration(), WithStaleWindow(time.Second))

	var binBuf, jsonBuf bytes.Buffer
	if err := seedStore.Save(&binBuf, WithSnapshotFormat(SnapshotBinary)); err != nil {
		f.Fatalf("Save(binary) error = %v", err)
	}
	if err := seedStore.Save(&jsonBuf, WithSnapshotFormat(SnapshotJSON)); err != nil {
		f.Fatalf("Save(json) error = %v", err)
	}
	_ = seedStore.Stop()

	validBin := binBuf.Bytes()
	validJSON := jsonBuf.Bytes()

	f.Add(uint8(0), validBin)
	f.Add(uint8(0), validBin[:len(validBin)/2])
	badVer := bytes.Clone(validBin)
	badVer[6] = 9
	f.Add(uint8(0), badVer)
	badCRC := bytes.Clone(validBin)
	badCRC[len(badCRC)-1] ^= 0xff
	f.Add(uint8(0), badCRC)
	f.Add(uint8(0), []byte("MSNAP\x02\x01\x01"))
	f.Add(uint8(0), []byte{0x00, 0x01, 0x02})

	f.Add(uint8(1), validJSON)
	f.Add(uint8(1), validJSON[:len(validJSON)/2])
	f.Add(uint8(1), []byte(`{"type":"header","magic":"MSNAP","version":1}`))
	f.Add(uint8(1), []byte(`{"type":"header","magic":"MSNAP","version":2}`))
	f.Add(uint8(1), []byte("{\"type\":\"header\",\"magic\":\"MSNAP\",\"version\":1}\n{\"type\":\"entry\",\"key\":\"k\",\"value\":\"***\",\"ttl_ns\":100}\n"))
	f.Add(uint8(1), []byte(`{malformed`))

	ms, err := New(WithMaxItems(64), WithPreciseClock())
	if err != nil {
		f.Fatalf("New(fuzzStore) error = %v", err)
	}
	defer func() {
		_ = ms.Stop()
	}()

	f.Fuzz(func(t *testing.T, formatSel uint8, data []byte) {
		format := SnapshotFormat(formatSel & 1)
		if err := ms.Load(bytes.NewReader(data), WithSnapshotFormat(format)); err == nil {
			if serr := ms.Save(io.Discard, WithSnapshotFormat(format)); serr != nil {
				t.Fatalf("Save after valid Load(%s) failed: %v", format, serr)
			}
		}
	})
}
