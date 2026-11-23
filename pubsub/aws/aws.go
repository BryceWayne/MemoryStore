package aws

import (
	"context"
	"encoding/base64"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const (
	defaultCleanupTimeout = 10 * time.Second
	initialRetryDelay     = 250 * time.Millisecond
	maxRetryDelay         = 2 * time.Second
	maxSQSQueueNameLen    = 80
	maxQueueSlugLen       = 36
	encodingAttributeKey  = "memorystore_encoding"
	encodingBase64Value   = "base64"
)

// Common errors returned by the AWS SNS + SQS Pub/Sub client.
var (
	ErrInvalidTopic    = errors.New("invalid topic")
	ErrClosed          = errors.New("aws pubsub client is closed")
	ErrInvalidQueueURL = errors.New("invalid sqs queue URL")
	ErrInvalidTopicARN = errors.New("invalid sns topic ARN")
	ErrMissingQueueURL = errors.New("queue URL is required when auto-management is disabled")
	ErrMissingTopicARN = errors.New("topic ARN is required when auto-management is disabled")
	ErrMissingRegion   = errors.New("aws region or custom endpoint is required")
)

// Internal minimal interfaces around the AWS SDK to enable thorough offline
// unit testing without requiring live AWS SNS or SQS endpoints.
type snsClient interface {
	CreateTopic(ctx context.Context, params *sns.CreateTopicInput, optFns ...func(*sns.Options)) (*sns.CreateTopicOutput, error)
	Publish(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error)
	Subscribe(ctx context.Context, params *sns.SubscribeInput, optFns ...func(*sns.Options)) (*sns.SubscribeOutput, error)
	Unsubscribe(ctx context.Context, params *sns.UnsubscribeInput, optFns ...func(*sns.Options)) (*sns.UnsubscribeOutput, error)
}

type sqsClient interface {
	CreateQueue(ctx context.Context, params *sqs.CreateQueueInput, optFns ...func(*sqs.Options)) (*sqs.CreateQueueOutput, error)
	GetQueueAttributes(ctx context.Context, params *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	SetQueueAttributes(ctx context.Context, params *sqs.SetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.SetQueueAttributesOutput, error)
	DeleteQueue(ctx context.Context, params *sqs.DeleteQueueInput, optFns ...func(*sqs.Options)) (*sqs.DeleteQueueOutput, error)
	ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// Client implements memorystore.PubSubClient using AWS SNS for publishing and
// AWS SQS long-polling for subscribing.
type Client struct {
	sns           snsClient
	sqs           sqsClient
	cfg           config
	mu            sync.RWMutex
	subscriptions map[string]*awsSubscription
	topicARNs     map[string]string
	closed        bool
}

type awsSubscription struct {
	topic       string
	topicARN    string
	queueURL    string
	subARN      string
	autoManaged bool
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// NewClient creates a new AWS SNS + SQS Pub/Sub client from an aws.Config and
// functional options. Either a Region or a custom BaseEndpoint must be
// configured on the resolved SNS and SQS client options.
func NewClient(cfg aws.Config, opts ...Option) (*Client, error) {
	cCfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cCfg)
		}
	}

	if err := validateConfig(cCfg); err != nil {
		return nil, err
	}

	snsCli := sns.NewFromConfig(cfg, cCfg.snsOptions...)
	sqsCli := sqs.NewFromConfig(cfg, cCfg.sqsOptions...)

	snsOpts := snsCli.Options()
	sqsOpts := sqsCli.Options()
	if !hasRegionOrEndpoint(snsOpts.Region, snsOpts.BaseEndpoint) ||
		!hasRegionOrEndpoint(sqsOpts.Region, sqsOpts.BaseEndpoint) {
		return nil, ErrMissingRegion
	}

	return newClientWithBackends(snsCli, sqsCli, cCfg)
}

func newClientWithBackends(snsBackend snsClient, sqsBackend sqsClient, cfg config) (*Client, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if snsBackend == nil || sqsBackend == nil {
		return nil, errors.New("sns and sqs backends are required")
	}
	return &Client{
		sns:           snsBackend,
		sqs:           sqsBackend,
		cfg:           cfg,
		subscriptions: make(map[string]*awsSubscription),
		topicARNs:     make(map[string]string),
	}, nil
}

func hasRegionOrEndpoint(region string, baseEndpoint *string) bool {
	return region != "" || (baseEndpoint != nil && *baseEndpoint != "")
}

func validateConfig(cfg config) error {
	if cfg.queueURL != "" {
		u, err := url.ParseRequestURI(cfg.queueURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: %q", ErrInvalidQueueURL, cfg.queueURL)
		}
	}
	if cfg.topicARN != "" {
		parsed, err := arn.Parse(cfg.topicARN)
		if err != nil || parsed.Service != "sns" || parsed.Resource == "" {
			return fmt.Errorf("%w: %q", ErrInvalidTopicARN, cfg.topicARN)
		}
	}
	return nil
}

func (c *Client) resolveTopicARN(ctx context.Context, topic string) (string, error) {
	if arn.IsARN(topic) {
		return topic, nil
	}
	if c.cfg.topicARN != "" {
		return c.cfg.topicARN, nil
	}

	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return "", ErrClosed
	}
	if cached, ok := c.topicARNs[topic]; ok {
		c.mu.RUnlock()
		return cached, nil
	}
	c.mu.RUnlock()

	if !c.cfg.shouldAutoManage() {
		return "", ErrMissingTopicARN
	}

	out, err := c.sns.CreateTopic(ctx, &sns.CreateTopicInput{
		Name: aws.String(topic),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create or resolve SNS topic %q: %w", topic, err)
	}
	topicARN := aws.ToString(out.TopicArn)
	if topicARN == "" {
		return "", fmt.Errorf("SNS CreateTopic returned empty TopicArn for %q", topic)
	}

	c.mu.Lock()
	if c.closed || c.topicARNs == nil {
		c.mu.Unlock()
		return "", ErrClosed
	}
	c.topicARNs[topic] = topicARN
	c.mu.Unlock()

	return topicARN, nil
}

// Subscribe subscribes to topic and returns a channel delivering received
// message bodies. In auto-managed mode (the default), Subscribe resolves or
// creates the SNS topic, creates a unique temporary SQS queue with an SNS
// delivery policy, and subscribes the queue to the topic. In pre-provisioned
// mode, Subscribe polls the configured WithQueueURL directly.
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
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
		_ = c.stopSubscription(cleanupCtx, existing)
		cancel()
	}

	autoManaged := c.cfg.shouldAutoManage()
	var (
		topicARN string
		queueURL string
		subARN   string
	)

	if !autoManaged {
		if c.cfg.queueURL == "" {
			return nil, ErrMissingQueueURL
		}
		queueURL = c.cfg.queueURL
	} else {
		var err error
		topicARN, err = c.resolveTopicARN(ctx, topic)
		if err != nil {
			return nil, err
		}

		queueName := ephemeralQueueName(topic)
		createOut, err := c.sqs.CreateQueue(ctx, &sqs.CreateQueueInput{
			QueueName: aws.String(queueName),
			Attributes: map[string]string{
				string(sqstypes.QueueAttributeNameReceiveMessageWaitTimeSeconds): strconv.Itoa(int(c.cfg.waitTimeSeconds)),
			},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create SQS queue: %w", err)
		}
		queueURL = aws.ToString(createOut.QueueUrl)
		if queueURL == "" {
			return nil, errors.New("SQS CreateQueue returned empty QueueUrl")
		}

		rollback := func() {
			c.cleanupEphemeralResources(queueURL, subARN)
		}

		attrOut, err := c.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl:       aws.String(queueURL),
			AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
		})
		if err != nil {
			rollback()
			return nil, fmt.Errorf("failed to get SQS queue ARN: %w", err)
		}
		queueARN := attrOut.Attributes[string(sqstypes.QueueAttributeNameQueueArn)]
		if queueARN == "" {
			rollback()
			return nil, errors.New("SQS GetQueueAttributes returned empty QueueArn")
		}

		policy, err := buildQueuePolicy(queueARN, topicARN)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("failed to build SQS queue policy: %w", err)
		}

		if _, err := c.sqs.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
			QueueUrl: aws.String(queueURL),
			Attributes: map[string]string{
				string(sqstypes.QueueAttributeNamePolicy): policy,
			},
		}); err != nil {
			rollback()
			return nil, fmt.Errorf("failed to set SQS queue policy: %w", err)
		}

		subOut, err := c.sns.Subscribe(ctx, &sns.SubscribeInput{
			TopicArn:              aws.String(topicARN),
			Protocol:              aws.String("sqs"),
			Endpoint:              aws.String(queueARN),
			ReturnSubscriptionArn: true,
			Attributes: map[string]string{
				"RawMessageDelivery": "true",
			},
		})
		if err != nil {
			rollback()
			return nil, fmt.Errorf("failed to subscribe SQS queue to SNS topic: %w", err)
		}
		subARN = aws.ToString(subOut.SubscriptionArn)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		if autoManaged {
			c.cleanupEphemeralResources(queueURL, subARN)
		}
		return nil, ErrClosed
	}

	prev := c.subscriptions[topic]
	ch := make(chan []byte, c.cfg.channelBuffer)
	recvCtx, cancel := context.WithCancel(context.Background())
	entry := &awsSubscription{
		topic:       topic,
		topicARN:    topicARN,
		queueURL:    queueURL,
		subARN:      subARN,
		autoManaged: autoManaged,
		cancel:      cancel,
	}

	waitSecs := c.cfg.waitTimeSeconds
	maxMsgs := c.cfg.maxNumberOfMessages
	entry.wg.Go(func() {
		defer close(ch)
		c.receiveLoop(recvCtx, queueURL, waitSecs, maxMsgs, ch)
	})

	c.subscriptions[topic] = entry
	c.mu.Unlock()

	if prev != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
		_ = c.stopSubscription(cleanupCtx, prev)
		cancel()
	}

	return ch, nil
}

func (c *Client) receiveLoop(ctx context.Context, queueURL string, waitSecs, maxMsgs int32, ch chan<- []byte) {
	retryDelay := initialRetryDelay
	for {
		if ctx.Err() != nil {
			return
		}
		out, err := c.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(queueURL),
			MaxNumberOfMessages:   maxMsgs,
			WaitTimeSeconds:       waitSecs,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			timer := time.NewTimer(retryDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
			retryDelay = min(retryDelay*2, maxRetryDelay)
			continue
		}
		retryDelay = initialRetryDelay

		for _, msg := range out.Messages {
			payload := extractPayload(msg)
			select {
			case ch <- payload:
				if handle := aws.ToString(msg.ReceiptHandle); handle != "" {
					_, _ = c.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{
						QueueUrl:      aws.String(queueURL),
						ReceiptHandle: aws.String(handle),
					})
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

// Publish sends message to the SNS topic resolved for topic. Valid UTF-8
// payloads are published directly; non-UTF-8 payloads are base64-encoded with
// MessageAttributes["memorystore_encoding"] = "base64".
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

	topicARN, err := c.resolveTopicARN(ctx, topic)
	if err != nil {
		return err
	}

	input := &sns.PublishInput{
		TopicArn: aws.String(topicARN),
	}
	if utf8.Valid(message) {
		input.Message = aws.String(string(message))
	} else {
		input.Message = aws.String(base64.StdEncoding.EncodeToString(message))
		input.MessageAttributes = map[string]snstypes.MessageAttributeValue{
			encodingAttributeKey: {
				DataType:    aws.String("String"),
				StringValue: aws.String(encodingBase64Value),
			},
		}
	}

	if _, err := c.sns.Publish(ctx, input); err != nil {
		return fmt.Errorf("failed to publish to SNS topic %q: %w", topicARN, err)
	}
	return nil
}

// Unsubscribe stops receiving messages for topic and, in auto-managed mode,
// unsubscribes from SNS and deletes the ephemeral SQS queue. Unsubscribing from
// a topic that is not currently subscribed is a no-op returning nil.
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

// Close stops all active subscriptions, waits for receive goroutines to exit
// and close their channels, and deletes any auto-managed SNS subscriptions and
// ephemeral SQS queues. Multiple calls are safe and idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	subs := c.subscriptions
	c.subscriptions = nil
	c.topicARNs = nil
	c.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cancel()

	var errs []error
	for _, entry := range subs {
		if err := c.stopSubscription(cleanupCtx, entry); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Client) stopSubscription(ctx context.Context, entry *awsSubscription) error {
	entry.cancel()
	entry.wg.Wait()

	if !entry.autoManaged {
		return nil
	}

	return c.deleteManagedResources(ctx, entry.queueURL, entry.subARN)
}

func (c *Client) cleanupEphemeralResources(queueURL, subARN string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cancel()
	_ = c.deleteManagedResources(cleanupCtx, queueURL, subARN)
}

func (c *Client) deleteManagedResources(ctx context.Context, queueURL, subARN string) error {
	var errs []error
	if subARN != "" && subARN != "pending confirmation" {
		if _, err := c.sns.Unsubscribe(ctx, &sns.UnsubscribeInput{
			SubscriptionArn: aws.String(subARN),
		}); err != nil && !isSNSNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to unsubscribe SNS subscription %q: %w", subARN, err))
		}
	}
	if queueURL != "" {
		if _, err := c.sqs.DeleteQueue(ctx, &sqs.DeleteQueueInput{
			QueueUrl: aws.String(queueURL),
		}); err != nil && !isSQSQueueNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to delete SQS queue %q: %w", queueURL, err))
		}
	}
	return errors.Join(errs...)
}

func isSNSNotFound(err error) bool {
	var notFound *snstypes.NotFoundException
	return errors.As(err, &notFound)
}

func isSQSQueueNotFound(err error) bool {
	var missing *sqstypes.QueueDoesNotExist
	return errors.As(err, &missing)
}

// ephemeralQueueName generates a valid SQS queue name (<= 80 chars,
// [a-zA-Z0-9_-]) incorporating a sanitized tail of topic and a fresh UUID.
func ephemeralQueueName(topic string) string {
	raw := topic
	if idx := strings.LastIndex(topic, ":"); idx >= 0 {
		raw = topic[idx+1:]
	}

	var b strings.Builder
	b.Grow(min(len(raw), maxQueueSlugLen))
	for i := 0; i < len(raw) && b.Len() < maxQueueSlugLen; i++ {
		ch := raw[i]
		isValid := (ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '_' || ch == '-'
		if isValid {
			b.WriteByte(ch)
		} else {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")

	id := uuid.New().String()
	var name string
	if slug == "" {
		name = "ms-" + id
	} else {
		name = "ms-" + slug + "-" + id
	}
	if len(name) > maxSQSQueueNameLen {
		name = name[:maxSQSQueueNameLen]
	}
	return name
}

type sqsPolicyDocument struct {
	Version   string               `json:"Version"`
	Statement []sqsPolicyStatement `json:"Statement"`
}

type sqsPolicyStatement struct {
	Sid       string             `json:"Sid"`
	Effect    string             `json:"Effect"`
	Principal map[string]string  `json:"Principal"`
	Action    string             `json:"Action"`
	Resource  string             `json:"Resource"`
	Condition sqsPolicyCondition `json:"Condition"`
}

type sqsPolicyCondition struct {
	ArnEquals map[string]string `json:"ArnEquals"`
}

func buildQueuePolicy(queueARN, topicARN string) (string, error) {
	doc := sqsPolicyDocument{
		Version: "2012-10-17",
		Statement: []sqsPolicyStatement{
			{
				Sid:       "AllowSNSPublish",
				Effect:    "Allow",
				Principal: map[string]string{"Service": "sns.amazonaws.com"},
				Action:    "sqs:SendMessage",
				Resource:  queueARN,
				Condition: sqsPolicyCondition{
					ArnEquals: map[string]string{"aws:SourceArn": topicARN},
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type snsNotificationEnvelope struct {
	Type              string                     `json:"Type"`
	MessageID         string                     `json:"MessageId"`
	TopicARN          string                     `json:"TopicArn"`
	Message           *string                    `json:"Message"`
	MessageAttributes map[string]snsEnvelopeAttr `json:"MessageAttributes"`
}

type snsEnvelopeAttr struct {
	Type  string `json:"Type"`
	Value string `json:"Value"`
}

func extractPayload(msg sqstypes.Message) []byte {
	body := aws.ToString(msg.Body)
	isBase64 := false
	if attr, ok := msg.MessageAttributes[encodingAttributeKey]; ok &&
		aws.ToString(attr.StringValue) == encodingBase64Value {
		isBase64 = true
	}

	if !isBase64 {
		var env snsNotificationEnvelope
		if err := json.Unmarshal([]byte(body), &env); err == nil &&
			env.Type == "Notification" &&
			env.Message != nil &&
			env.TopicARN != "" &&
			env.MessageID != "" {
			body = *env.Message
			if attr, ok := env.MessageAttributes[encodingAttributeKey]; ok &&
				attr.Value == encodingBase64Value {
				isBase64 = true
			}
		}
	}

	if isBase64 {
		if decoded, err := base64.StdEncoding.DecodeString(body); err == nil {
			return decoded
		}
	}
	return []byte(body)
}
