package azure

import (
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus/admin"
)

const (
	defaultChannelBuffer   = 100
	defaultMaxMessages     = 10
	defaultAutoDeleteIdle  = "P1D"
	maxSubscriptionNameLen = 50
)

type config struct {
	subscriptionName   string
	subscriptionNameFn func(topic string) string
	autoManage         *bool
	autoDeleteOnIdle   string
	receiveMode        azservicebus.ReceiveMode
	maxMessages        int
	channelBuffer      int
	clientOptions      *azservicebus.ClientOptions
	adminClientOptions *admin.ClientOptions
}

func defaultConfig() config {
	return config{
		autoDeleteOnIdle: defaultAutoDeleteIdle,
		receiveMode:      azservicebus.ReceiveModePeekLock,
		maxMessages:      defaultMaxMessages,
		channelBuffer:    defaultChannelBuffer,
	}
}

func (c config) shouldAutoManage() bool {
	if c.autoManage != nil {
		return *c.autoManage
	}
	// When a caller provides a static pre-provisioned subscription name,
	// default to non-managing mode so Data Sender/Receiver RBAC works out of the box.
	return c.subscriptionName == ""
}

// Option configures a Client created by NewClient or NewClientFromConnectionString.
type Option func(*config)

// WithSubscriptionName configures a pre-provisioned Azure Service Bus
// subscription name to attach to instead of creating ephemeral subscriptions.
// Unless overridden by WithAutoManage(true), setting a static subscription name
// disables dynamic admin create/delete calls so least-privilege RBAC roles work.
func WithSubscriptionName(name string) Option {
	return func(c *config) {
		c.subscriptionName = name
	}
}

// WithSubscriptionNameFunc configures a custom function that derives the
// Azure Service Bus subscription name for a given topic. Returned names must
// be non-empty and at most 50 characters (Azure Service Bus limit).
func WithSubscriptionNameFunc(fn func(topic string) string) Option {
	return func(c *config) {
		c.subscriptionNameFn = fn
	}
}

// WithAutoManage controls whether the Client uses azservicebus/admin to create
// missing topics/subscriptions on Subscribe and delete ephemeral subscriptions
// on Unsubscribe/Close.
func WithAutoManage(enabled bool) Option {
	return func(c *config) {
		c.autoManage = &enabled
	}
}

// WithAutoDeleteOnIdle sets the ISO 8601 idle duration (for example, "PT1H" or
// "P1D") applied when auto-creating ephemeral subscriptions. Defaults to "P1D".
func WithAutoDeleteOnIdle(iso8601Duration string) Option {
	return func(c *config) {
		if iso8601Duration != "" {
			c.autoDeleteOnIdle = iso8601Duration
		}
	}
}

// WithReceiveMode sets the Azure Service Bus receive mode
// (azservicebus.ReceiveModePeekLock or azservicebus.ReceiveModeReceiveAndDelete).
// Defaults to azservicebus.ReceiveModePeekLock.
func WithReceiveMode(mode azservicebus.ReceiveMode) Option {
	return func(c *config) {
		c.receiveMode = mode
	}
}

// WithMaxMessages sets the maximum number of messages fetched per
// ReceiveMessages call. Defaults to 10.
func WithMaxMessages(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxMessages = n
		}
	}
}

// WithChannelBuffer sets the buffer size of the channel returned by Subscribe.
// Defaults to 100.
func WithChannelBuffer(size int) Option {
	return func(c *config) {
		if size > 0 {
			c.channelBuffer = size
		}
	}
}

// WithClientOptions passes custom azservicebus.ClientOptions to the underlying
// Service Bus data client.
func WithClientOptions(opts *azservicebus.ClientOptions) Option {
	return func(c *config) {
		c.clientOptions = opts
	}
}

// WithAdminClientOptions passes custom admin.ClientOptions to the underlying
// Service Bus admin client (used when auto-management is enabled).
func WithAdminClientOptions(opts *admin.ClientOptions) Option {
	return func(c *config) {
		c.adminClientOptions = opts
	}
}
