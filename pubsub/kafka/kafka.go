package kafka

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

const (
	defaultCleanupTimeout  = 10 * time.Second
	receiveRetryDelay      = 250 * time.Millisecond
	metadataRetryDelay     = 100 * time.Millisecond
	describeGroupTimeout   = 2 * time.Second
	describeGroupRetryWait = 50 * time.Millisecond
	describeGroupMaxTries  = 4
	kafkaFetchAPIKey       = int16(1)
)

// Common errors returned by the Kafka Pub/Sub client.
var (
	ErrInvalidTopic   = errors.New("invalid topic")
	ErrInvalidBrokers = errors.New("at least one Kafka broker address is required")
	ErrClosed         = errors.New("kafka client is closed")
)

// Client implements memorystore.PubSubClient using Apache Kafka or Redpanda
// via pure-Go franz-go (kgo).
type Client struct {
	cfg                config
	directConsumerOpts []kgo.Opt
	groupConsumerOpts  []kgo.Opt
	producer           *kgo.Client
	mu                 sync.RWMutex
	subscriptions      map[string]*kafkaSubscription
	closed             bool
	closeCh            chan struct{}
}

type kafkaSubscription struct {
	subClient    *kgo.Client
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	markMu       sync.Mutex
	armed        bool
	pendingMarks []*kgo.Record
}

// NewClient creates a new Kafka Pub/Sub client connected to brokers.
// At least one non-blank broker address is required.
func NewClient(brokers []string, opts ...Option) (*Client, error) {
	if len(brokers) == 0 {
		return nil, ErrInvalidBrokers
	}
	brokersCopy := make([]string, len(brokers))
	for i, b := range brokers {
		trimmed := strings.TrimSpace(b)
		if trimmed == "" {
			return nil, ErrInvalidBrokers
		}
		brokersCopy[i] = trimmed
	}

	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	commonOpts := []kgo.Opt{
		kgo.SeedBrokers(brokersCopy...),
	}
	if cfg.autoCreateTopics {
		commonOpts = append(commonOpts, kgo.AllowAutoTopicCreation())
	}
	if cfg.tlsConfig != nil {
		commonOpts = append(commonOpts, kgo.DialTLSConfig(cfg.tlsConfig))
	}
	if len(cfg.saslMechanisms) > 0 {
		commonOpts = append(commonOpts, kgo.SASL(cfg.saslMechanisms...))
	}

	extraProducerOpts, extraDirectConsumerOpts, extraGroupConsumerOpts := splitKafkaOptions(cfg.kafkaOptions)

	producerOpts := make([]kgo.Opt, 0, len(commonOpts)+len(extraProducerOpts))
	producerOpts = append(producerOpts, commonOpts...)
	producerOpts = append(producerOpts, extraProducerOpts...)

	producer, err := kgo.NewClient(producerOpts...)
	if err != nil {
		return nil, err
	}

	directConsumerOpts := make([]kgo.Opt, 0, len(commonOpts)+len(extraDirectConsumerOpts))
	directConsumerOpts = append(directConsumerOpts, commonOpts...)
	directConsumerOpts = append(directConsumerOpts, extraDirectConsumerOpts...)

	groupConsumerOpts := make([]kgo.Opt, 0, len(commonOpts)+len(extraGroupConsumerOpts))
	groupConsumerOpts = append(groupConsumerOpts, commonOpts...)
	// Leave franz-go's default CooperativeStickyBalancer intact so joining
	// members do not trigger eager stop-the-world revocations on active
	// members; groupReadiness.isStandbyConfirmed guards against premature
	// Round-1 readiness. Callers can still override via WithKafkaOptions.
	groupConsumerOpts = append(groupConsumerOpts, extraGroupConsumerOpts...)

	return &Client{
		cfg:                cfg,
		directConsumerOpts: directConsumerOpts,
		groupConsumerOpts:  groupConsumerOpts,
		producer:           producer,
		subscriptions:      make(map[string]*kafkaSubscription),
		closeCh:            make(chan struct{}),
	}, nil
}

func splitKafkaOptions(opts []kgo.Opt) (producerOpts, directConsumerOpts, groupConsumerOpts []kgo.Opt) {
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		_, isProducer := opt.(kgo.ProducerOpt)
		_, isConsumer := opt.(kgo.ConsumerOpt)
		_, isGroup := opt.(kgo.GroupOpt)

		switch {
		case isProducer && !isConsumer && !isGroup:
			producerOpts = append(producerOpts, opt)
		case isConsumer && !isProducer && !isGroup:
			directConsumerOpts = append(directConsumerOpts, opt)
			groupConsumerOpts = append(groupConsumerOpts, opt)
		case isGroup && !isProducer:
			groupConsumerOpts = append(groupConsumerOpts, opt)
		default:
			producerOpts = append(producerOpts, opt)
			if !isGroup {
				directConsumerOpts = append(directConsumerOpts, opt)
			}
			groupConsumerOpts = append(groupConsumerOpts, opt)
		}
	}
	return producerOpts, directConsumerOpts, groupConsumerOpts
}

// Subscribe subscribes to topicName and returns a channel delivering received
// message payloads.
//
// In broadcast mode (default), Subscribe resolves the current log-end offsets
// across all topic partitions before returning so that pre-existing records are
// not replayed and messages published immediately after Subscribe returns are
// never missed. In consumer group mode (WithConsumerGroup), Subscribe waits for
// group assignment and initial fetch readiness within ctx.
//
// The provided ctx governs setup and readiness synchronization only; the
// background receive loop runs until Unsubscribe or Close is called.
func (c *Client) Subscribe(ctx context.Context, topicName string) (<-chan []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topicName == "" {
		return nil, ErrInvalidTopic
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	existing := c.subscriptions[topicName]
	delete(c.subscriptions, topicName)
	c.mu.Unlock()

	if existing != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
		c.stopSubscription(cleanupCtx, existing, true)
		cleanupCancel()
	}

	partLeaders, err := c.fetchTopicPartitions(ctx, topicName)
	if err != nil {
		return nil, err
	}

	var (
		subOpts   []kgo.Opt
		readiness *groupReadiness
	)

	recvCtx, cancel := context.WithCancel(context.Background())

	if c.cfg.consumerGroup == "" {
		partOffsets, err := c.fetchEndOffsets(ctx, topicName, partLeaders)
		if err != nil {
			cancel()
			return nil, err
		}
		subOpts = slices.Clone(c.directConsumerOpts)
		subOpts = append(subOpts,
			kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{
				topicName: partOffsets,
			}),
			kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
		)
	} else {
		readiness = newGroupReadiness(topicName, c.cfg.consumerGroup, c.producer, recvCtx, partLeaders)
		subOpts = slices.Clone(c.groupConsumerOpts)
		subOpts = append(subOpts,
			kgo.ConsumerGroup(c.cfg.consumerGroup),
			kgo.ConsumeTopics(topicName),
			kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
			kgo.OnPartitionsAssigned(func(_ context.Context, _ *kgo.Client, assigned map[string][]int32) {
				readiness.onAssigned(assigned)
			}),
			kgo.WithHooks(readiness),
		)
	}

	var subClient *kgo.Client
	if c.cfg.consumerGroup != "" {
		markedOpts := append(slices.Clone(subOpts), kgo.AutoCommitMarks())
		subClient, err = kgo.NewClient(markedOpts...)
		if err != nil {
			// If the caller explicitly passed kgo.DisableAutoCommit() or
			// kgo.GreedyAutoCommit() via WithKafkaOptions, adding
			// kgo.AutoCommitMarks() is rejected by kgo validation; fall back to
			// the caller's configured commit mode.
			if fallbackClient, fallbackErr := kgo.NewClient(subOpts...); fallbackErr == nil {
				subClient = fallbackClient
				err = nil
			}
		}
	} else {
		subClient, err = kgo.NewClient(subOpts...)
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create kafka consumer client: %w", err)
	}

	ch := make(chan []byte, c.cfg.channelBuffer)
	entry := &kafkaSubscription{
		subClient: subClient,
		cancel:    cancel,
	}
	isGroup := c.cfg.consumerGroup != ""

	entry.wg.Go(func() {
		defer close(ch)
		defer subClient.AllowRebalance()
		for {
			if recvCtx.Err() != nil {
				return
			}
			fetches := subClient.PollFetches(recvCtx)
			if fetches.IsClientClosed() || recvCtx.Err() != nil {
				subClient.AllowRebalance()
				return
			}
			for rec := range fetches.RecordsAll() {
				select {
				case ch <- rec.Value:
					if isGroup {
						entry.markMu.Lock()
						if entry.armed {
							subClient.MarkCommitRecords(rec)
						} else {
							entry.pendingMarks = append(entry.pendingMarks, rec)
						}
						entry.markMu.Unlock()
					}
				case <-recvCtx.Done():
					subClient.AllowRebalance()
					return
				}
			}
			retryWait := fetches.Empty() && len(fetches.Errors()) > 0
			subClient.AllowRebalance()
			if retryWait {
				timer := time.NewTimer(receiveRetryDelay)
				select {
				case <-timer.C:
				case <-recvCtx.Done():
					timer.Stop()
					return
				}
			}
		}
	})

	if readiness != nil {
		select {
		case <-readiness.readyCh:
		case <-c.closeCh:
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
			c.stopSubscription(cleanupCtx, entry, false)
			cleanupCancel()
			return nil, ErrClosed
		case <-ctx.Done():
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
			c.stopSubscription(cleanupCtx, entry, false)
			cleanupCancel()
			return nil, ctx.Err()
		}
	}

	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
			c.stopSubscription(cleanupCtx, entry, false)
			cleanupCancel()
			return nil, ErrClosed
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			c.mu.Unlock()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
			c.stopSubscription(cleanupCtx, entry, false)
			cleanupCancel()
			return nil, ctxErr
		}
		if prev := c.subscriptions[topicName]; prev != nil {
			delete(c.subscriptions, topicName)
			c.mu.Unlock()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
			c.stopSubscription(cleanupCtx, prev, true)
			cleanupCancel()
			continue
		}
		c.subscriptions[topicName] = entry
		if isGroup {
			entry.markMu.Lock()
			entry.armed = true
			if len(entry.pendingMarks) > 0 {
				subClient.MarkCommitRecords(entry.pendingMarks...)
				entry.pendingMarks = nil
			}
			entry.markMu.Unlock()
		}
		c.mu.Unlock()
		break
	}

	return ch, nil
}

// Publish sends message to topicName synchronously.
func (c *Client) Publish(ctx context.Context, topicName string, message []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if topicName == "" {
		return ErrInvalidTopic
	}

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return ErrClosed
	}
	producer := c.producer
	c.mu.RUnlock()

	err := producer.ProduceSync(ctx, &kgo.Record{
		Topic: topicName,
		Value: message,
	}).FirstErr()
	if errors.Is(err, kgo.ErrClientClosed) || (err != nil && c.isClosed()) {
		return ErrClosed
	}
	return err
}

// Unsubscribe stops receiving messages for topicName, closes the subscription's
// channel, and shuts down its consumer client. Calling Unsubscribe for an
// unknown topic is a no-op and returns nil.
func (c *Client) Unsubscribe(ctx context.Context, topicName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	entry, ok := c.subscriptions[topicName]
	if !ok {
		c.mu.Unlock()
		return nil
	}
	delete(c.subscriptions, topicName)
	c.mu.Unlock()

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cleanupCancel()
	c.stopSubscription(cleanupCtx, entry, true)
	return nil
}

// Close flushes buffered producer records, closes the producer client, and
// stops all active subscriptions cleanly. Multiple calls are safe and
// idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.closeCh)
	subs := c.subscriptions
	c.subscriptions = nil
	producer := c.producer
	c.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cancel()

	var firstErr error
	if producer != nil {
		if err := producer.Flush(cleanupCtx); err != nil {
			firstErr = err
		}
		producer.Close()
	}

	for _, entry := range subs {
		c.stopSubscription(cleanupCtx, entry, true)
	}

	return firstErr
}

func (c *Client) isClosed() bool {
	select {
	case <-c.closeCh:
		return true
	default:
		return false
	}
}

func (c *Client) stopSubscription(ctx context.Context, entry *kafkaSubscription, commit bool) {
	entry.cancel()
	entry.wg.Wait()
	entry.subClient.AllowRebalance()

	if !commit {
		entry.markMu.Lock()
		entry.pendingMarks = nil
		entry.markMu.Unlock()
	}

	if c.cfg.consumerGroup != "" {
		if commit {
			if vals := entry.subClient.OptValues(kgo.AutoCommitMarks); len(vals) > 0 {
				if marks, ok := vals[0].(bool); ok && marks {
					_ = entry.subClient.CommitMarkedOffsets(ctx)
				} else if greedyVals := entry.subClient.OptValues(kgo.GreedyAutoCommit); len(greedyVals) > 0 {
					if greedy, ok := greedyVals[0].(bool); ok && greedy {
						_ = entry.subClient.CommitUncommittedOffsets(ctx)
					}
				}
			}
		}
		_ = entry.subClient.LeaveGroupContext(ctx)
	}
	entry.subClient.CloseAllowingRebalance()
}

func (c *Client) fetchTopicPartitions(ctx context.Context, topicName string) (map[int32]int32, error) {
	req := kmsg.NewPtrMetadataRequest()
	req.AllowAutoTopicCreation = c.cfg.autoCreateTopics
	topicReq := kmsg.NewMetadataRequestTopic()
	topicReq.Topic = &topicName
	req.Topics = []kmsg.MetadataRequestTopic{topicReq}

	for {
		resp, err := c.producer.Request(ctx, req)
		if err != nil {
			if errors.Is(err, kgo.ErrClientClosed) || c.isClosed() {
				return nil, ErrClosed
			}
			return nil, err
		}
		metaResp, ok := resp.(*kmsg.MetadataResponse)
		if !ok || len(metaResp.Topics) == 0 {
			return nil, fmt.Errorf("missing metadata response for topic %q", topicName)
		}

		t := metaResp.Topics[0]
		if t.ErrorCode != 0 {
			kerrVal := kerr.ErrorForCode(t.ErrorCode)
			retryable := t.ErrorCode == kerr.LeaderNotAvailable.Code ||
				(c.cfg.autoCreateTopics && t.ErrorCode == kerr.UnknownTopicOrPartition.Code)
			if !retryable {
				return nil, kerrVal
			}
		} else if len(t.Partitions) > 0 {
			leaders := make(map[int32]int32, len(t.Partitions))
			allLeadersReady := true
			for _, p := range t.Partitions {
				if p.ErrorCode != 0 || p.Leader < 0 {
					allLeadersReady = false
					break
				}
				leaders[p.Partition] = p.Leader
			}
			if allLeadersReady {
				return leaders, nil
			}
		}

		timer := time.NewTimer(metadataRetryDelay)
		select {
		case <-timer.C:
		case <-c.closeCh:
			timer.Stop()
			return nil, ErrClosed
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

func (c *Client) fetchEndOffsets(ctx context.Context, topicName string, partLeaders map[int32]int32) (map[int32]kgo.Offset, error) {
	partitions := make([]int32, 0, len(partLeaders))
	for p := range partLeaders {
		partitions = append(partitions, p)
	}

	for {
		req := kmsg.NewPtrListOffsetsRequest()
		req.ReplicaID = -1
		topicReq := kmsg.NewListOffsetsRequestTopic()
		topicReq.Topic = topicName
		for _, p := range partitions {
			partReq := kmsg.NewListOffsetsRequestTopicPartition()
			partReq.Partition = p
			partReq.Timestamp = -1
			topicReq.Partitions = append(topicReq.Partitions, partReq)
		}
		req.Topics = append(req.Topics, topicReq)

		shards := c.producer.RequestSharded(ctx, req)
		if len(shards) == 0 {
			if c.isClosed() {
				return nil, ErrClosed
			}
			return nil, fmt.Errorf("no brokers available to list offsets for topic %q", topicName)
		}

		offsets := make(map[int32]kgo.Offset, len(partitions))
		retry := false
		for _, sh := range shards {
			if sh.Err != nil {
				if errors.Is(sh.Err, kgo.ErrClientClosed) || c.isClosed() {
					return nil, ErrClosed
				}
				return nil, sh.Err
			}
			resp, ok := sh.Resp.(*kmsg.ListOffsetsResponse)
			if !ok {
				return nil, fmt.Errorf("unexpected list offsets response type %T", sh.Resp)
			}
			for _, t := range resp.Topics {
				for _, p := range t.Partitions {
					if p.ErrorCode != 0 {
						kerrVal := kerr.ErrorForCode(p.ErrorCode)
						if kerr.IsRetriable(kerrVal) {
							retry = true
							break
						}
						return nil, kerrVal
					}
					off := kgo.NewOffset().At(p.Offset)
					if p.LeaderEpoch >= 0 {
						off = off.WithEpoch(p.LeaderEpoch)
					}
					offsets[p.Partition] = off
				}
				if retry {
					break
				}
			}
			if retry {
				break
			}
		}

		if !retry && len(offsets) == len(partitions) {
			return offsets, nil
		}

		timer := time.NewTimer(metadataRetryDelay)
		select {
		case <-timer.C:
		case <-c.closeCh:
			timer.Stop()
			return nil, ErrClosed
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

type groupReadiness struct {
	topic          string
	group          string
	producer       *kgo.Client
	stopCtx        context.Context
	partLeaders    map[int32]int32
	readyCh        chan struct{}
	readyOnce      sync.Once
	mu             sync.Mutex
	assignedOnce   bool
	neededLeaders  map[int32]struct{}
	fetchedBrokers map[int32]struct{}
}

func newGroupReadiness(
	topic string,
	group string,
	producer *kgo.Client,
	stopCtx context.Context,
	partLeaders map[int32]int32,
) *groupReadiness {
	return &groupReadiness{
		topic:          topic,
		group:          group,
		producer:       producer,
		stopCtx:        stopCtx,
		partLeaders:    partLeaders,
		readyCh:        make(chan struct{}),
		neededLeaders:  make(map[int32]struct{}),
		fetchedBrokers: make(map[int32]struct{}),
	}
}

func (g *groupReadiness) markReady() {
	g.readyOnce.Do(func() {
		close(g.readyCh)
	})
}

func (g *groupReadiness) onAssigned(assigned map[string][]int32) {
	parts := assigned[g.topic]
	if len(parts) == 0 {
		g.mu.Lock()
		alreadyAssigned := g.assignedOnce
		g.mu.Unlock()
		if alreadyAssigned {
			return
		}
		// Only mark a 0-partition member ready as a standby if all topic
		// partitions are already assigned to other members in a stable group.
		// Under CooperativeStickyBalancer Round 1, partitions being revoked
		// from an existing member are temporarily unassigned until Round 2.
		if g.isStandbyConfirmed() {
			g.mu.Lock()
			if !g.assignedOnce {
				g.assignedOnce = true
				g.markReady()
			}
			g.mu.Unlock()
		}
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.assignedOnce = true
	clear(g.neededLeaders)
	for _, p := range parts {
		if leader, ok := g.partLeaders[p]; ok {
			g.neededLeaders[leader] = struct{}{}
		}
	}
	g.checkReadyLocked()
}

func (g *groupReadiness) isStandbyConfirmed() bool {
	if g.producer == nil || g.stopCtx == nil || g.stopCtx.Err() != nil {
		return false
	}

	reqCtx, cancel := context.WithTimeout(g.stopCtx, describeGroupTimeout)
	defer cancel()

	for attempt := range describeGroupMaxTries {
		if attempt > 0 {
			timer := time.NewTimer(describeGroupRetryWait)
			select {
			case <-timer.C:
			case <-reqCtx.Done():
				timer.Stop()
				return false
			}
		}

		req := kmsg.NewPtrDescribeGroupsRequest()
		req.Groups = []string{g.group}

		resp, err := g.producer.Request(reqCtx, req)
		if err != nil {
			if reqCtx.Err() != nil || errors.Is(err, kgo.ErrClientClosed) {
				return false
			}
			continue
		}

		descResp, ok := resp.(*kmsg.DescribeGroupsResponse)
		if !ok {
			return false
		}

		var target *kmsg.DescribeGroupsResponseGroup
		for i := range descResp.Groups {
			if descResp.Groups[i].Group == g.group {
				target = &descResp.Groups[i]
				break
			}
		}
		if target == nil {
			continue
		}

		if target.ErrorCode != 0 {
			if target.ErrorCode == kerr.GroupIDNotFound.Code {
				if confirmed, retry := g.isStandbyConfirmed848(reqCtx); !retry {
					return confirmed
				}
				continue
			}
			if kerr.IsRetriable(kerr.ErrorForCode(target.ErrorCode)) {
				continue
			}
			return false
		}

		if target.State != "Stable" {
			if isTransientGroupState(target.State) {
				continue
			}
			return false
		}

		assignedParts := make(map[int32]struct{}, len(g.partLeaders))
		for _, m := range target.Members {
			if len(m.MemberAssignment) == 0 {
				continue
			}
			parsed, parseErr := kgo.ParseConsumerSyncAssignment(m.MemberAssignment)
			if parseErr != nil {
				continue
			}
			for _, p := range parsed[g.topic] {
				if _, known := g.partLeaders[p]; known {
					assignedParts[p] = struct{}{}
				}
			}
		}
		return len(assignedParts) >= len(g.partLeaders)
	}

	return false
}

func (g *groupReadiness) isStandbyConfirmed848(ctx context.Context) (confirmed, retry bool) {
	req := kmsg.NewPtrConsumerGroupDescribeRequest()
	req.Groups = []string{g.group}

	resp, err := g.producer.Request(ctx, req)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, kgo.ErrClientClosed) {
			return false, false
		}
		return false, true
	}

	descResp, ok := resp.(*kmsg.ConsumerGroupDescribeResponse)
	if !ok {
		return false, false
	}

	var target *kmsg.ConsumerGroupDescribeResponseGroup
	for i := range descResp.Groups {
		if descResp.Groups[i].Group == g.group {
			target = &descResp.Groups[i]
			break
		}
	}
	if target == nil {
		return false, true
	}

	if target.ErrorCode != 0 {
		return false, kerr.IsRetriable(kerr.ErrorForCode(target.ErrorCode))
	}

	if target.State != "Stable" {
		return false, isTransientGroupState(target.State)
	}

	assignedParts := make(map[int32]struct{}, len(g.partLeaders))
	for _, m := range target.Members {
		for _, tp := range m.Assignment.TopicPartitions {
			if tp.Topic != g.topic {
				continue
			}
			for _, p := range tp.Partitions {
				if _, known := g.partLeaders[p]; known {
					assignedParts[p] = struct{}{}
				}
			}
		}
	}
	return len(assignedParts) >= len(g.partLeaders), false
}

func isTransientGroupState(state string) bool {
	switch state {
	case "PreparingRebalance", "CompletingRebalance", "Assigning", "Reconciling":
		return true
	default:
		return false
	}
}

func (g *groupReadiness) OnBrokerWrite(meta kgo.BrokerMetadata, key int16, _ int, _, _ time.Duration, err error) {
	if key != kafkaFetchAPIKey || err != nil {
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	g.fetchedBrokers[meta.NodeID] = struct{}{}
	g.checkReadyLocked()
}

func (g *groupReadiness) checkReadyLocked() {
	if !g.assignedOnce {
		return
	}
	if len(g.neededLeaders) == 0 {
		if len(g.fetchedBrokers) > 0 {
			g.markReady()
		}
		return
	}
	for leader := range g.neededLeaders {
		if _, ok := g.fetchedBrokers[leader]; !ok {
			return
		}
	}
	g.markReady()
}
