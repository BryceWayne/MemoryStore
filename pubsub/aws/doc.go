// Package aws provides an AWS SNS + SQS Pub/Sub adapter backed by
// github.com/aws/aws-sdk-go-v2/service/sns and
// github.com/aws/aws-sdk-go-v2/service/sqs that satisfies
// memorystore.PubSubClient.
//
// Because memorystore.PubSubClient uses only standard library types, this
// package is maintained as an independent Go module so core MemoryStore users
// do not incur AWS SDK dependencies.
//
// # SNS + SQS Fan-Out and Payload Encoding
//
// Publish sends messages to an AWS SNS topic, and Subscribe long-polls an
// attached AWS SQS queue using ReceiveMessage and deletes each message via
// DeleteMessage only after it has been delivered to the subscriber channel.
//
// AWS SNS requires message bodies to be valid UTF-8 strings (up to 256 KiB).
// Valid UTF-8 payloads (including MemoryStore JSON invalidation events) are
// published verbatim with no extra attributes for interoperability with non-Go
// subscribers. Non-UTF-8 binary payloads are automatically base64-encoded with
// MessageAttributes["memorystore_encoding"] = "base64" on Publish and decoded
// back to the original byte slice on ReceiveMessage.
//
// # Subscription Lifecycle Modes
//
// Two operational modes are supported:
//
// Ephemeral auto-managed mode (default): Subscribe resolves or creates the
// SNS topic (CreateTopic), creates a unique temporary SQS queue
// ("ms-<slug>-<uuid>", within SQS's 80-character [a-zA-Z0-9_-] limit),
// attaches a queue Policy allowing sns.amazonaws.com to invoke sqs:SendMessage
// scoped by aws:SourceArn, and subscribes the queue to the SNS topic with
// RawMessageDelivery enabled. On Unsubscribe or Close, the adapter unsubscribes
// from SNS and deletes the ephemeral SQS queue.
//
// Pre-provisioned least-privilege IAM mode: Configure a pre-provisioned queue
// via WithQueueURL (which disables auto-management unless WithAutoManage(true)
// is explicitly set) and optionally WithTopicARN (or pass full SNS topic ARNs
// as the topic string), or call WithAutoManage(false). In this mode all topic,
// queue, policy, and subscription management calls are skipped.
//
// # RawMessageDelivery and SNS Envelopes
//
// Auto-managed subscriptions enable RawMessageDelivery ("true") so SQS receives
// the exact published body. In pre-provisioned mode, if an SQS subscription
// does not have RawMessageDelivery enabled, SNS wraps messages in a JSON
// notification envelope (Type == "Notification" with TopicArn, MessageId, and
// Message). The receive loop detects and unwraps standard SNS notification
// envelopes automatically while leaving ordinary JSON payloads untouched.
//
// # Required IAM Permissions
//
// Auto-managed mode requires:
//   - SNS: sns:CreateTopic, sns:Publish, sns:Subscribe, sns:Unsubscribe
//   - SQS: sqs:CreateQueue, sqs:GetQueueAttributes, sqs:SetQueueAttributes,
//     sqs:DeleteQueue, sqs:ReceiveMessage, sqs:DeleteMessage
//
// Pre-provisioned least-privilege mode requires only:
//   - Publisher: sns:Publish
//   - Subscriber: sqs:ReceiveMessage, sqs:DeleteMessage
//
// # Examples
//
// Default ephemeral auto-managed mode:
//
//	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
//	if err != nil {
//	    log.Fatal(err)
//	}
//	ps, err := aws.NewClient(cfg)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	store := memorystore.NewMemoryStoreWithConfig(memorystore.Config{
//	    PubSub: ps,
//	})
//	defer store.Stop()
//
// Pre-provisioned least-privilege IAM mode:
//
//	ps, err := aws.NewClient(
//	    cfg,
//	    aws.WithTopicARN("arn:aws:sns:us-east-1:123456789012:cache-invalidation"),
//	    aws.WithQueueURL("https://sqs.us-east-1.amazonaws.com/123456789012/pod-1-invalidation"),
//	)
//	if err != nil {
//	    log.Fatal(err)
//	}
package aws
