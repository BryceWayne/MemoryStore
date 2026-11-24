package kafka

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

func generateTestTLSConfigs(t *testing.T) (serverTLS, clientTLS *tls.Config) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey() error = %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate() error = %v", err)
	}

	tlsCert := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("x509.ParseCertificate() error = %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)

	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	}, &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}
}

func assertNoGoroutineLeaks(t *testing.T) {
	t.Helper()
	if prof := pprof.Lookup("goroutineleak"); prof != nil {
		var buf bytes.Buffer
		if err := prof.WriteTo(&buf, 1); err != nil {
			t.Fatalf("goroutineleak WriteTo() error = %v", err)
		}
		if strings.Contains(buf.String(), "github.com/BryceWayne/MemoryStore/pubsub/kafka") ||
			strings.Contains(buf.String(), "github.com/twmb/franz-go") {
			t.Errorf("detected leaked goroutine in pubsub/kafka or franz-go:\n%s", buf.String())
		}
	}
}

func TestConstructorValidation(t *testing.T) {
	if _, err := NewClient(nil); !errors.Is(err, ErrInvalidBrokers) {
		t.Errorf("NewClient(nil) = %v, want ErrInvalidBrokers", err)
	}
	if _, err := NewClient([]string{}); !errors.Is(err, ErrInvalidBrokers) {
		t.Errorf("NewClient(empty) = %v, want ErrInvalidBrokers", err)
	}
	if _, err := NewClient([]string{"localhost:9092", "   "}); !errors.Is(err, ErrInvalidBrokers) {
		t.Errorf("NewClient(blank broker) = %v, want ErrInvalidBrokers", err)
	}
	if _, err := NewClient([]string{"localhost:9092"}, WithKafkaOptions(kgo.TransactionalID(""))); err == nil {
		t.Error("NewClient(invalid kgo.Opt) expected error, got nil")
	}
}

func TestClient_BroadcastLifecycleAndAtEnd(t *testing.T) {
	ctx := t.Context()
	cluster := kfake.MustCluster(
		kfake.NumBrokers(3),
		kfake.SeedTopics(4, "cache-inv", "other-topic"),
	)
	defer cluster.Close()

	// Produce a pre-existing record before any subscriber attaches.
	seedClient, err := NewClient(cluster.ListenAddrs())
	if err != nil {
		t.Fatalf("NewClient(seed) error = %v", err)
	}
	defer func() { _ = seedClient.Close() }()

	if err := seedClient.Publish(ctx, "cache-inv", []byte("stale-history")); err != nil {
		t.Fatalf("seed Publish() error = %v", err)
	}

	sub1, err := NewClient(cluster.ListenAddrs(), WithChannelBuffer(32))
	if err != nil {
		t.Fatalf("NewClient(sub1) error = %v", err)
	}
	defer func() { _ = sub1.Close() }()

	sub2, err := NewClient(cluster.ListenAddrs())
	if err != nil {
		t.Fatalf("NewClient(sub2) error = %v", err)
	}
	defer func() { _ = sub2.Close() }()

	// Verify Subscribe's ctx is used for setup only (canceling immediately must
	// not terminate the background receive loop, matching memorystore).
	subCtx1, cancelSub1 := context.WithTimeout(ctx, 10*time.Second)
	ch1, err := sub1.Subscribe(subCtx1, "cache-inv")
	cancelSub1()
	if err != nil {
		t.Fatalf("sub1.Subscribe() error = %v", err)
	}

	ch2, err := sub2.Subscribe(ctx, "cache-inv")
	if err != nil {
		t.Fatalf("sub2.Subscribe() error = %v", err)
	}

	chOther, err := sub1.Subscribe(ctx, "other-topic")
	if err != nil {
		t.Fatalf("sub1.Subscribe(other-topic) error = %v", err)
	}

	// Publish across multiple partitions right after Subscribe returns.
	const total = 16
	wantPayloads := make(map[string]bool, total)
	for i := range total {
		msg := fmt.Sprintf("invalidate-%02d", i)
		wantPayloads[msg] = true
		if err := seedClient.Publish(ctx, "cache-inv", []byte(msg)); err != nil {
			t.Fatalf("Publish(%d) error = %v", i, err)
		}
	}

	collect := func(name string, ch <-chan []byte) {
		got := make(map[string]bool, total)
		deadline := time.After(5 * time.Second)
		for len(got) < total {
			select {
			case msg, ok := <-ch:
				if !ok {
					t.Fatalf("%s channel closed early after %d/%d messages", name, len(got), total)
				}
				s := string(msg)
				if s == "stale-history" {
					t.Fatalf("%s replayed pre-Subscribe record %q", name, s)
				}
				got[s] = true
			case <-deadline:
				t.Fatalf("%s timed out after %d/%d messages", name, len(got), total)
			}
		}
		for k := range wantPayloads {
			if !got[k] {
				t.Errorf("%s missing payload %q", name, k)
			}
		}
	}

	collect("sub1", ch1)
	collect("sub2", ch2)

	// Unsubscribe closes ch1 while leaving sub1's other-topic intact.
	if err := sub1.Unsubscribe(ctx, "cache-inv"); err != nil {
		t.Fatalf("Unsubscribe() error = %v", err)
	}
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("expected ch1 to be closed after Unsubscribe")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ch1 to close")
	}

	// Unsubscribe on unknown topic is idempotent and returns nil.
	if err := sub1.Unsubscribe(ctx, "does-not-exist"); err != nil {
		t.Errorf("Unsubscribe(unknown) = %v, want nil", err)
	}

	// other-topic still receives messages.
	if err := seedClient.Publish(ctx, "other-topic", []byte("still-alive")); err != nil {
		t.Fatalf("Publish(other-topic) error = %v", err)
	}
	select {
	case got := <-chOther:
		if string(got) != "still-alive" {
			t.Errorf("other-topic got %q, want still-alive", string(got))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for other-topic message")
	}

	// Close is idempotent and closes remaining channels.
	if err := sub1.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := sub1.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	select {
	case _, ok := <-chOther:
		if ok {
			t.Error("expected chOther to be closed after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for chOther to close")
	}
}

func TestClient_ConsumerGroupFanOut(t *testing.T) {
	ctx := t.Context()
	cluster := kfake.MustCluster(
		kfake.NumBrokers(3),
		kfake.SeedTopics(4, "work-topic"),
		kfake.BrokerConfigs(map[string]string{
			"group.consumer.heartbeat.interval.ms": "100",
		}),
	)
	defer cluster.Close()

	pub, err := NewClient(cluster.ListenAddrs())
	if err != nil {
		t.Fatalf("NewClient(pub) error = %v", err)
	}
	defer func() { _ = pub.Close() }()

	c1, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup("workers"))
	if err != nil {
		t.Fatalf("NewClient(c1) error = %v", err)
	}
	defer func() { _ = c1.Close() }()

	c2, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup("workers"))
	if err != nil {
		t.Fatalf("NewClient(c2) error = %v", err)
	}
	defer func() { _ = c2.Close() }()

	ch1, err := c1.Subscribe(ctx, "work-topic")
	if err != nil {
		t.Fatalf("c1.Subscribe() error = %v", err)
	}
	ch2, err := c2.Subscribe(ctx, "work-topic")
	if err != nil {
		t.Fatalf("c2.Subscribe() error = %v", err)
	}

	// Also verify standby 0-partition group member does not block Subscribe.
	standbyCluster := kfake.MustCluster(
		kfake.NumBrokers(1),
		kfake.SeedTopics(1, "single-part"),
	)
	defer standbyCluster.Close()

	g1, err := NewClient(standbyCluster.ListenAddrs(), WithConsumerGroup("standby-group"))
	if err != nil {
		t.Fatalf("NewClient(g1) error = %v", err)
	}
	defer func() { _ = g1.Close() }()
	g2, err := NewClient(standbyCluster.ListenAddrs(), WithConsumerGroup("standby-group"))
	if err != nil {
		t.Fatalf("NewClient(g2) error = %v", err)
	}
	defer func() { _ = g2.Close() }()

	if _, err := g1.Subscribe(ctx, "single-part"); err != nil {
		t.Fatalf("g1.Subscribe() error = %v", err)
	}
	standbyCtx, cancelStandby := context.WithTimeout(ctx, 5*time.Second)
	defer cancelStandby()
	standbyCh, err := g2.Subscribe(standbyCtx, "single-part")
	if err != nil {
		t.Fatalf("g2.Subscribe(standby) error = %v", err)
	}

	// Verify standby member promotes and receives messages once g1 unsubscribes.
	if err := g1.Unsubscribe(ctx, "single-part"); err != nil {
		t.Fatalf("g1.Unsubscribe() error = %v", err)
	}
	standbyPub, err := NewClient(standbyCluster.ListenAddrs())
	if err != nil {
		t.Fatalf("NewClient(standbyPub) error = %v", err)
	}
	defer func() { _ = standbyPub.Close() }()

	promoteDeadline := time.Now().Add(5 * time.Second)
	promoted := false
	for !promoted && time.Now().Before(promoteDeadline) {
		if err := standbyPub.Publish(ctx, "single-part", []byte("promoted")); err != nil {
			t.Fatalf("standbyPub.Publish() error = %v", err)
		}
		select {
		case msg := <-standbyCh:
			if string(msg) == "promoted" {
				promoted = true
			}
		case <-time.After(150 * time.Millisecond):
		}
	}
	if !promoted {
		t.Fatal("standby member g2 did not promote after g1.Unsubscribe")
	}

	const total = 24
	for i := range total {
		if err := pub.Publish(ctx, "work-topic", []byte(fmt.Sprintf("job-%02d", i))); err != nil {
			t.Fatalf("Publish(%d) error = %v", i, err)
		}
	}

	received := make(map[string]int, total)
	deadline := time.After(8 * time.Second)
	for len(received) < total {
		select {
		case msg := <-ch1:
			received[string(msg)]++
		case msg := <-ch2:
			received[string(msg)]++
		case <-deadline:
			t.Fatalf("timed out waiting for consumer group messages, got %d/%d", len(received), total)
		}
	}

	for i := range total {
		key := fmt.Sprintf("job-%02d", i)
		if count := received[key]; count != 1 {
			t.Errorf("message %q delivered %d times across group, want 1", key, count)
		}
	}

	if err := c1.Unsubscribe(ctx, "work-topic"); err != nil {
		t.Fatalf("c1.Unsubscribe() error = %v", err)
	}
	if err := c2.Unsubscribe(ctx, "work-topic"); err != nil {
		t.Fatalf("c2.Unsubscribe() error = %v", err)
	}
}

func TestClient_TLSAndSASLAuth(t *testing.T) {
	ctx := t.Context()
	serverTLS, clientTLS := generateTestTLSConfigs(t)

	cluster := kfake.MustCluster(
		kfake.NumBrokers(1),
		kfake.TLS(serverTLS),
		kfake.EnableSASL(),
		kfake.Superuser("SCRAM-SHA-256", "alice", "alice-secret"),
		kfake.Superuser("SCRAM-SHA-512", "bob", "bob-secret"),
		kfake.AllowAutoTopicCreation(),
	)
	defer cluster.Close()

	// SCRAM-SHA-256 over TLS.
	c256, err := NewClient(
		cluster.ListenAddrs(),
		WithTLSConfig(clientTLS),
		WithSCRAMSHA256("alice", "alice-secret"),
	)
	if err != nil {
		t.Fatalf("NewClient(SCRAM-SHA-256) error = %v", err)
	}
	defer func() { _ = c256.Close() }()

	ch256, err := c256.Subscribe(ctx, "secure-topic")
	if err != nil {
		t.Fatalf("c256.Subscribe() error = %v", err)
	}

	// SCRAM-SHA-512 over TLS.
	c512, err := NewClient(
		cluster.ListenAddrs(),
		WithTLSConfig(clientTLS),
		WithSCRAMSHA512("bob", "bob-secret"),
	)
	if err != nil {
		t.Fatalf("NewClient(SCRAM-SHA-512) error = %v", err)
	}
	defer func() { _ = c512.Close() }()

	if err := c512.Publish(ctx, "secure-topic", []byte("tls-scram-ok")); err != nil {
		t.Fatalf("c512.Publish() error = %v", err)
	}

	select {
	case msg := <-ch256:
		if string(msg) != "tls-scram-ok" {
			t.Errorf("got %q, want tls-scram-ok", string(msg))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for TLS/SCRAM message")
	}

	// Explicit WithSASL mechanism.
	cSASL, err := NewClient(
		cluster.ListenAddrs(),
		WithTLSConfig(clientTLS),
		WithSASL(nil, scram.Auth{User: "alice", Pass: "alice-secret"}.AsSha256Mechanism()),
	)
	if err != nil {
		t.Fatalf("NewClient(WithSASL) error = %v", err)
	}
	defer func() { _ = cSASL.Close() }()

	if err := cSASL.Publish(ctx, "secure-topic", []byte("via-with-sasl")); err != nil {
		t.Fatalf("cSASL.Publish() error = %v", err)
	}
	select {
	case msg := <-ch256:
		if string(msg) != "via-with-sasl" {
			t.Errorf("got %q, want via-with-sasl", string(msg))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WithSASL message")
	}

	// Invalid credentials must fail.
	badClient, err := NewClient(
		cluster.ListenAddrs(),
		WithTLSConfig(clientTLS),
		WithSCRAMSHA256("alice", "wrong-password"),
	)
	if err != nil {
		t.Fatalf("NewClient(bad creds) error = %v", err)
	}
	defer func() { _ = badClient.Close() }()

	authCtx, cancelAuth := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancelAuth()
	if _, err := badClient.Subscribe(authCtx, "secure-topic"); err == nil {
		t.Error("expected Subscribe with invalid SCRAM credentials to fail")
	}
}

func TestClient_OptionsAndEdgeCases(t *testing.T) {
	ctx := t.Context()
	cluster := kfake.MustCluster(
		kfake.NumBrokers(1),
		kfake.AllowAutoTopicCreation(),
	)
	defer cluster.Close()

	// WithAutoCreateTopics(false) rejects non-existent topics.
	noAuto, err := NewClient(cluster.ListenAddrs(), WithAutoCreateTopics(false))
	if err != nil {
		t.Fatalf("NewClient(noAuto) error = %v", err)
	}
	defer func() { _ = noAuto.Close() }()

	if _, err := noAuto.Subscribe(ctx, "missing-topic"); !errors.Is(err, kerr.UnknownTopicOrPartition) {
		t.Errorf("Subscribe(missing with autoCreate=false) = %v, want kerr.UnknownTopicOrPartition", err)
	}
	if err := noAuto.Publish(ctx, "missing-topic", []byte("x")); !errors.Is(err, kerr.UnknownTopicOrPartition) {
		t.Errorf("Publish(missing with autoCreate=false) = %v, want kerr.UnknownTopicOrPartition", err)
	}

	// WithKafkaOptions partitions ProducerOpt, ConsumerOpt, GroupOpt cleanly
	// in both broadcast mode and consumer group mode without kgo validation failure.
	mixedOpts := WithKafkaOptions(
		nil,
		kgo.RequiredAcks(kgo.AllISRAcks()), // ProducerOpt
		kgo.FetchMaxBytes(1<<20),           // ConsumerOpt
		kgo.DisableAutoCommit(),            // GroupOpt (filtered out in broadcast/producer)
		kgo.ClientID("ms-kafka-test"),      // Shared Opt
	)

	c, err := NewClient(
		cluster.ListenAddrs(),
		WithChannelBuffer(0), // ignored, keeps default 100
		WithChannelBuffer(4),
		mixedOpts,
	)
	if err != nil {
		t.Fatalf("NewClient(mixedOpts) error = %v", err)
	}

	// Also verify WithKafkaOptions in consumer group mode (GroupOpt + ProducerOpt + ConsumerOpt).
	cgMixed, err := NewClient(
		cluster.ListenAddrs(),
		WithConsumerGroup("mixed-cg"),
		mixedOpts,
	)
	if err != nil {
		t.Fatalf("NewClient(cgMixed) error = %v", err)
	}
	defer func() { _ = cgMixed.Close() }()
	if _, err := cgMixed.Subscribe(ctx, "mixed-cg-topic"); err != nil {
		t.Fatalf("cgMixed.Subscribe() error = %v", err)
	}

	// Unreachable broker returns error bounded by ctx without leaking.
	unreachClient, err := NewClient([]string{"127.0.0.1:1"}, WithConsumerGroup("unreach-cg"))
	if err != nil {
		t.Fatalf("NewClient(unreachable) error = %v", err)
	}
	defer func() { _ = unreachClient.Close() }()
	unreachCtx, cancelUnreach := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelUnreach()
	if _, err := unreachClient.Subscribe(unreachCtx, "t"); err == nil {
		t.Error("expected Subscribe on unreachable broker to fail")
	}

	if c.cfg.channelBuffer != 4 {
		t.Errorf("channelBuffer = %d, want 4", c.cfg.channelBuffer)
	}

	// Empty topic -> ErrInvalidTopic.
	if _, err := c.Subscribe(ctx, ""); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Subscribe(empty) = %v, want ErrInvalidTopic", err)
	}
	if err := c.Publish(ctx, "", []byte("x")); !errors.Is(err, ErrInvalidTopic) {
		t.Errorf("Publish(empty) = %v, want ErrInvalidTopic", err)
	}

	// Canceled ctx rejected upfront.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Subscribe(canceledCtx, "t"); !errors.Is(err, context.Canceled) {
		t.Errorf("Subscribe(canceled) = %v, want context.Canceled", err)
	}
	if err := c.Publish(canceledCtx, "t", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Publish(canceled) = %v, want context.Canceled", err)
	}
	if err := c.Unsubscribe(canceledCtx, "t"); !errors.Is(err, context.Canceled) {
		t.Errorf("Unsubscribe(canceled) = %v, want context.Canceled", err)
	}

	// Auto-created topic works end-to-end.
	ch1, err := c.Subscribe(ctx, "auto-topic")
	if err != nil {
		t.Fatalf("Subscribe(auto-topic) error = %v", err)
	}

	// Duplicate Subscribe replaces prior subscription cleanly and closes ch1.
	ch2, err := c.Subscribe(ctx, "auto-topic")
	if err != nil {
		t.Fatalf("duplicate Subscribe(auto-topic) error = %v", err)
	}
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("expected ch1 to close when replaced by duplicate Subscribe")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replaced ch1 to close")
	}

	// Concurrent duplicate Subscribe stress check.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			_, _ = c.Subscribe(ctx, "concurrent-topic")
		})
	}
	wg.Wait()
	_ = ch2

	// Slow consumer with small buffer (1) must not deadlock Close/Unsubscribe.
	slowClient, err := NewClient(cluster.ListenAddrs(), WithChannelBuffer(1))
	if err != nil {
		t.Fatalf("NewClient(slow) error = %v", err)
	}
	slowCh, err := slowClient.Subscribe(ctx, "slow-topic")
	if err != nil {
		t.Fatalf("slowClient.Subscribe() error = %v", err)
	}
	for i := range 20 {
		if err := slowClient.Publish(ctx, "slow-topic", []byte{byte(i)}); err != nil {
			t.Fatalf("slow Publish(%d) error = %v", i, err)
		}
	}
	// Close without draining slowCh must return promptly without deadlocking.
	doneClose := make(chan error, 1)
	go func() {
		doneClose <- slowClient.Close()
	}()
	select {
	case err := <-doneClose:
		if err != nil {
			t.Fatalf("slowClient.Close() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slowClient.Close() deadlocked on full subscriber channel")
	}
	_ = slowCh

	// Post-Close operations return ErrClosed.
	if err := c.Close(); err != nil {
		t.Fatalf("c.Close() error = %v", err)
	}
	if _, err := c.Subscribe(ctx, "auto-topic"); !errors.Is(err, ErrClosed) {
		t.Errorf("Subscribe(closed) = %v, want ErrClosed", err)
	}
	if err := c.Publish(ctx, "auto-topic", []byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("Publish(closed) = %v, want ErrClosed", err)
	}
	if err := c.Unsubscribe(ctx, "auto-topic"); !errors.Is(err, ErrClosed) {
		t.Errorf("Unsubscribe(closed) = %v, want ErrClosed", err)
	}
}

func TestClient_NoGoroutineLeaks(t *testing.T) {
	ctx := t.Context()
	cluster := kfake.MustCluster(
		kfake.NumBrokers(2),
		kfake.SeedTopics(2, "leak-broadcast", "leak-group"),
	)

	// Broadcast lifecycle.
	bClient, err := NewClient(cluster.ListenAddrs())
	if err != nil {
		t.Fatalf("NewClient(broadcast) error = %v", err)
	}
	bCh, err := bClient.Subscribe(ctx, "leak-broadcast")
	if err != nil {
		t.Fatalf("bClient.Subscribe() error = %v", err)
	}
	if err := bClient.Publish(ctx, "leak-broadcast", []byte("b1")); err != nil {
		t.Fatalf("bClient.Publish() error = %v", err)
	}
	<-bCh
	if err := bClient.Unsubscribe(ctx, "leak-broadcast"); err != nil {
		t.Fatalf("bClient.Unsubscribe() error = %v", err)
	}
	if err := bClient.Close(); err != nil {
		t.Fatalf("bClient.Close() error = %v", err)
	}

	// Consumer group lifecycle.
	gClient, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup("leak-cg"))
	if err != nil {
		t.Fatalf("NewClient(group) error = %v", err)
	}
	gCh, err := gClient.Subscribe(ctx, "leak-group")
	if err != nil {
		t.Fatalf("gClient.Subscribe() error = %v", err)
	}
	if err := gClient.Publish(ctx, "leak-group", []byte("g1")); err != nil {
		t.Fatalf("gClient.Publish() error = %v", err)
	}
	<-gCh
	if err := gClient.Close(); err != nil {
		t.Fatalf("gClient.Close() error = %v", err)
	}

	cluster.Close()
	assertNoGoroutineLeaks(t)
}

func TestClient_BlockRebalanceOnPoll_NoDeadlock(t *testing.T) {
	ctx := t.Context()
	cluster := kfake.MustCluster(
		kfake.NumBrokers(2),
		kfake.SeedTopics(4, "block-rebalance-topic"),
		kfake.BrokerConfigs(map[string]string{
			"group.consumer.heartbeat.interval.ms": "100",
		}),
	)

	pub, err := NewClient(
		cluster.ListenAddrs(),
		WithKafkaOptions(kgo.RecordPartitioner(kgo.RoundRobinPartitioner())),
	)
	if err != nil {
		t.Fatalf("NewClient(pub) error = %v", err)
	}

	c1, err := NewClient(
		cluster.ListenAddrs(),
		WithConsumerGroup("block-rebalance-cg"),
		WithKafkaOptions(kgo.BlockRebalanceOnPoll()),
	)
	if err != nil {
		t.Fatalf("NewClient(c1) error = %v", err)
	}

	c2, err := NewClient(
		cluster.ListenAddrs(),
		WithConsumerGroup("block-rebalance-cg"),
		WithKafkaOptions(kgo.BlockRebalanceOnPoll()),
	)
	if err != nil {
		t.Fatalf("NewClient(c2) error = %v", err)
	}

	ch1, err := c1.Subscribe(ctx, "block-rebalance-topic")
	if err != nil {
		t.Fatalf("c1.Subscribe() error = %v", err)
	}

	// Publish & consume on c1 so PollFetches acquires the rebalance block
	// at least once before c2 joins and triggers a rebalance, and before
	// Unsubscribe / Close run.
	if err := pub.Publish(ctx, "block-rebalance-topic", []byte("before-join")); err != nil {
		t.Fatalf("Publish(before-join) error = %v", err)
	}
	select {
	case msg := <-ch1:
		if string(msg) != "before-join" {
			t.Fatalf("got %q, want before-join", string(msg))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for before-join message")
	}

	ch2, err := c2.Subscribe(ctx, "block-rebalance-topic")
	if err != nil {
		t.Fatalf("c2.Subscribe() error = %v", err)
	}

	// Publish across all 4 partitions and verify all 4 post-join records are
	// delivered without loss under BlockRebalanceOnPoll (with c2 receiving at
	// least one) before Unsubscribe / Close.
	for i := range 4 {
		if err := pub.Publish(ctx, "block-rebalance-topic", []byte(fmt.Sprintf("after-join-%d", i))); err != nil {
			t.Fatalf("Publish(after-join-%d) error = %v", i, err)
		}
	}
	afterSeen := make(map[string]int, 4)
	var afterC2 int
	afterDeadline := time.After(5 * time.Second)
	for len(afterSeen) < 4 {
		select {
		case msg := <-ch1:
			afterSeen[string(msg)]++
		case msg := <-ch2:
			afterSeen[string(msg)]++
			afterC2++
		case <-afterDeadline:
			t.Fatalf("timed out waiting for after-join messages, got %d/4 (c2=%d)", len(afterSeen), afterC2)
		}
	}
	if afterC2 == 0 {
		t.Fatal("expected c2 to receive at least one after-join message under BlockRebalanceOnPoll")
	}
	_ = pub.Close()

	// Unsubscribe on c1 and Close on c2 must both complete promptly without deadlocking.
	doneUnsub := make(chan error, 1)
	go func() {
		doneUnsub <- c1.Unsubscribe(ctx, "block-rebalance-topic")
	}()
	select {
	case err := <-doneUnsub:
		if err != nil {
			t.Fatalf("c1.Unsubscribe() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("c1.Unsubscribe() deadlocked with kgo.BlockRebalanceOnPoll()")
	}

	if err := c1.Close(); err != nil {
		t.Fatalf("c1.Close() error = %v", err)
	}

	doneClose := make(chan error, 1)
	go func() {
		doneClose <- c2.Close()
	}()
	select {
	case err := <-doneClose:
		if err != nil {
			t.Fatalf("c2.Close() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("c2.Close() deadlocked with kgo.BlockRebalanceOnPoll()")
	}

	cluster.Close()
	assertNoGoroutineLeaks(t)
}

func TestClient_ConsumerGroupJoinWhileActive_NoMessageLoss(t *testing.T) {
	modes := []struct {
		name string
		opts []Option
	}{
		{
			name: "DefaultBalancer",
		},
		{
			name: "CooperativeStickyBalancer",
			opts: []Option{
				WithKafkaOptions(
					kgo.Balancers(kgo.CooperativeStickyBalancer()),
					kgo.HeartbeatInterval(100*time.Millisecond),
					kgo.SessionTimeout(6*time.Second),
				),
			},
		},
	}

	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			cluster := kfake.MustCluster(
				kfake.NumBrokers(3),
				kfake.SeedTopics(4, "join-active-topic"),
				kfake.BrokerConfigs(map[string]string{
					"group.consumer.heartbeat.interval.ms": "100",
				}),
			)

			pub, err := NewClient(
				cluster.ListenAddrs(),
				WithKafkaOptions(kgo.RecordPartitioner(kgo.RoundRobinPartitioner())),
			)
			if err != nil {
				t.Fatalf("NewClient(pub) error = %v", err)
			}

			groupName := "active-cg-" + tc.name
			opts1 := append([]Option{WithConsumerGroup(groupName)}, tc.opts...)
			opts2 := append([]Option{WithConsumerGroup(groupName)}, tc.opts...)

			g1, err := NewClient(cluster.ListenAddrs(), opts1...)
			if err != nil {
				t.Fatalf("NewClient(g1) error = %v", err)
			}
			g2, err := NewClient(cluster.ListenAddrs(), opts2...)
			if err != nil {
				t.Fatalf("NewClient(g2) error = %v", err)
			}

			ch1, err := g1.Subscribe(ctx, "join-active-topic")
			if err != nil {
				t.Fatalf("g1.Subscribe() error = %v", err)
			}

			// g2 joins while g1 is active holding all 4 partitions.
			ch2, err := g2.Subscribe(ctx, "join-active-topic")
			if err != nil {
				t.Fatalf("g2.Subscribe() error = %v", err)
			}

			// Publish 24 messages immediately with zero delay across all 4 partitions.
			const total = 24
			for i := range total {
				if err := pub.Publish(ctx, "join-active-topic", []byte(fmt.Sprintf("msg-%02d", i))); err != nil {
					t.Fatalf("Publish(%d) error = %v", i, err)
				}
			}

			seen := make(map[string]int, total)
			var count1, count2 int
			deadline := time.After(8 * time.Second)
			for count1+count2 < total {
				select {
				case msg := <-ch1:
					seen[string(msg)]++
					count1++
				case msg := <-ch2:
					seen[string(msg)]++
					count2++
				case <-deadline:
					t.Fatalf("timed out: count1=%d count2=%d total=%d/%d", count1, count2, count1+count2, total)
				}
			}

			if count1 == 0 || count2 == 0 {
				t.Errorf("expected both members to receive messages, got count1=%d count2=%d", count1, count2)
			}
			for i := range total {
				key := fmt.Sprintf("msg-%02d", i)
				if seen[key] != 1 {
					t.Errorf("message %q delivered %d times, want 1", key, seen[key])
				}
			}

			// Also verify 0-partition standby member on a 1-partition topic.
			_ = pub.Close()
			_ = g1.Close()
			_ = g2.Close()

			onePartCluster := kfake.MustCluster(
				kfake.NumBrokers(1),
				kfake.SeedTopics(1, "standby-1p"),
			)
			standbyGroup := "standby-cg-" + tc.name
			sOpts1 := append([]Option{WithConsumerGroup(standbyGroup)}, tc.opts...)
			sOpts2 := append([]Option{WithConsumerGroup(standbyGroup)}, tc.opts...)

			s1, err := NewClient(onePartCluster.ListenAddrs(), sOpts1...)
			if err != nil {
				t.Fatalf("NewClient(s1) error = %v", err)
			}
			s2, err := NewClient(onePartCluster.ListenAddrs(), sOpts2...)
			if err != nil {
				t.Fatalf("NewClient(s2) error = %v", err)
			}
			sPub, err := NewClient(onePartCluster.ListenAddrs())
			if err != nil {
				t.Fatalf("NewClient(sPub) error = %v", err)
			}

			s1Ch, err := s1.Subscribe(ctx, "standby-1p")
			if err != nil {
				t.Fatalf("s1.Subscribe() error = %v", err)
			}
			s2Ctx, cancelS2 := context.WithTimeout(ctx, 5*time.Second)
			s2Ch, err := s2.Subscribe(s2Ctx, "standby-1p")
			cancelS2()
			if err != nil {
				t.Fatalf("s2.Subscribe(standby) error = %v", err)
			}

			// Under default CooperativeStickyBalancer, s1 keeps partition 0 until
			// Unsubscribe while s2 remains standby.
			if err := sPub.Publish(ctx, "standby-1p", []byte("kept-on-s1")); err != nil {
				t.Fatalf("sPub.Publish(kept-on-s1) error = %v", err)
			}
			select {
			case msg := <-s1Ch:
				if string(msg) != "kept-on-s1" {
					t.Fatalf("s1 got %q, want kept-on-s1", string(msg))
				}
			case msg := <-s2Ch:
				t.Fatalf("expected s1 to keep partition 0 while active, but standby s2 received %q", string(msg))
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for kept-on-s1 on s1")
			}

			if err := s1.Unsubscribe(ctx, "standby-1p"); err != nil {
				t.Fatalf("s1.Unsubscribe() error = %v", err)
			}
			promoteDeadline := time.Now().Add(5 * time.Second)
			promoted := false
			for !promoted && time.Now().Before(promoteDeadline) {
				if err := sPub.Publish(ctx, "standby-1p", []byte("standby-promoted")); err != nil {
					t.Fatalf("sPub.Publish() error = %v", err)
				}
				select {
				case msg := <-s2Ch:
					if string(msg) == "standby-promoted" {
						promoted = true
					}
				case <-time.After(150 * time.Millisecond):
				}
			}
			if !promoted {
				t.Fatal("standby member s2 did not promote after s1.Unsubscribe")
			}

			_ = sPub.Close()
			_ = s1.Close()
			_ = s2.Close()
			onePartCluster.Close()
			cluster.Close()
			assertNoGoroutineLeaks(t)
		})
	}
}

type slowReadConn struct {
	net.Conn
	delay time.Duration
}

func (s *slowReadConn) Read(b []byte) (int, error) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.Conn.Read(b)
}

func TestClient_AsymmetricLatency_ConsumerGroupScaleOut_NoMessageLoss(t *testing.T) {
	const (
		iterations = 5
		total      = 16
	)

	for iter := range iterations {
		ctx := t.Context()
		cluster := kfake.MustCluster(
			kfake.NumBrokers(3),
			kfake.SeedTopics(4, "asym-scaleout"),
		)

		pub, err := NewClient(
			cluster.ListenAddrs(),
			WithKafkaOptions(kgo.RecordPartitioner(kgo.RoundRobinPartitioner())),
		)
		if err != nil {
			cluster.Close()
			t.Fatalf("iter %d: NewClient(pub) error = %v", iter, err)
		}

		group := fmt.Sprintf("asym-cg-%d", iter)
		c1, err := NewClient(
			cluster.ListenAddrs(),
			WithConsumerGroup(group),
			WithKafkaOptions(kgo.Dialer(func(dialCtx context.Context, network, addr string) (net.Conn, error) {
				conn, dialErr := (&net.Dialer{}).DialContext(dialCtx, network, addr)
				if dialErr != nil {
					return nil, dialErr
				}
				return &slowReadConn{Conn: conn, delay: 25 * time.Millisecond}, nil
			})),
		)
		if err != nil {
			_ = pub.Close()
			cluster.Close()
			t.Fatalf("iter %d: NewClient(c1) error = %v", iter, err)
		}

		c2, err := NewClient(
			cluster.ListenAddrs(),
			WithConsumerGroup(group),
		)
		if err != nil {
			_ = pub.Close()
			_ = c1.Close()
			cluster.Close()
			t.Fatalf("iter %d: NewClient(c2) error = %v", iter, err)
		}

		ch1, err := c1.Subscribe(ctx, "asym-scaleout")
		if err != nil {
			_ = pub.Close()
			_ = c1.Close()
			_ = c2.Close()
			cluster.Close()
			t.Fatalf("iter %d: c1.Subscribe() error = %v", iter, err)
		}

		ch2, err := c2.Subscribe(ctx, "asym-scaleout")
		if err != nil {
			_ = pub.Close()
			_ = c1.Close()
			_ = c2.Close()
			cluster.Close()
			t.Fatalf("iter %d: c2.Subscribe() error = %v", iter, err)
		}

		// Immediately publish 16 round-robin messages with 0ms delay after c2.Subscribe returns.
		for i := range total {
			if err := pub.Publish(ctx, "asym-scaleout", []byte(fmt.Sprintf("asym-%02d", i))); err != nil {
				_ = pub.Close()
				_ = c1.Close()
				_ = c2.Close()
				cluster.Close()
				t.Fatalf("iter %d: Publish(%d) error = %v", iter, i, err)
			}
		}

		seen := make(map[string]int, total)
		var count1, count2 int
		deadline := time.After(10 * time.Second)
		for count1+count2 < total {
			select {
			case msg := <-ch1:
				seen[string(msg)]++
				count1++
			case msg := <-ch2:
				seen[string(msg)]++
				count2++
			case <-deadline:
				_ = pub.Close()
				_ = c1.Close()
				_ = c2.Close()
				cluster.Close()
				t.Fatalf("iter %d: timed out: got=%d/%d (c1=%d c2=%d)", iter, count1+count2, total, count1, count2)
			}
		}

		if count1 == 0 || count2 == 0 {
			t.Errorf("iter %d: expected both c1 and c2 to receive messages, got c1=%d c2=%d", iter, count1, count2)
		}
		for i := range total {
			key := fmt.Sprintf("asym-%02d", i)
			if seen[key] != 1 {
				t.Errorf("iter %d: message %q delivered %d times, want 1", iter, key, seen[key])
			}
		}

		_ = pub.Close()
		_ = c1.Close()
		_ = c2.Close()
		cluster.Close()
	}

	assertNoGoroutineLeaks(t)
}

func TestClient_StandbyConfirmsAfterTransientRebalance(t *testing.T) {
	ctx := t.Context()
	cluster := kfake.MustCluster(
		kfake.NumBrokers(1),
		kfake.SeedTopics(1, "transient-1p"),
	)

	s1, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup("transient-cg"))
	if err != nil {
		cluster.Close()
		t.Fatalf("NewClient(s1) error = %v", err)
	}

	if _, err := s1.Subscribe(ctx, "transient-1p"); err != nil {
		_ = s1.Close()
		cluster.Close()
		t.Fatalf("s1.Subscribe() error = %v", err)
	}

	// Inject a single transient PreparingRebalance DescribeGroups response so
	// s2's standby check must retry and succeed on the next Stable response.
	var injected atomic.Bool
	cluster.ControlKey(int16(kmsg.DescribeGroups), func(_ kmsg.Request) (kmsg.Response, error, bool) {
		injected.Store(true)
		resp := kmsg.NewPtrDescribeGroupsResponse()
		g := kmsg.NewDescribeGroupsResponseGroup()
		g.Group = "transient-cg"
		g.State = "PreparingRebalance"
		resp.Groups = []kmsg.DescribeGroupsResponseGroup{g}
		return resp, nil, true
	})

	s2, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup("transient-cg"))
	if err != nil {
		_ = s1.Close()
		cluster.Close()
		t.Fatalf("NewClient(s2) error = %v", err)
	}

	subCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s2.Subscribe(subCtx, "transient-1p"); err != nil {
		_ = s1.Close()
		_ = s2.Close()
		cluster.Close()
		t.Fatalf("s2.Subscribe() after transient PreparingRebalance error = %v", err)
	}
	if !injected.Load() {
		_ = s1.Close()
		_ = s2.Close()
		cluster.Close()
		t.Fatal("expected injected PreparingRebalance DescribeGroups response to be hit")
	}

	_ = s1.Close()
	_ = s2.Close()
	cluster.Close()
	assertNoGoroutineLeaks(t)
}

type stallOnKeyConn struct {
	net.Conn
	targetKey uint16
	armed     atomic.Bool
	inFlight  chan struct{}
	once      *sync.Once
	closedCh  chan struct{}
	closeOnce sync.Once
}

func (s *stallOnKeyConn) Write(b []byte) (int, error) {
	if len(b) >= 6 && binary.BigEndian.Uint16(b[4:6]) == s.targetKey {
		s.armed.Store(true)
	}
	return s.Conn.Write(b)
}

func (s *stallOnKeyConn) Read(b []byte) (int, error) {
	if s.armed.CompareAndSwap(true, false) {
		s.once.Do(func() {
			close(s.inFlight)
		})
		select {
		case <-s.closedCh:
			return 0, io.EOF
		case <-time.After(5 * time.Second):
			return 0, io.EOF
		}
	}
	return s.Conn.Read(b)
}

func (s *stallOnKeyConn) Close() error {
	s.closeOnce.Do(func() {
		close(s.closedCh)
	})
	return s.Conn.Close()
}

func TestClient_CloseUnblocksPendingSubscribe(t *testing.T) {
	t.Run("PendingJoinGroup", func(t *testing.T) {
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "pending-topic"),
		)

		joinSeen := make(chan struct{})
		var joinOnce sync.Once
		cluster.ControlKey(int16(kmsg.JoinGroup), func(_ kmsg.Request) (kmsg.Response, error, bool) {
			cluster.KeepControl()
			joinOnce.Do(func() {
				close(joinSeen)
			})
			resp := kmsg.NewPtrJoinGroupResponse()
			resp.ErrorCode = kerr.CoordinatorLoadInProgress.Code
			return resp, nil, true
		})

		client, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup("pending-cg"))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		subErrCh := make(chan error, 1)
		go func() {
			// Pass context.Background() so only Client.Close() can unblock readiness wait.
			_, err := client.Subscribe(context.Background(), "pending-topic")
			subErrCh <- err
		}()

		select {
		case <-joinSeen:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for JoinGroup in pending Subscribe")
		}

		if err := client.Close(); err != nil {
			t.Fatalf("client.Close() error = %v", err)
		}

		select {
		case err := <-subErrCh:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("Subscribe() error = %v, want ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Subscribe(context.Background()) remained blocked after Client.Close()")
		}

		cluster.Close()
		assertNoGoroutineLeaks(t)
	})

	t.Run("PendingMetadataRetry", func(t *testing.T) {
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "pending-meta-topic"),
		)

		metaSeen := make(chan struct{})
		var metaOnce sync.Once
		cluster.ControlKey(int16(kmsg.Metadata), func(_ kmsg.Request) (kmsg.Response, error, bool) {
			cluster.KeepControl()
			metaOnce.Do(func() {
				close(metaSeen)
			})
			resp := kmsg.NewPtrMetadataResponse()
			topicResp := kmsg.NewMetadataResponseTopic()
			topicName := "pending-meta-topic"
			topicResp.Topic = &topicName
			topicResp.ErrorCode = kerr.LeaderNotAvailable.Code
			resp.Topics = []kmsg.MetadataResponseTopic{topicResp}
			return resp, nil, true
		})

		client, err := NewClient(cluster.ListenAddrs())
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		subErrCh := make(chan error, 1)
		go func() {
			_, err := client.Subscribe(context.Background(), "pending-meta-topic")
			subErrCh <- err
		}()

		select {
		case <-metaSeen:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for Metadata in pending Subscribe")
		}

		if err := client.Close(); err != nil {
			t.Fatalf("client.Close() error = %v", err)
		}

		select {
		case err := <-subErrCh:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("Subscribe() error = %v, want ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Subscribe(context.Background()) remained blocked on Metadata retry after Client.Close()")
		}

		cluster.Close()
		assertNoGoroutineLeaks(t)
	})

	t.Run("PendingListOffsetsRetry", func(t *testing.T) {
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "pending-offsets-topic"),
		)

		offsetsSeen := make(chan struct{})
		var offsetsOnce sync.Once
		cluster.ControlKey(int16(kmsg.ListOffsets), func(_ kmsg.Request) (kmsg.Response, error, bool) {
			cluster.KeepControl()
			offsetsOnce.Do(func() {
				close(offsetsSeen)
			})
			resp := kmsg.NewPtrListOffsetsResponse()
			topicResp := kmsg.NewListOffsetsResponseTopic()
			topicResp.Topic = "pending-offsets-topic"
			partResp := kmsg.NewListOffsetsResponseTopicPartition()
			partResp.Partition = 0
			partResp.ErrorCode = kerr.LeaderNotAvailable.Code
			topicResp.Partitions = []kmsg.ListOffsetsResponseTopicPartition{partResp}
			resp.Topics = []kmsg.ListOffsetsResponseTopic{topicResp}
			return resp, nil, true
		})

		client, err := NewClient(cluster.ListenAddrs())
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}

		subErrCh := make(chan error, 1)
		go func() {
			_, err := client.Subscribe(context.Background(), "pending-offsets-topic")
			subErrCh <- err
		}()

		select {
		case <-offsetsSeen:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for ListOffsets in pending Subscribe")
		}

		if err := client.Close(); err != nil {
			t.Fatalf("client.Close() error = %v", err)
		}

		select {
		case err := <-subErrCh:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("Subscribe() error = %v, want ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Subscribe(context.Background()) remained blocked on ListOffsets retry after Client.Close()")
		}

		cluster.Close()
		assertNoGoroutineLeaks(t)
	})

	for _, tc := range []struct {
		name      string
		targetKey uint16
	}{
		{name: "InFlightMetadataMidRead", targetKey: uint16(kmsg.Metadata)},
		{name: "InFlightListOffsetsMidRead", targetKey: uint16(kmsg.ListOffsets)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := kfake.MustCluster(
				kfake.NumBrokers(1),
				kfake.SeedTopics(1, "inflight-topic"),
			)

			inFlight := make(chan struct{})
			var once sync.Once
			client, err := NewClient(
				cluster.ListenAddrs(),
				WithKafkaOptions(kgo.Dialer(func(dialCtx context.Context, network, addr string) (net.Conn, error) {
					conn, dialErr := (&net.Dialer{}).DialContext(dialCtx, network, addr)
					if dialErr != nil {
						return nil, dialErr
					}
					return &stallOnKeyConn{
						Conn:      conn,
						targetKey: tc.targetKey,
						inFlight:  inFlight,
						once:      &once,
						closedCh:  make(chan struct{}),
					}, nil
				})),
			)
			if err != nil {
				cluster.Close()
				t.Fatalf("NewClient() error = %v", err)
			}

			subErrCh := make(chan error, 1)
			go func() {
				_, err := client.Subscribe(context.Background(), "inflight-topic")
				subErrCh <- err
			}()

			select {
			case <-inFlight:
			case <-time.After(5 * time.Second):
				_ = client.Close()
				cluster.Close()
				t.Fatalf("timed out waiting for in-flight %s read", tc.name)
			}

			if err := client.Close(); err != nil {
				t.Fatalf("client.Close() error = %v", err)
			}

			select {
			case err := <-subErrCh:
				if !errors.Is(err, ErrClosed) {
					t.Fatalf("Subscribe() error = %v, want ErrClosed", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Subscribe(context.Background()) remained blocked after Client.Close()")
			}

			cluster.Close()
			assertNoGoroutineLeaks(t)
		})
	}
}

func TestClient_ConsumerGroupSlowConsumerNoMessageLoss(t *testing.T) {
	ctx := t.Context()
	cluster := kfake.MustCluster(
		kfake.NumBrokers(1),
		kfake.SeedTopics(1, "batch-loss-topic", "disable-ac-topic", "greedy-ac-topic"),
	)
	defer cluster.Close()

	// 1. Slow consumer with WithChannelBuffer(1) receiving a multi-record batch:
	// Unsubscribe mid-batch must NOT commit offsets of undelivered records.
	c1, err := NewClient(
		cluster.ListenAddrs(),
		WithConsumerGroup("batch-loss-cg"),
		WithChannelBuffer(1),
	)
	if err != nil {
		t.Fatalf("NewClient(c1) error = %v", err)
	}
	ch1, err := c1.Subscribe(ctx, "batch-loss-topic")
	if err != nil {
		_ = c1.Close()
		t.Fatalf("c1.Subscribe() error = %v", err)
	}

	rawProd, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
	if err != nil {
		_ = c1.Close()
		t.Fatalf("kgo.NewClient(rawProd) error = %v", err)
	}
	const batchSize = 5
	recs := make([]*kgo.Record, batchSize)
	for i := range batchSize {
		recs[i] = &kgo.Record{
			Topic: "batch-loss-topic",
			Value: fmt.Appendf(nil, "b-%d", i),
		}
	}
	if err := rawProd.ProduceSync(ctx, recs...).FirstErr(); err != nil {
		rawProd.Close()
		_ = c1.Close()
		t.Fatalf("rawProd.ProduceSync() error = %v", err)
	}
	rawProd.Close()

	// Wait until at least one record is delivered out of ch1 and the 1-slot
	// buffer is saturated by a subsequent record, guaranteeing remaining
	// records in the batch block on ch1 <- rec.Value.
	var delivered []string
	select {
	case msg := <-ch1:
		delivered = append(delivered, string(msg))
	case <-time.After(5 * time.Second):
		_ = c1.Close()
		t.Fatal("timed out waiting for first batch record on ch1")
	}
	bufDeadline := time.Now().Add(2 * time.Second)
	for len(ch1) == 0 && time.Now().Before(bufDeadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if err := c1.Unsubscribe(ctx, "batch-loss-topic"); err != nil {
		_ = c1.Close()
		t.Fatalf("c1.Unsubscribe() error = %v", err)
	}
	for msg := range ch1 {
		delivered = append(delivered, string(msg))
	}
	if len(delivered) >= batchSize {
		_ = c1.Close()
		t.Fatalf("expected slow consumer c1 to stop before consuming all %d records, got %v", batchSize, delivered)
	}
	if err := c1.Close(); err != nil {
		t.Fatalf("c1.Close() error = %v", err)
	}

	c2, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup("batch-loss-cg"))
	if err != nil {
		t.Fatalf("NewClient(c2) error = %v", err)
	}
	ch2, err := c2.Subscribe(ctx, "batch-loss-topic")
	if err != nil {
		_ = c2.Close()
		t.Fatalf("c2.Subscribe() error = %v", err)
	}

	seen := make(map[string]int, batchSize)
	for _, s := range delivered {
		seen[s]++
	}
	var c2Count int
	wantRemaining := batchSize - len(delivered)
	remDeadline := time.After(5 * time.Second)
	for c2Count < wantRemaining {
		select {
		case msg := <-ch2:
			seen[string(msg)]++
			c2Count++
		case <-remDeadline:
			_ = c2.Close()
			t.Fatalf("c2 timed out waiting for undelivered batch records: c1=%v c2Count=%d wantRemaining=%d", delivered, c2Count, wantRemaining)
		}
	}
	if err := c2.Close(); err != nil {
		t.Fatalf("c2.Close() error = %v", err)
	}
	for i := range batchSize {
		key := fmt.Sprintf("b-%d", i)
		if seen[key] != 1 {
			t.Errorf("record %q delivered %d times across c1+c2, want 1 (c1=%v)", key, seen[key], delivered)
		}
	}

	// 2. WithConsumerGroup + WithKafkaOptions(kgo.DisableAutoCommit()) and
	// kgo.GreedyAutoCommit() must both construct, subscribe, deliver, and close cleanly.
	for _, tc := range []struct {
		name  string
		topic string
		group string
		opt   kgo.Opt
	}{
		{name: "DisableAutoCommit", topic: "disable-ac-topic", group: "disable-ac-cg", opt: kgo.DisableAutoCommit()},
		{name: "GreedyAutoCommit", topic: "greedy-ac-topic", group: "greedy-ac-cg", opt: kgo.GreedyAutoCommit()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cgClient, err := NewClient(
				cluster.ListenAddrs(),
				WithConsumerGroup(tc.group),
				WithKafkaOptions(tc.opt),
			)
			if err != nil {
				t.Fatalf("NewClient(%s) error = %v", tc.name, err)
			}
			ch, err := cgClient.Subscribe(ctx, tc.topic)
			if err != nil {
				_ = cgClient.Close()
				t.Fatalf("Subscribe(%s) error = %v", tc.name, err)
			}
			if err := cgClient.Publish(ctx, tc.topic, []byte("compat-ok")); err != nil {
				_ = cgClient.Close()
				t.Fatalf("Publish(%s) error = %v", tc.name, err)
			}
			select {
			case msg := <-ch:
				if string(msg) != "compat-ok" {
					t.Errorf("got %q, want compat-ok", string(msg))
				}
			case <-time.After(5 * time.Second):
				_ = cgClient.Close()
				t.Fatalf("timed out waiting for %s message", tc.name)
			}
			if err := cgClient.Close(); err != nil {
				t.Fatalf("Close(%s) error = %v", tc.name, err)
			}
		})
	}

	assertNoGoroutineLeaks(t)
}

func TestClient_KIP848StandbyConfirmed(t *testing.T) {
	ctx := t.Context()
	cluster := kfake.MustCluster(
		kfake.NumBrokers(1),
		kfake.SeedTopics(1, "kip848-topic"),
	)

	g1, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup("kip848-cg"))
	if err != nil {
		cluster.Close()
		t.Fatalf("NewClient(g1) error = %v", err)
	}
	if _, err := g1.Subscribe(ctx, "kip848-topic"); err != nil {
		_ = g1.Close()
		cluster.Close()
		t.Fatalf("g1.Subscribe() error = %v", err)
	}

	// Simulate KIP-848 coordinator where classic DescribeGroups returns
	// GroupIDNotFound and ConsumerGroupDescribe transitions Reconciling -> Stable.
	cluster.ControlKey(int16(kmsg.DescribeGroups), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
		cluster.KeepControl()
		req := kreq.(*kmsg.DescribeGroupsRequest)
		resp := req.ResponseKind().(*kmsg.DescribeGroupsResponse)
		resp.SetVersion(req.GetVersion())
		grp := kmsg.NewDescribeGroupsResponseGroup()
		grp.Group = "kip848-cg"
		grp.ErrorCode = kerr.GroupIDNotFound.Code
		resp.Groups = []kmsg.DescribeGroupsResponseGroup{grp}
		return resp, nil, true
	})

	var cgDescribeCalls atomic.Int32
	cluster.ControlKey(int16(kmsg.ConsumerGroupDescribe), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
		cluster.KeepControl()
		call := cgDescribeCalls.Add(1)
		req := kreq.(*kmsg.ConsumerGroupDescribeRequest)
		resp := req.ResponseKind().(*kmsg.ConsumerGroupDescribeResponse)
		resp.SetVersion(req.GetVersion())
		grp := kmsg.NewConsumerGroupDescribeResponseGroup()
		grp.Group = "kip848-cg"
		if call == 1 {
			grp.State = "Reconciling"
		} else {
			grp.State = "Stable"
			member := kmsg.NewConsumerGroupDescribeResponseGroupMember()
			tp := kmsg.NewAssignmentTopicPartition()
			tp.Topic = "kip848-topic"
			tp.Partitions = []int32{0}
			member.Assignment.TopicPartitions = []kmsg.AssignmentTopicPartition{tp}
			grp.Members = []kmsg.ConsumerGroupDescribeResponseGroupMember{member}
		}
		resp.Groups = []kmsg.ConsumerGroupDescribeResponseGroup{grp}
		return resp, nil, true
	})

	g2, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup("kip848-cg"))
	if err != nil {
		_ = g1.Close()
		cluster.Close()
		t.Fatalf("NewClient(g2) error = %v", err)
	}

	subCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := g2.Subscribe(subCtx, "kip848-topic"); err != nil {
		_ = g1.Close()
		_ = g2.Close()
		cluster.Close()
		t.Fatalf("g2.Subscribe() with KIP-848 fallback error = %v", err)
	}
	if cgDescribeCalls.Load() < 2 {
		t.Errorf("expected at least 2 ConsumerGroupDescribe calls (Reconciling -> Stable), got %d", cgDescribeCalls.Load())
	}

	_ = g1.Close()
	_ = g2.Close()
	cluster.Close()
	assertNoGoroutineLeaks(t)
}

func TestClient_SubscribeCtxCancelDuringGroupReadiness(t *testing.T) {
	ctx := t.Context()

	t.Run("GroupReadinessTimeout", func(t *testing.T) {
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "cancel-ready-topic"),
		)
		cluster.ControlKey(int16(kmsg.JoinGroup), func(_ kmsg.Request) (kmsg.Response, error, bool) {
			cluster.KeepControl()
			resp := kmsg.NewPtrJoinGroupResponse()
			resp.ErrorCode = kerr.CoordinatorLoadInProgress.Code
			return resp, nil, true
		})

		c, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup("cancel-ready-cg"))
		if err != nil {
			cluster.Close()
			t.Fatalf("NewClient() error = %v", err)
		}

		subCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if _, err := c.Subscribe(subCtx, "cancel-ready-topic"); !errors.Is(err, context.DeadlineExceeded) {
			_ = c.Close()
			cluster.Close()
			t.Fatalf("Subscribe() error = %v, want context.DeadlineExceeded", err)
		}
		if err := c.Close(); err != nil {
			cluster.Close()
			t.Fatalf("Close() error = %v", err)
		}
		cluster.Close()
		assertNoGoroutineLeaks(t)
	})

	t.Run("BroadcastCtxCancelBeforeRegistration", func(t *testing.T) {
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "late-cancel-broadcast"),
		)
		c, err := NewClient(cluster.ListenAddrs())
		if err != nil {
			cluster.Close()
			t.Fatalf("NewClient() error = %v", err)
		}

		subCtx, subCancel := context.WithCancel(ctx)
		cluster.ControlKey(int16(kmsg.ListOffsets), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			subCancel()
			req := kreq.(*kmsg.ListOffsetsRequest)
			resp := req.ResponseKind().(*kmsg.ListOffsetsResponse)
			resp.SetVersion(req.GetVersion())
			topicResp := kmsg.NewListOffsetsResponseTopic()
			topicResp.Topic = "late-cancel-broadcast"
			partResp := kmsg.NewListOffsetsResponseTopicPartition()
			partResp.Partition = 0
			partResp.Offset = 0
			topicResp.Partitions = []kmsg.ListOffsetsResponseTopicPartition{partResp}
			resp.Topics = []kmsg.ListOffsetsResponseTopic{topicResp}
			return resp, nil, true
		})

		ch, subErr := c.Subscribe(subCtx, "late-cancel-broadcast")
		if !errors.Is(subErr, context.Canceled) {
			_ = c.Close()
			cluster.Close()
			t.Fatalf("Subscribe(late cancel) error = %v, want context.Canceled", subErr)
		}
		if ch != nil {
			t.Fatalf("expected nil channel on canceled Subscribe, got %v", ch)
		}

		c.mu.RLock()
		subCount := len(c.subscriptions)
		c.mu.RUnlock()
		if subCount != 0 {
			t.Errorf("c.subscriptions len = %d, want 0 after canceled Subscribe", subCount)
		}

		if err := c.Close(); err != nil {
			cluster.Close()
			t.Fatalf("Close() error = %v", err)
		}
		cluster.Close()
		assertNoGoroutineLeaks(t)
	})
}

func TestClient_FetchErrorsAndPollRetry(t *testing.T) {
	ctx := t.Context()

	t.Run("NonRetriableListOffsetsError", func(t *testing.T) {
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "list-err-topic"),
		)
		cluster.ControlKey(int16(kmsg.ListOffsets), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			req := kreq.(*kmsg.ListOffsetsRequest)
			resp := req.ResponseKind().(*kmsg.ListOffsetsResponse)
			resp.SetVersion(req.GetVersion())
			topicResp := kmsg.NewListOffsetsResponseTopic()
			topicResp.Topic = "list-err-topic"
			partResp := kmsg.NewListOffsetsResponseTopicPartition()
			partResp.Partition = 0
			partResp.ErrorCode = kerr.TopicAuthorizationFailed.Code
			topicResp.Partitions = []kmsg.ListOffsetsResponseTopicPartition{partResp}
			resp.Topics = []kmsg.ListOffsetsResponseTopic{topicResp}
			return resp, nil, true
		})

		c, err := NewClient(cluster.ListenAddrs())
		if err != nil {
			cluster.Close()
			t.Fatalf("NewClient() error = %v", err)
		}
		if _, err := c.Subscribe(ctx, "list-err-topic"); !errors.Is(err, kerr.TopicAuthorizationFailed) {
			_ = c.Close()
			cluster.Close()
			t.Fatalf("Subscribe() error = %v, want TopicAuthorizationFailed", err)
		}
		_ = c.Close()
		cluster.Close()
		assertNoGoroutineLeaks(t)
	})

	t.Run("PollFetchesTransientErrorRetry", func(t *testing.T) {
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "poll-retry-topic"),
		)
		c, err := NewClient(cluster.ListenAddrs())
		if err != nil {
			cluster.Close()
			t.Fatalf("NewClient() error = %v", err)
		}

		ch, err := c.Subscribe(ctx, "poll-retry-topic")
		if err != nil {
			_ = c.Close()
			cluster.Close()
			t.Fatalf("Subscribe() error = %v", err)
		}

		// Inject one non-retriable fetch partition error so franz-go surfaces
		// it in fetches.Errors() and exercises receiveLoop's retryWait backoff.
		var fetchInjected atomic.Bool
		cluster.ControlKey(int16(kmsg.Fetch), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			if fetchInjected.Swap(true) {
				return nil, nil, false
			}
			req := kreq.(*kmsg.FetchRequest)
			resp := req.ResponseKind().(*kmsg.FetchResponse)
			resp.SetVersion(req.GetVersion())
			topicResp := kmsg.NewFetchResponseTopic()
			topicResp.Topic = "poll-retry-topic"
			partResp := kmsg.NewFetchResponseTopicPartition()
			partResp.Partition = 0
			partResp.ErrorCode = kerr.TopicAuthorizationFailed.Code
			topicResp.Partitions = []kmsg.FetchResponseTopicPartition{partResp}
			resp.Topics = []kmsg.FetchResponseTopic{topicResp}
			return resp, nil, true
		})

		// Publish after letting PollFetches observe the fetch error and retry.
		deadline := time.Now().Add(3 * time.Second)
		for !fetchInjected.Load() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !fetchInjected.Load() {
			_ = c.Close()
			cluster.Close()
			t.Fatal("expected injected Fetch error to be observed")
		}
		time.Sleep(receiveRetryDelay + 50*time.Millisecond)

		if err := c.Publish(ctx, "poll-retry-topic", []byte("after-fetch-retry")); err != nil {
			_ = c.Close()
			cluster.Close()
			t.Fatalf("Publish() error = %v", err)
		}
		select {
		case msg := <-ch:
			if string(msg) != "after-fetch-retry" {
				t.Errorf("got %q, want after-fetch-retry", string(msg))
			}
		case <-time.After(5 * time.Second):
			_ = c.Close()
			cluster.Close()
			t.Fatal("timed out waiting for message after PollFetches error retry")
		}

		_ = c.Close()
		cluster.Close()
		assertNoGoroutineLeaks(t)
	})
}

func TestClient_ConcurrentConsumerGroupLifecycleAndLeaks(t *testing.T) {
	ctx := t.Context()
	cluster := kfake.MustCluster(
		kfake.NumBrokers(2),
		kfake.SeedTopics(2, "cg-stress-0", "cg-stress-1"),
	)

	c, err := NewClient(
		cluster.ListenAddrs(),
		WithConsumerGroup("stress-cg"),
		WithChannelBuffer(4),
	)
	if err != nil {
		cluster.Close()
		t.Fatalf("NewClient() error = %v", err)
	}

	var wg sync.WaitGroup
	var drainers sync.WaitGroup
	topics := []string{"cg-stress-0", "cg-stress-1"}
	for g := range 4 {
		wg.Go(func() {
			for i := range 6 {
				topic := topics[(g+i)%len(topics)]
				switch (g + i) % 3 {
				case 0:
					if ch, subErr := c.Subscribe(ctx, topic); subErr == nil {
						drainers.Go(func() {
							for range ch {
							}
						})
					}
				case 1:
					_ = c.Publish(ctx, topic, []byte("cg-stress-msg"))
				case 2:
					_ = c.Unsubscribe(ctx, topic)
				}
			}
		})
	}

	time.Sleep(50 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	wg.Wait()
	drainers.Wait()

	cluster.Close()
	assertNoGoroutineLeaks(t)
}

type stallSecondaryFetchWriteConn struct {
	net.Conn
	addr      string
	fastMu    *sync.Mutex
	fastAddr  *string
	releaseCh <-chan struct{}
	closedCh  chan struct{}
	closeOnce sync.Once
}

func (s *stallSecondaryFetchWriteConn) Write(b []byte) (int, error) {
	if len(b) >= 6 && binary.BigEndian.Uint16(b[4:6]) == uint16(kmsg.Fetch) {
		s.fastMu.Lock()
		if *s.fastAddr == "" {
			*s.fastAddr = s.addr
		}
		isFast := *s.fastAddr == s.addr
		s.fastMu.Unlock()

		if !isFast {
			select {
			case <-s.releaseCh:
			case <-s.closedCh:
				return 0, net.ErrClosed
			case <-time.After(5 * time.Second):
			}
		}
	}
	return s.Conn.Write(b)
}

func (s *stallSecondaryFetchWriteConn) Close() error {
	s.closeOnce.Do(func() {
		close(s.closedCh)
	})
	return s.Conn.Close()
}

type abortProbeHook struct {
	fetched atomic.Int64
	once    sync.Once
	readyCh chan struct{}
}

func (h *abortProbeHook) OnFetchRecordUnbuffered(_ *kgo.Record, polled bool) {
	if !polled {
		return
	}
	h.fetched.Add(1)
	h.once.Do(func() {
		close(h.readyCh)
	})
}

func TestClient_ConsumerGroupSubscribeAbortNoMessageLoss(t *testing.T) {
	for _, tc := range []struct {
		name      string
		abortMode string
	}{
		{name: "ContextCancel", abortMode: "ctx"},
		{name: "ClientClose", abortMode: "close"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			topic := "cg-abort-topic-" + tc.name
			group := "cg-abort-" + tc.name

			cluster := kfake.MustCluster(
				kfake.NumBrokers(3),
				kfake.SeedTopics(6, topic),
				kfake.BrokerConfigs(map[string]string{
					"group.consumer.heartbeat.interval.ms": "100",
				}),
			)

			pub, err := NewClient(
				cluster.ListenAddrs(),
				WithKafkaOptions(kgo.RecordPartitioner(kgo.RoundRobinPartitioner())),
			)
			if err != nil {
				cluster.Close()
				t.Fatalf("NewClient(pub) error = %v", err)
			}

			// Seed the consumer group so all 6 partitions have committed offsets;
			// subsequent backlog records will be eligible for immediate fetch.
			seedSub, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup(group))
			if err != nil {
				_ = pub.Close()
				cluster.Close()
				t.Fatalf("NewClient(seedSub) error = %v", err)
			}
			seedCh, err := seedSub.Subscribe(ctx, topic)
			if err != nil {
				_ = seedSub.Close()
				_ = pub.Close()
				cluster.Close()
				t.Fatalf("seedSub.Subscribe() error = %v", err)
			}
			const seedCount = 6
			for i := range seedCount {
				if err := pub.Publish(ctx, topic, fmt.Appendf(nil, "seed-%d", i)); err != nil {
					_ = seedSub.Close()
					_ = pub.Close()
					cluster.Close()
					t.Fatalf("Publish(seed-%d) error = %v", i, err)
				}
			}
			for i := range seedCount {
				select {
				case <-seedCh:
				case <-time.After(5 * time.Second):
					_ = seedSub.Close()
					_ = pub.Close()
					cluster.Close()
					t.Fatalf("timed out waiting for seed message %d/%d", i, seedCount)
				}
			}
			if err := seedSub.Close(); err != nil {
				_ = pub.Close()
				cluster.Close()
				t.Fatalf("seedSub.Close() error = %v", err)
			}

			// Produce a multi-partition backlog across all 3 brokers while no consumer is active.
			const backlog = 12
			for i := range backlog {
				if err := pub.Publish(ctx, topic, fmt.Appendf(nil, "msg-%02d", i)); err != nil {
					_ = pub.Close()
					cluster.Close()
					t.Fatalf("Publish(backlog %d) error = %v", i, err)
				}
			}

			var (
				fastMu       sync.Mutex
				fastAddr     string
				releaseCh    = make(chan struct{})
				releaseOnce  sync.Once
				releaseFetch = func() {
					releaseOnce.Do(func() {
						close(releaseCh)
					})
				}
			)
			defer releaseFetch()

			probe := &abortProbeHook{readyCh: make(chan struct{})}
			c1, err := NewClient(
				cluster.ListenAddrs(),
				WithConsumerGroup(group),
				WithKafkaOptions(
					kgo.WithHooks(probe),
					kgo.Dialer(func(dialCtx context.Context, network, addr string) (net.Conn, error) {
						conn, dialErr := (&net.Dialer{}).DialContext(dialCtx, network, addr)
						if dialErr != nil {
							return nil, dialErr
						}
						return &stallSecondaryFetchWriteConn{
							Conn:      conn,
							addr:      addr,
							fastMu:    &fastMu,
							fastAddr:  &fastAddr,
							releaseCh: releaseCh,
							closedCh:  make(chan struct{}),
						}, nil
					}),
				),
			)
			if err != nil {
				_ = pub.Close()
				cluster.Close()
				t.Fatalf("NewClient(c1) error = %v", err)
			}

			subCtx, subCancel := context.WithCancel(context.Background())
			defer subCancel()

			type subResult struct {
				ch  <-chan []byte
				err error
			}
			subResCh := make(chan subResult, 1)
			go func() {
				ch, subErr := c1.Subscribe(subCtx, topic)
				subResCh <- subResult{ch: ch, err: subErr}
			}()

			// Wait until the fast broker returns backlog records while secondary
			// brokers' Fetch writes are stalled (readiness is still incomplete).
			select {
			case <-probe.readyCh:
			case <-time.After(8 * time.Second):
				releaseFetch()
				subCancel()
				_ = c1.Close()
				_ = pub.Close()
				cluster.Close()
				t.Fatal("timed out waiting for fast broker to deliver backlog records during readiness")
			}

			// Allow receive goroutine to buffer the polled records into the
			// not-yet-returned channel before aborting Subscribe.
			time.Sleep(50 * time.Millisecond)

			switch tc.abortMode {
			case "ctx":
				subCancel()
			case "close":
				if err := c1.Close(); err != nil {
					releaseFetch()
					_ = pub.Close()
					cluster.Close()
					t.Fatalf("c1.Close() error = %v", err)
				}
			}

			var res subResult
			select {
			case res = <-subResCh:
			case <-time.After(8 * time.Second):
				releaseFetch()
				_ = c1.Close()
				_ = pub.Close()
				cluster.Close()
				t.Fatal("timed out waiting for aborted c1.Subscribe() to return")
			}

			if res.ch != nil {
				t.Fatalf("expected nil channel on aborted Subscribe, got %v", res.ch)
			}
			switch tc.abortMode {
			case "ctx":
				if !errors.Is(res.err, context.Canceled) {
					t.Fatalf("c1.Subscribe() error = %v, want context.Canceled", res.err)
				}
			case "close":
				if !errors.Is(res.err, ErrClosed) {
					t.Fatalf("c1.Subscribe() error = %v, want ErrClosed", res.err)
				}
			}
			if probe.fetched.Load() == 0 {
				t.Fatal("expected at least 1 record fetched during readiness before Subscribe aborted")
			}

			if tc.abortMode == "ctx" {
				if err := c1.Close(); err != nil {
					releaseFetch()
					_ = pub.Close()
					cluster.Close()
					t.Fatalf("c1.Close() error = %v", err)
				}
			}
			releaseFetch()

			// Subsequent consumer in the same group must receive 100% of the backlog.
			c2, err := NewClient(cluster.ListenAddrs(), WithConsumerGroup(group))
			if err != nil {
				_ = pub.Close()
				cluster.Close()
				t.Fatalf("NewClient(c2) error = %v", err)
			}
			ch2, err := c2.Subscribe(ctx, topic)
			if err != nil {
				_ = c2.Close()
				_ = pub.Close()
				cluster.Close()
				t.Fatalf("c2.Subscribe() error = %v", err)
			}

			seen := make(map[string]int, backlog)
			var gotCount int
			deadline := time.After(10 * time.Second)
			for gotCount < backlog {
				select {
				case msg := <-ch2:
					seen[string(msg)]++
					gotCount++
				case <-deadline:
					_ = c2.Close()
					_ = pub.Close()
					cluster.Close()
					t.Fatalf("c2 timed out after %d/%d backlog messages (c1 fetched=%d): seen=%v",
						gotCount, backlog, probe.fetched.Load(), seen)
				}
			}
			for i := range backlog {
				key := fmt.Sprintf("msg-%02d", i)
				if seen[key] != 1 {
					t.Errorf("backlog record %q received %d times by c2, want 1", key, seen[key])
				}
			}

			if err := c2.Close(); err != nil {
				_ = pub.Close()
				cluster.Close()
				t.Fatalf("c2.Close() error = %v", err)
			}
			if err := pub.Close(); err != nil {
				cluster.Close()
				t.Fatalf("pub.Close() error = %v", err)
			}
			cluster.Close()
			assertNoGoroutineLeaks(t)
		})
	}
}

type kafkaLogEntry struct {
	level   slog.Level
	message string
	attrs   map[string]string
}

type kafkaCaptureHandler struct {
	mu      sync.Mutex
	entries []kafkaLogEntry
}

func (h *kafkaCaptureHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= slog.LevelDebug
}

func (h *kafkaCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.entries = append(h.entries, kafkaLogEntry{
		level:   r.Level,
		message: r.Message,
		attrs:   attrs,
	})
	h.mu.Unlock()
	return nil
}

func (h *kafkaCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *kafkaCaptureHandler) WithGroup(string) slog.Handler      { return h }

func (h *kafkaCaptureHandler) hasWarn(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		if e.level == slog.LevelWarn && strings.Contains(e.message, substr) {
			return true
		}
	}
	return false
}

func (h *kafkaCaptureHandler) snapshot() []kafkaLogEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]kafkaLogEntry(nil), h.entries...)
}

func TestWithLogger(t *testing.T) {
	t.Run("NilLoggerIsSafe", func(t *testing.T) {
		cluster := kfake.MustCluster(kfake.NumBrokers(1))
		defer cluster.Close()

		c, err := NewClient(cluster.ListenAddrs(), WithLogger(nil))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		defer func() { _ = c.Close() }()
		c.log().Warn("discarded")

		zeroClient := &Client{}
		zeroClient.log().Warn("discarded")
	})

	t.Run("PollFetchesLogsFetchError", func(t *testing.T) {
		ctx := t.Context()
		handler := &kafkaCaptureHandler{}
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "log-fetch-topic"),
		)
		defer cluster.Close()

		c, err := NewClient(
			cluster.ListenAddrs(),
			WithLogger(slog.New(handler)),
			WithKafkaOptions(kgo.FetchMaxWait(50*time.Millisecond)),
		)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		defer func() { _ = c.Close() }()

		ch, err := c.Subscribe(ctx, "log-fetch-topic")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}

		var fetchInjected atomic.Bool
		cluster.ControlKey(int16(kmsg.Fetch), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			if fetchInjected.Swap(true) {
				return nil, nil, false
			}
			req := kreq.(*kmsg.FetchRequest)
			resp := req.ResponseKind().(*kmsg.FetchResponse)
			resp.SetVersion(req.GetVersion())
			topicResp := kmsg.NewFetchResponseTopic()
			topicResp.Topic = "log-fetch-topic"
			if len(req.Topics) > 0 {
				topicResp.TopicID = req.Topics[0].TopicID
			}
			partResp := kmsg.NewFetchResponseTopicPartition()
			partResp.Partition = 0
			partResp.ErrorCode = kerr.TopicAuthorizationFailed.Code
			topicResp.Partitions = []kmsg.FetchResponseTopicPartition{partResp}
			resp.Topics = []kmsg.FetchResponseTopic{topicResp}
			return resp, nil, true
		})

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if handler.hasWarn("kafka fetch error") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !handler.hasWarn("kafka fetch error") {
			t.Errorf("expected kafka fetch error warn log, got %+v", handler.snapshot())
		}

		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		for range ch {
		}
		assertNoGoroutineLeaks(t)
	})

	t.Run("StopSubscriptionLogsCommitAndLeaveErrors", func(t *testing.T) {
		ctx := t.Context()
		handler := &kafkaCaptureHandler{}
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "log-commit-topic"),
		)
		defer cluster.Close()

		c, err := NewClient(
			cluster.ListenAddrs(),
			WithConsumerGroup("log-cg"),
			WithLogger(slog.New(handler)),
		)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		defer func() { _ = c.Close() }()

		ch, err := c.Subscribe(ctx, "log-commit-topic")
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}

		if err := c.Publish(ctx, "log-commit-topic", []byte("one")); err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for message")
		}

		// Grab entry and stop with an already-canceled context so CommitMarkedOffsets
		// and LeaveGroupContext both fail and emit structured warn logs.
		c.mu.Lock()
		entry := c.subscriptions["log-commit-topic"]
		delete(c.subscriptions, "log-commit-topic")
		c.mu.Unlock()

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		c.stopSubscription(canceledCtx, entry, true)
		for range ch {
		}

		if !handler.hasWarn("kafka commit marked offsets failed") {
			t.Errorf("expected commit marked offsets warn log, got %+v", handler.entries)
		}
		if !handler.hasWarn("kafka leave group failed") {
			t.Errorf("expected leave group warn log, got %+v", handler.entries)
		}

		if err := c.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		assertNoGoroutineLeaks(t)
	})
}

// Compile-time structural check that Client satisfies memorystore.PubSubClient.
type pubSubContract interface {
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)
	Publish(ctx context.Context, topic string, message []byte) error
	Unsubscribe(ctx context.Context, topic string) error
	Close() error
}

var _ pubSubContract = (*Client)(nil)

func TestClient_ConsumerGroupGreedyAutoCommitAndCancelCommitLog(t *testing.T) {
	ctx := t.Context()
	handler := &kafkaCaptureHandler{}
	cluster := kfake.MustCluster(
		kfake.NumBrokers(1),
		kfake.SeedTopics(1, "greedy-cg-topic"),
	)
	defer cluster.Close()

	pub, err := NewClient(cluster.ListenAddrs())
	if err != nil {
		t.Fatalf("NewClient(pub) error = %v", err)
	}
	defer func() { _ = pub.Close() }()

	c, err := NewClient(
		cluster.ListenAddrs(),
		WithConsumerGroup("greedy-cg"),
		WithLogger(slog.New(handler)),
		WithKafkaOptions(kgo.GreedyAutoCommit()),
	)
	if err != nil {
		t.Fatalf("NewClient(greedy) error = %v", err)
	}
	defer func() { _ = c.Close() }()

	ch, err := c.Subscribe(ctx, "greedy-cg-topic")
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if err := pub.Publish(ctx, "greedy-cg-topic", []byte("greedy-msg")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for greedy-msg")
	}

	c.mu.Lock()
	entry := c.subscriptions["greedy-cg-topic"]
	delete(c.subscriptions, "greedy-cg-topic")
	c.mu.Unlock()

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	c.stopSubscription(canceledCtx, entry, true)
	for range ch {
	}

	if !handler.hasWarn("kafka commit uncommitted offsets failed") {
		t.Errorf("expected commit uncommitted offsets warn log, got %+v", handler.snapshot())
	}
	assertNoGoroutineLeaks(t)
}

func TestClient_MetadataOffsetsAndStandbyErrorBranches(t *testing.T) {
	ctx := t.Context()

	t.Run("MetadataAndListOffsetsRetryThenFailAndCancel", func(t *testing.T) {
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "retry-meta-topic"),
		)
		defer cluster.Close()

		c, err := NewClient(cluster.ListenAddrs(), WithLogger(slog.New(&kafkaCaptureHandler{})))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		defer func() { _ = c.Close() }()

		// 1. Metadata retryable LeaderNotAvailable followed by success.
		var metaInjected atomic.Bool
		cluster.ControlKey(int16(kmsg.Metadata), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			if metaInjected.Swap(true) {
				return nil, nil, false
			}
			req := kreq.(*kmsg.MetadataRequest)
			resp := req.ResponseKind().(*kmsg.MetadataResponse)
			tr := kmsg.NewMetadataResponseTopic()
			name := "retry-meta-topic"
			tr.Topic = &name
			tr.ErrorCode = kerr.LeaderNotAvailable.Code
			resp.Topics = []kmsg.MetadataResponseTopic{tr}
			return resp, nil, true
		})
		leaders, err := c.fetchTopicPartitions(ctx, "retry-meta-topic")
		if err != nil || len(leaders) != 1 {
			t.Fatalf("fetchTopicPartitions(retry) = (%v, %v), want 1 partition", leaders, err)
		}

		// 2. ListOffsets retryable LeaderNotAvailable followed by success.
		var offInjected atomic.Bool
		cluster.ControlKey(int16(kmsg.ListOffsets), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			if offInjected.Swap(true) {
				return nil, nil, false
			}
			req := kreq.(*kmsg.ListOffsetsRequest)
			resp := req.ResponseKind().(*kmsg.ListOffsetsResponse)
			tr := kmsg.NewListOffsetsResponseTopic()
			tr.Topic = "retry-meta-topic"
			pr := kmsg.NewListOffsetsResponseTopicPartition()
			pr.Partition = 0
			pr.ErrorCode = kerr.LeaderNotAvailable.Code
			tr.Partitions = []kmsg.ListOffsetsResponseTopicPartition{pr}
			resp.Topics = []kmsg.ListOffsetsResponseTopic{tr}
			return resp, nil, true
		})
		offsets, err := c.fetchEndOffsets(ctx, "retry-meta-topic", leaders)
		if err != nil || len(offsets) != 1 {
			t.Fatalf("fetchEndOffsets(retry) = (%v, %v), want 1 offset", offsets, err)
		}

		// 3. Non-retryable ListOffsets error returns immediately.
		cluster.ControlKey(int16(kmsg.ListOffsets), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			req := kreq.(*kmsg.ListOffsetsRequest)
			resp := req.ResponseKind().(*kmsg.ListOffsetsResponse)
			tr := kmsg.NewListOffsetsResponseTopic()
			tr.Topic = "retry-meta-topic"
			pr := kmsg.NewListOffsetsResponseTopicPartition()
			pr.Partition = 0
			pr.ErrorCode = kerr.TopicAuthorizationFailed.Code
			tr.Partitions = []kmsg.ListOffsetsResponseTopicPartition{pr}
			resp.Topics = []kmsg.ListOffsetsResponseTopic{tr}
			return resp, nil, true
		})
		if _, err := c.fetchEndOffsets(ctx, "retry-meta-topic", leaders); !errors.Is(err, kerr.TopicAuthorizationFailed) {
			t.Fatalf("fetchEndOffsets(non-retryable) = %v, want TopicAuthorizationFailed", err)
		}

		// 4. Context canceled during ListOffsets retry sleep.
		cluster.ControlKey(int16(kmsg.ListOffsets), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			cluster.KeepControl()
			req := kreq.(*kmsg.ListOffsetsRequest)
			resp := req.ResponseKind().(*kmsg.ListOffsetsResponse)
			tr := kmsg.NewListOffsetsResponseTopic()
			tr.Topic = "retry-meta-topic"
			pr := kmsg.NewListOffsetsResponseTopicPartition()
			pr.Partition = 0
			pr.ErrorCode = kerr.LeaderNotAvailable.Code
			tr.Partitions = []kmsg.ListOffsetsResponseTopicPartition{pr}
			resp.Topics = []kmsg.ListOffsetsResponseTopic{tr}
			return resp, nil, true
		})
		shortCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if _, err := c.fetchEndOffsets(shortCtx, "retry-meta-topic", leaders); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("fetchEndOffsets(timeout) = %v, want DeadlineExceeded", err)
		}

		// 5. Context canceled during Metadata retry sleep.
		cluster.ControlKey(int16(kmsg.Metadata), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			cluster.KeepControl()
			req := kreq.(*kmsg.MetadataRequest)
			resp := req.ResponseKind().(*kmsg.MetadataResponse)
			tr := kmsg.NewMetadataResponseTopic()
			name := "retry-meta-topic"
			tr.Topic = &name
			tr.ErrorCode = kerr.LeaderNotAvailable.Code
			resp.Topics = []kmsg.MetadataResponseTopic{tr}
			return resp, nil, true
		})
		shortMetaCtx, cancelMeta := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancelMeta()
		if _, err := c.fetchTopicPartitions(shortMetaCtx, "retry-meta-topic"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("fetchTopicPartitions(timeout) = %v, want DeadlineExceeded", err)
		}
	})

	t.Run("StandbyConfirmed848AndReadinessBranches", func(t *testing.T) {
		cluster := kfake.MustCluster(
			kfake.NumBrokers(1),
			kfake.SeedTopics(1, "standby-topic"),
		)
		defer cluster.Close()

		c, err := NewClient(cluster.ListenAddrs(), WithLogger(slog.New(&kafkaCaptureHandler{})))
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		defer func() { _ = c.Close() }()

		// 1. DescribeGroups GroupIDNotFound -> ConsumerGroupDescribe (KIP-848) Stable confirmed.
		var descMode atomic.Int32 // 0: GroupIDNotFound, 1: GroupAuthorizationFailed, 2: Dead
		cluster.ControlKey(int16(kmsg.DescribeGroups), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			cluster.KeepControl()
			req := kreq.(*kmsg.DescribeGroupsRequest)
			resp := req.ResponseKind().(*kmsg.DescribeGroupsResponse)
			g := kmsg.NewDescribeGroupsResponseGroup()
			g.Group = "cg-848"
			switch descMode.Load() {
			case 0:
				g.ErrorCode = kerr.GroupIDNotFound.Code
			case 1:
				g.ErrorCode = kerr.GroupAuthorizationFailed.Code
			default:
				g.State = "Dead"
			}
			resp.Groups = []kmsg.DescribeGroupsResponseGroup{g}
			return resp, nil, true
		})
		cluster.ControlKey(int16(kmsg.ConsumerGroupDescribe), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
			cluster.KeepControl()
			req := kreq.(*kmsg.ConsumerGroupDescribeRequest)
			resp := req.ResponseKind().(*kmsg.ConsumerGroupDescribeResponse)
			g := kmsg.NewConsumerGroupDescribeResponseGroup()
			g.Group = "cg-848"
			g.State = "Stable"
			m := kmsg.NewConsumerGroupDescribeResponseGroupMember()
			tp := kmsg.NewAssignmentTopicPartition()
			tp.Topic = "standby-topic"
			tp.Partitions = []int32{0}
			m.Assignment.TopicPartitions = []kmsg.AssignmentTopicPartition{tp}
			g.Members = []kmsg.ConsumerGroupDescribeResponseGroupMember{m}
			resp.Groups = []kmsg.ConsumerGroupDescribeResponseGroup{g}
			return resp, nil, true
		})

		gr := newGroupReadiness("standby-topic", "cg-848", c.producer, ctx, map[int32]int32{0: 0}, c.log())
		if !gr.isStandbyConfirmed() {
			t.Fatal("expected isStandbyConfirmed() == true via KIP-848 ConsumerGroupDescribe")
		}
		// Trigger onAssigned(empty) -> standby markReady -> subsequent onAssigned returns early.
		gr.onAssigned(nil)
		gr.onAssigned(nil)
		select {
		case <-gr.readyCh:
		default:
			t.Fatal("expected readyCh to be closed after standby confirmation")
		}

		// 2. Non-retriable DescribeGroups error -> false.
		descMode.Store(1)
		if gr.isStandbyConfirmed() {
			t.Fatal("expected isStandbyConfirmed() == false on GroupAuthorizationFailed")
		}

		// 3. Dead DescribeGroups state -> false.
		descMode.Store(2)
		if gr.isStandbyConfirmed() {
			t.Fatal("expected isStandbyConfirmed() == false on Dead state")
		}

		// 4. checkReadyLocked with empty neededLeaders and fetchedBrokers > 0.
		gr2 := newGroupReadiness("standby-topic", "cg-empty", c.producer, ctx, map[int32]int32{}, nil)
		gr2.assignedOnce = true
		gr2.fetchedBrokers[0] = struct{}{}
		gr2.checkReadyLocked()
		select {
		case <-gr2.readyCh:
		default:
			t.Fatal("expected readyCh closed when neededLeaders is empty and fetchedBrokers non-empty")
		}

		// 5. Transient group states helper coverage.
		for _, st := range []string{"PreparingRebalance", "CompletingRebalance", "Assigning", "Reconciling"} {
			if !isTransientGroupState(st) {
				t.Errorf("isTransientGroupState(%q) = false, want true", st)
			}
		}
		if isTransientGroupState("Dead") {
			t.Error("isTransientGroupState(Dead) = true, want false")
		}
	})
}
