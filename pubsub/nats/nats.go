package nats

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	jsReceiveRetryDelay     = 100 * time.Millisecond
	ensureStreamMaxAttempts = 6
	ensureStreamRetryBase   = 5 * time.Millisecond
	streamLockStripes       = 64
)

var processStreamLocks [streamLockStripes]sync.Mutex

// Common errors returned by the NATS Pub/Sub client.
var (
	ErrInvalidTopic         = errors.New("invalid topic")
	ErrInvalidURL           = errors.New("nats url is required")
	ErrInvalidConn          = errors.New("nats connection is required")
	ErrConnClosed           = errors.New("nats connection is closed")
	ErrClosed               = errors.New("nats pubsub client is closed")
	ErrInvalidJetStream     = errors.New("jetstream stream name is invalid")
	ErrInvalidDurablePrefix = errors.New("jetstream durable prefix is invalid")
)

// Client implements memorystore.PubSubClient using Core NATS or NATS JetStream.
type Client struct {
	conn          *nats.Conn
	js            jetstream.JetStream
	cfg           config
	closeConn     bool
	mu            sync.RWMutex
	streamMu      sync.Mutex
	subscriptions map[string]*natsSubscription
	closed        bool
}

type natsSubscription struct {
	topic   string
	sub     *nats.Subscription
	msgsCtx jetstream.MessagesContext
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewClient dials the given NATS server URL (or comma-separated cluster URLs)
// and returns a Client that owns the underlying *nats.Conn (closing it on
// Close unless overridden with WithCloseConn(false)).
func NewClient(url string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(url) == "" {
		return nil, ErrInvalidURL
	}

	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	conn, err := nats.Connect(url, cfg.natsOptions...)
	if err != nil {
		return nil, err
	}

	client, err := newClient(conn, cfg, true)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return client, nil
}

// NewClientFromConn wraps an existing *nats.Conn. By default, Close does not
// drain or close the caller-supplied connection unless overridden with
// WithCloseConn(true).
func NewClientFromConn(conn *nats.Conn, opts ...Option) (*Client, error) {
	if conn == nil {
		return nil, ErrInvalidConn
	}
	if conn.IsClosed() || conn.IsDraining() {
		return nil, ErrConnClosed
	}

	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	return newClient(conn, cfg, false)
}

func newClient(conn *nats.Conn, cfg config, defaultCloseConn bool) (*Client, error) {
	var js jetstream.JetStream
	if cfg.jetStreamEnabled {
		var err error
		js, err = jetstream.New(conn)
		if err != nil {
			return nil, err
		}
	}

	return &Client{
		conn:          conn,
		js:            js,
		cfg:           cfg,
		closeConn:     cfg.resolveCloseConn(defaultCloseConn),
		subscriptions: make(map[string]*natsSubscription),
	}, nil
}

func validateConfig(cfg config) error {
	if cfg.jetStreamEnabled && !isValidJSName(cfg.streamName) {
		return ErrInvalidJetStream
	}
	if cfg.durableEnabled && !cfg.jetStreamEnabled {
		return fmt.Errorf("%w: WithJetStreamDurable requires WithJetStream", ErrInvalidJetStream)
	}
	if cfg.durableEnabled && !isValidJSName(cfg.durablePrefix) {
		return ErrInvalidDurablePrefix
	}
	if cfg.queueGroup != "" && strings.ContainsAny(cfg.queueGroup, " \t\r\n") {
		return fmt.Errorf("%w: %q", nats.ErrBadQueueName, cfg.queueGroup)
	}
	return nil
}

func isValidJSName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == '\x7f' {
			return false
		}
		switch r {
		case '>', '*', '.', '/', '\\':
			return false
		}
	}
	return true
}

func sanitizeConsumerName(parts ...string) string {
	var stemParts []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		var b strings.Builder
		for _, r := range part {
			switch {
			case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
				b.WriteRune(r)
			case r == '*':
				b.WriteString("star")
			case r == '>':
				b.WriteString("gt")
			default:
				b.WriteByte('_')
			}
		}
		if b.Len() > 0 {
			stemParts = append(stemParts, b.String())
		}
	}

	stem := strings.Join(stemParts, "_")
	const maxStemLen = 48
	if len(stem) > maxStemLen {
		stem = stem[:maxStemLen]
	}

	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	digest := hex.EncodeToString(sum[:8])
	if stem == "" {
		return "ms_" + digest
	}
	return stem + "_" + digest
}

func subjectMatches(pattern, subject string) bool {
	if pattern == subject {
		return true
	}
	pTokens := strings.Split(pattern, ".")
	sTokens := strings.Split(subject, ".")
	for i, pt := range pTokens {
		if pt == ">" {
			return i < len(sTokens)
		}
		if i >= len(sTokens) {
			return false
		}
		if sTokens[i] == ">" {
			return false
		}
		if pt != "*" && pt != sTokens[i] {
			return false
		}
	}
	return len(pTokens) == len(sTokens)
}

func subjectsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	aTokens := strings.Split(a, ".")
	bTokens := strings.Split(b, ".")
	limit := min(len(aTokens), len(bTokens))
	for i := range limit {
		at, bt := aTokens[i], bTokens[i]
		if (at == ">" && i == len(aTokens)-1) || (bt == ">" && i == len(bTokens)-1) {
			return true
		}
		if at == "*" || bt == "*" || at == bt {
			continue
		}
		return false
	}
	return len(aTokens) == len(bTokens)
}

func mergeOverlappingSubjects(a, b string) string {
	aTokens := strings.Split(a, ".")
	bTokens := strings.Split(b, ".")
	limit := min(len(aTokens), len(bTokens))
	merged := make([]string, 0, max(len(aTokens), len(bTokens)))
	for i := range limit {
		at, bt := aTokens[i], bTokens[i]
		if (at == ">" && i == len(aTokens)-1) || (bt == ">" && i == len(bTokens)-1) {
			merged = append(merged, ">")
			return strings.Join(merged, ".")
		}
		if at == bt {
			merged = append(merged, at)
		} else {
			merged = append(merged, "*")
		}
	}
	return strings.Join(merged, ".")
}

func streamCoversSubject(subjects []string, topic string) bool {
	for _, subj := range subjects {
		if subjectMatches(subj, topic) {
			return true
		}
	}
	return false
}

func mergeStreamSubject(subjects []string, topic string) ([]string, bool) {
	if streamCoversSubject(subjects, topic) {
		return subjects, false
	}

	candidate := topic
	remaining := slices.Clone(subjects)
	for {
		mergedAny := false
		nextRemaining := make([]string, 0, len(remaining))
		for _, subj := range remaining {
			switch {
			case subjectMatches(candidate, subj):
				// candidate covers subj; prune subsumed subject.
			case subjectMatches(subj, candidate):
				candidate = subj
				mergedAny = true
			case subjectsOverlap(candidate, subj):
				candidate = mergeOverlappingSubjects(candidate, subj)
				mergedAny = true
			default:
				nextRemaining = append(nextRemaining, subj)
			}
		}
		remaining = nextRemaining
		if !mergedAny {
			break
		}
	}
	remaining = append(remaining, candidate)
	return remaining, true
}

func streamProcessLock(conn *nats.Conn, streamName string) *sync.Mutex {
	var scope string
	if conn != nil {
		scope = conn.ConnectedClusterName()
		if scope == "" {
			scope = conn.ConnectedServerId()
		}
		if scope == "" {
			scope = conn.ConnectedUrl()
		}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(scope))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(streamName))
	return &processStreamLocks[h.Sum32()%streamLockStripes]
}

func isRetryableStreamErr(err error) bool {
	if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
		return true
	}
	var jsErr jetstream.JetStreamError
	if errors.As(err, &jsErr) {
		if apiErr := jsErr.APIError(); apiErr != nil {
			if apiErr.ErrorCode == 10052 && strings.Contains(strings.ToLower(apiErr.Description), "overlaps with") {
				return true
			}
		}
	}
	return false
}

func waitEnsureStreamJitter(ctx context.Context) error {
	delay := ensureStreamRetryBase + time.Duration(rand.N(int64(ensureStreamRetryBase*2)))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) flushWithContext(ctx context.Context) error {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return c.conn.FlushWithContext(ctx)
	}
	flushCtx, cancel := context.WithTimeout(ctx, defaultCleanupTimeout)
	defer cancel()
	return c.conn.FlushWithContext(flushCtx)
}

func (c *Client) ensureStream(ctx context.Context, topic string) (jetstream.Stream, error) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()

	procMu := streamProcessLock(c.conn, c.cfg.streamName)
	procMu.Lock()
	defer procMu.Unlock()

	var lastErr error
	for attempt := range ensureStreamMaxAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		stream, err := c.js.Stream(ctx, c.cfg.streamName)
		if err == nil {
			if !c.cfg.jsAutoCreate {
				return stream, nil
			}
			info := stream.CachedInfo()
			if info == nil {
				info, err = stream.Info(ctx)
				if err != nil {
					lastErr = err
					if isRetryableStreamErr(err) && attempt < ensureStreamMaxAttempts-1 {
						if waitErr := waitEnsureStreamJitter(ctx); waitErr != nil {
							return nil, waitErr
						}
						continue
					}
					return nil, err
				}
			}

			updatedSubjects, changed := mergeStreamSubject(info.Config.Subjects, topic)
			if !changed {
				return stream, nil
			}

			updatedCfg := info.Config
			updatedCfg.Subjects = updatedSubjects
			updatedStream, updateErr := c.js.CreateOrUpdateStream(ctx, updatedCfg)
			if updateErr != nil {
				lastErr = updateErr
				if isRetryableStreamErr(updateErr) && attempt < ensureStreamMaxAttempts-1 {
					if waitErr := waitEnsureStreamJitter(ctx); waitErr != nil {
						return nil, waitErr
					}
					continue
				}
				return nil, updateErr
			}

			verifiedStream, verifyErr := c.js.Stream(ctx, c.cfg.streamName)
			if verifyErr != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				lastErr = verifyErr
				if attempt < ensureStreamMaxAttempts-1 {
					if waitErr := waitEnsureStreamJitter(ctx); waitErr != nil {
						return nil, waitErr
					}
					continue
				}
				return updatedStream, nil
			}
			if verifiedInfo := verifiedStream.CachedInfo(); verifiedInfo == nil || streamCoversSubject(verifiedInfo.Config.Subjects, topic) {
				return verifiedStream, nil
			}
			lastErr = fmt.Errorf("jetstream stream %q subject %q overwritten concurrently", c.cfg.streamName, topic)
			if attempt < ensureStreamMaxAttempts-1 {
				if waitErr := waitEnsureStreamJitter(ctx); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			return nil, lastErr
		}

		if errors.Is(err, nats.ErrNoResponders) {
			return nil, fmt.Errorf("%w: %w", jetstream.ErrJetStreamNotEnabled, err)
		}

		if errors.Is(err, jetstream.ErrStreamNotFound) && c.cfg.jsAutoCreate {
			streamCfg := jetstream.StreamConfig{
				Name:     c.cfg.streamName,
				Subjects: []string{topic},
			}
			if c.cfg.jsStorageSet {
				streamCfg.Storage = c.cfg.jsStorage
			}
			createdStream, createErr := c.js.CreateStream(ctx, streamCfg)
			if createErr != nil {
				lastErr = createErr
				if isRetryableStreamErr(createErr) && attempt < ensureStreamMaxAttempts-1 {
					if waitErr := waitEnsureStreamJitter(ctx); waitErr != nil {
						return nil, waitErr
					}
					continue
				}
				return nil, createErr
			}
			verifiedStream, verifyErr := c.js.Stream(ctx, c.cfg.streamName)
			if verifyErr != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				lastErr = verifyErr
				if attempt < ensureStreamMaxAttempts-1 {
					if waitErr := waitEnsureStreamJitter(ctx); waitErr != nil {
						return nil, waitErr
					}
					continue
				}
				return createdStream, nil
			}
			if verifiedInfo := verifiedStream.CachedInfo(); verifiedInfo == nil || streamCoversSubject(verifiedInfo.Config.Subjects, topic) {
				return verifiedStream, nil
			}
			lastErr = fmt.Errorf("jetstream stream %q subject %q overwritten concurrently", c.cfg.streamName, topic)
			if attempt < ensureStreamMaxAttempts-1 {
				if waitErr := waitEnsureStreamJitter(ctx); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			return nil, lastErr
		}

		return nil, err
	}

	return nil, lastErr
}

func (c *Client) setupCoreSubscription(ctx context.Context, topic string) (*nats.Subscription, error) {
	var (
		sub *nats.Subscription
		err error
	)
	if c.cfg.queueGroup != "" {
		sub, err = c.conn.QueueSubscribeSync(topic, c.cfg.queueGroup)
	} else {
		sub, err = c.conn.SubscribeSync(topic)
	}
	if err != nil {
		return nil, err
	}
	if err := c.flushWithContext(ctx); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}
	return sub, nil
}

func (c *Client) setupJetStreamSubscription(ctx context.Context, topic string) (jetstream.MessagesContext, bool, error) {
	stream, err := c.ensureStream(ctx, topic)
	if err != nil {
		return nil, false, err
	}

	var (
		cons        jetstream.Consumer
		ackExplicit bool
	)
	switch {
	case c.cfg.durableEnabled:
		durableName := sanitizeConsumerName(c.cfg.durablePrefix, c.cfg.queueGroup, topic)
		cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
			Durable:       durableName,
			FilterSubject: topic,
			AckPolicy:     jetstream.AckExplicitPolicy,
			DeliverPolicy: jetstream.DeliverAllPolicy,
		})
		ackExplicit = true
	case c.cfg.queueGroup != "":
		consName := sanitizeConsumerName("msq", c.cfg.queueGroup, topic)
		cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
			Name:              consName,
			FilterSubject:     topic,
			AckPolicy:         jetstream.AckExplicitPolicy,
			DeliverPolicy:     jetstream.DeliverNewPolicy,
			InactiveThreshold: defaultJSInactiveTTL,
		})
		ackExplicit = true
	default:
		cons, err = stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
			FilterSubjects: []string{topic},
			DeliverPolicy:  jetstream.DeliverNewPolicy,
		})
	}
	if err != nil {
		return nil, false, err
	}

	msgsCtx, err := cons.Messages()
	if err != nil {
		return nil, false, err
	}
	return msgsCtx, ackExplicit, nil
}

// Subscribe subscribes to topic and returns a channel delivering received
// message payloads. The provided ctx governs initial subscription setup only;
// background message delivery runs until Unsubscribe or Close is called.
func (c *Client) Subscribe(ctx context.Context, topic string) (<-chan []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topic == "" {
		return nil, ErrInvalidTopic
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	existing := c.subscriptions[topic]
	delete(c.subscriptions, topic)
	c.mu.Unlock()

	if existing != nil {
		_ = c.stopSubscription(ctx, existing)
	}

	var (
		pendingSub     *nats.Subscription
		pendingMsgsCtx jetstream.MessagesContext
		ackExplicit    bool
		err            error
	)
	if c.cfg.jetStreamEnabled {
		pendingMsgsCtx, ackExplicit, err = c.setupJetStreamSubscription(ctx, topic)
	} else {
		pendingSub, err = c.setupCoreSubscription(ctx, topic)
	}
	if err != nil {
		return nil, err
	}

	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			cleanupPendingSub(pendingSub, pendingMsgsCtx)
			return nil, ErrClosed
		}
		prev := c.subscriptions[topic]
		if prev == nil {
			break
		}
		delete(c.subscriptions, topic)
		c.mu.Unlock()
		_ = c.stopSubscription(ctx, prev)
	}

	ch := make(chan []byte, c.cfg.channelBuffer)
	recvCtx, cancel := context.WithCancel(context.Background())
	entry := &natsSubscription{
		topic:   topic,
		sub:     pendingSub,
		msgsCtx: pendingMsgsCtx,
		cancel:  cancel,
	}

	if pendingMsgsCtx != nil {
		entry.wg.Go(func() {
			runJetStreamLoop(recvCtx, pendingMsgsCtx, ackExplicit, ch)
		})
	} else {
		entry.wg.Go(func() {
			runCoreLoop(recvCtx, pendingSub, ch)
		})
	}

	c.subscriptions[topic] = entry
	c.mu.Unlock()

	return ch, nil
}

func cleanupPendingSub(sub *nats.Subscription, msgsCtx jetstream.MessagesContext) {
	if sub != nil {
		_ = sub.Unsubscribe()
	}
	if msgsCtx != nil {
		msgsCtx.Stop()
	}
}

func runCoreLoop(recvCtx context.Context, sub *nats.Subscription, outCh chan<- []byte) {
	defer close(outCh)
	for {
		msg, err := sub.NextMsgWithContext(recvCtx)
		if err != nil {
			if errors.Is(err, nats.ErrSlowConsumer) {
				continue
			}
			return
		}
		select {
		case outCh <- msg.Data:
		default:
			// Drop when slow subscriber buffer is full, matching InMemoryPubSub.
		}
	}
}

func runJetStreamLoop(recvCtx context.Context, msgsCtx jetstream.MessagesContext, ackExplicit bool, outCh chan<- []byte) {
	defer close(outCh)
	defer msgsCtx.Stop()
	for {
		if recvCtx.Err() != nil {
			return
		}
		msg, err := msgsCtx.Next(jetstream.NextContext(recvCtx))
		if err != nil {
			if recvCtx.Err() != nil ||
				errors.Is(err, jetstream.ErrMsgIteratorClosed) ||
				errors.Is(err, nats.ErrConnectionClosed) ||
				errors.Is(err, context.Canceled) {
				return
			}
			timer := time.NewTimer(jsReceiveRetryDelay)
			select {
			case <-timer.C:
			case <-recvCtx.Done():
				timer.Stop()
				return
			}
			continue
		}

		select {
		case outCh <- msg.Data():
			if ackExplicit {
				ackCtx, ackCancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
				_ = msg.DoubleAck(ackCtx)
				ackCancel()
			}
		case <-recvCtx.Done():
			if ackExplicit {
				_ = msg.Nak()
			}
			return
		}
	}
}

// Publish sends message to topic.
func (c *Client) Publish(ctx context.Context, topic string, message []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if topic == "" {
		return ErrInvalidTopic
	}

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return ErrClosed
	}
	conn := c.conn
	js := c.js
	jsEnabled := c.cfg.jetStreamEnabled
	c.mu.RUnlock()

	if jsEnabled {
		if _, err := c.ensureStream(ctx, topic); err != nil {
			return err
		}
		_, err := js.Publish(ctx, topic, message)
		return err
	}

	if err := conn.Publish(topic, message); err != nil {
		return err
	}
	return c.flushWithContext(ctx)
}

// Unsubscribe stops receiving messages for topic and closes its delivery
// channel. Calling Unsubscribe for an unknown topic is a no-op and returns nil.
func (c *Client) Unsubscribe(ctx context.Context, topic string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	entry, ok := c.subscriptions[topic]
	if !ok {
		c.mu.Unlock()
		return nil
	}
	delete(c.subscriptions, topic)
	c.mu.Unlock()

	return c.stopSubscription(ctx, entry)
}

func (c *Client) stopSubscription(ctx context.Context, entry *natsSubscription) error {
	if entry.msgsCtx != nil {
		entry.cancel()
		entry.msgsCtx.Stop()
		entry.wg.Wait()
		return nil
	}

	timeout := c.cfg.drainTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if rem := time.Until(deadline); rem < timeout {
			timeout = rem
		}
	}

	if timeout <= 0 || entry.sub.Drain() != nil {
		entry.cancel()
		_ = entry.sub.Unsubscribe()
		entry.wg.Wait()
		return nil
	}

	forceStop := func() {
		entry.cancel()
		_ = entry.sub.Unsubscribe()
	}
	timer := time.AfterFunc(timeout, forceStop)
	stopCtxWatch := context.AfterFunc(ctx, forceStop)
	entry.wg.Wait()
	timer.Stop()
	stopCtxWatch()
	entry.cancel()
	return nil
}

// Close stops all active subscriptions, waits for their delivery goroutines to
// exit, and drains/closes the underlying *nats.Conn if owned or configured via
// WithCloseConn(true). Multiple calls are safe and idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	subs := c.subscriptions
	c.subscriptions = nil
	c.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cancel()

	for _, entry := range subs {
		_ = c.stopSubscription(cleanupCtx, entry)
	}

	if !c.closeConn {
		return nil
	}

	if c.conn.IsClosed() {
		return nil
	}

	statusCh := c.conn.StatusChanged(nats.CLOSED)
	defer c.conn.RemoveStatusListener(statusCh)

	err := c.conn.Drain()
	if err != nil {
		if errors.Is(err, nats.ErrConnectionClosed) {
			return nil
		}
		if !errors.Is(err, nats.ErrConnectionDraining) {
			c.conn.Close()
			if errors.Is(err, nats.ErrConnectionReconnecting) {
				return nil
			}
			return err
		}
	}

	timer := time.NewTimer(c.cfg.drainTimeout)
	defer timer.Stop()

	select {
	case <-statusCh:
	case <-timer.C:
		c.conn.Close()
	}
	return nil
}
