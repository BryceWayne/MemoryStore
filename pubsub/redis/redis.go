package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultCleanupTimeout = 10 * time.Second
	unsubscribeTimeout    = 2 * time.Second
	receiveRetryDelay     = 100 * time.Millisecond
	defaultDialTimeout    = 5 * time.Second
)

// Common errors returned by the Redis Pub/Sub client.
var (
	ErrInvalidTopic   = errors.New("invalid topic")
	ErrInvalidOptions = errors.New("redis universal options with at least one address are required")
	ErrInvalidClient  = errors.New("redis universal client is required")
	ErrClosed         = errors.New("redis pubsub client is closed")
)

// Client implements memorystore.PubSubClient using go-redis UniversalClient
// Pub/Sub (supporting standalone, Sentinel, and Cluster topologies).
type Client struct {
	client        redis.UniversalClient
	cfg           config
	mu            sync.RWMutex
	subscriptions map[string]*redisSubscription
	wg            sync.WaitGroup
	handshakeWG   sync.WaitGroup
	closeCtx      context.Context
	closeCancel   context.CancelFunc
	closed        bool
}

type redisSubscription struct {
	topic     string
	isPattern bool
	ps        *redis.PubSub
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewClient creates a new Redis Pub/Sub client from redis.UniversalOptions.
// By default, Close also closes the created redis.UniversalClient unless
// overridden with WithCloseClient(false).
func NewClient(redisOpts *redis.UniversalOptions, opts ...Option) (*Client, error) {
	if err := validateUniversalOptions(redisOpts); err != nil {
		return nil, err
	}

	cfg := defaultConfig(true)
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	clonedOpts := *redisOpts
	clonedOpts.Dialer = wrapHandshakeDialer(redisOpts)
	client := redis.NewUniversalClient(&clonedOpts)
	return newClient(client, cfg), nil
}

// NewClientFromClient wraps an existing redis.UniversalClient as a
// memorystore.PubSubClient. By default, Close leaves the injected client open
// unless overridden with WithCloseClient(true).
func NewClientFromClient(client redis.UniversalClient, opts ...Option) (*Client, error) {
	if isNilClient(client) {
		return nil, ErrInvalidClient
	}

	cfg := defaultConfig(false)
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	return newClient(client, cfg), nil
}

func newClient(client redis.UniversalClient, cfg config) *Client {
	closeCtx, closeCancel := context.WithCancel(context.Background())
	return &Client{
		client:        client,
		cfg:           cfg,
		subscriptions: make(map[string]*redisSubscription),
		closeCtx:      closeCtx,
		closeCancel:   closeCancel,
	}
}

type handshakeAbortKey struct{}

type handshakeAbort struct {
	mu    sync.Mutex
	stops []func() bool
}

func (h *handshakeAbort) track(ctx context.Context, conn net.Conn) {
	stop := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})
	h.mu.Lock()
	h.stops = append(h.stops, stop)
	h.mu.Unlock()
}

func (h *handshakeAbort) disarmAll() {
	h.mu.Lock()
	stops := h.stops
	h.stops = nil
	h.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
}

func wrapHandshakeDialer(opts *redis.UniversalOptions) func(context.Context, string, string) (net.Conn, error) {
	baseDialer := opts.Dialer
	if baseDialer == nil {
		dialTimeout := opts.DialTimeout
		if dialTimeout == 0 {
			dialTimeout = defaultDialTimeout
		}
		baseDialer = redis.NewDialer(&redis.Options{
			DialTimeout: dialTimeout,
			TLSConfig:   opts.TLSConfig,
		})
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := baseDialer(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if abort, ok := ctx.Value(handshakeAbortKey{}).(*handshakeAbort); ok && abort != nil {
			abort.track(ctx, conn)
		}
		return conn, nil
	}
}

func validateUniversalOptions(opts *redis.UniversalOptions) error {
	if opts == nil || len(opts.Addrs) == 0 {
		return ErrInvalidOptions
	}
	for _, addr := range opts.Addrs {
		if strings.TrimSpace(addr) == "" {
			return ErrInvalidOptions
		}
	}
	return nil
}

func isNilClient(client redis.UniversalClient) bool {
	if client == nil {
		return true
	}
	v := reflect.ValueOf(client)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func:
		return v.IsNil()
	default:
		return false
	}
}

func isWildcardPattern(topic string) bool {
	return strings.ContainsAny(topic, "*?[")
}

func waitForSubscription(ctx context.Context, ps *redis.PubSub) error {
	for {
		msg, err := ps.Receive(ctx)
		if err != nil {
			return err
		}
		if _, ok := msg.(*redis.Subscription); ok {
			return nil
		}
	}
}

func closeUnusedPubSub(ctx context.Context, ps *redis.PubSub, topic string, isPattern bool) {
	cleanupCtx, cancel := context.WithTimeout(ctx, unsubscribeTimeout)
	if isPattern {
		_ = ps.PUnsubscribe(cleanupCtx, topic)
	} else {
		_ = ps.Unsubscribe(cleanupCtx, topic)
	}
	cancel()
	_ = ps.Close()
}

// Subscribe subscribes to topic and returns a channel delivering received
// message payloads. Topics containing '*', '?', or '[' are subscribed via
// PSUBSCRIBE; all other topics use SUBSCRIBE.
//
// The provided ctx governs only the initial subscription handshake; once
// Subscribe returns, the background delivery loop runs until Unsubscribe or
// Close is called.
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
	c.handshakeWG.Add(1)
	c.mu.Unlock()
	defer c.handshakeWG.Done()

	subCtx, subCancel := context.WithCancel(ctx)
	stopClose := context.AfterFunc(c.closeCtx, subCancel)
	defer func() {
		stopClose()
		subCancel()
	}()

	if existing != nil {
		_ = c.stopSubscription(subCtx, existing)
		if c.closeCtx.Err() != nil {
			return nil, ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	isPattern := isWildcardPattern(topic)
	abort := &handshakeAbort{}
	dialCtx := context.WithValue(subCtx, handshakeAbortKey{}, abort)

	psCh := make(chan *redis.PubSub, 1)
	go func() {
		if isPattern {
			psCh <- c.client.PSubscribe(dialCtx, topic)
		} else {
			psCh <- c.client.Subscribe(dialCtx, topic)
		}
	}()

	var ps *redis.PubSub
	select {
	case ps = <-psCh:
	case <-subCtx.Done():
		go func() {
			if p := <-psCh; p != nil {
				_ = p.Close()
			}
		}()
		if c.closeCtx.Err() != nil {
			return nil, ErrClosed
		}
		return nil, ctx.Err()
	}

	stopAfter := context.AfterFunc(subCtx, func() {
		_ = ps.Close()
	})
	err := waitForSubscription(subCtx, ps)
	// Disarm stopAfter and dialer abort hooks before stopClose/subCancel so
	// normal cleanup never closes ps or its underlying connection.
	stopped := stopAfter()
	abort.disarmAll()
	stopClose()
	subCancel()
	if !stopped || err != nil {
		_ = ps.Close()
		if c.closeCtx.Err() != nil {
			return nil, ErrClosed
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			return nil, fmt.Errorf("failed to subscribe to %q: %w", topic, err)
		}
		return nil, context.Canceled
	}

	c.mu.Lock()
	if c.closed || c.closeCtx.Err() != nil {
		c.mu.Unlock()
		closeUnusedPubSub(context.Background(), ps, topic, isPattern)
		return nil, ErrClosed
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		c.mu.Unlock()
		closeUnusedPubSub(c.closeCtx, ps, topic, isPattern)
		return nil, ctxErr
	}

	prev := c.subscriptions[topic]
	ch := make(chan []byte, c.cfg.channelBuffer)
	recvCtx, cancel := context.WithCancel(context.Background())
	entry := &redisSubscription{
		topic:     topic,
		isPattern: isPattern,
		ps:        ps,
		cancel:    cancel,
	}

	c.wg.Add(1)
	entry.wg.Go(func() {
		defer c.wg.Done()
		defer close(ch)
		for {
			if recvCtx.Err() != nil {
				return
			}
			msg, err := ps.ReceiveMessage(recvCtx)
			if err != nil {
				if recvCtx.Err() != nil {
					return
				}
				timer := time.NewTimer(receiveRetryDelay)
				select {
				case <-timer.C:
				case <-recvCtx.Done():
					timer.Stop()
					return
				}
				continue
			}

			select {
			case ch <- []byte(msg.Payload):
			case <-recvCtx.Done():
				return
			}
		}
	})

	c.subscriptions[topic] = entry
	c.mu.Unlock()

	if prev != nil {
		_ = c.stopSubscription(c.closeCtx, prev)
	}

	return ch, nil
}

// Publish publishes message to topic.
func (c *Client) Publish(ctx context.Context, topic string, message []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if topic == "" {
		return ErrInvalidTopic
	}

	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return ErrClosed
	}

	return c.client.Publish(ctx, topic, message).Err()
}

// Unsubscribe stops receiving messages for topic and closes its delivery
// channel. Unsubscribe is idempotent and returns nil if topic is not currently
// subscribed.
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

func (c *Client) stopSubscription(ctx context.Context, entry *redisSubscription) error {
	entry.cancel()

	unsubCtx, cancel := context.WithTimeout(ctx, unsubscribeTimeout)
	defer cancel()
	if entry.isPattern {
		_ = entry.ps.PUnsubscribe(unsubCtx, entry.topic)
	} else {
		_ = entry.ps.Unsubscribe(unsubCtx, entry.topic)
	}
	_ = entry.ps.Close()
	entry.wg.Wait()
	return nil
}

// Close stops all active subscriptions, waits for all delivery goroutines to
// exit and close their channels, and closes the underlying Redis client if
// configured (the default for NewClient). Multiple calls are safe and
// idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.closeCancel()
	subs := c.subscriptions
	c.subscriptions = nil
	c.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cancel()

	for _, entry := range subs {
		_ = c.stopSubscription(cleanupCtx, entry)
	}
	c.handshakeWG.Wait()
	c.wg.Wait()

	if c.cfg.closeClient {
		return c.client.Close()
	}
	return nil
}
