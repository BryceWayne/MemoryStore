// Package azure provides an Azure Service Bus adapter backed by
// github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus that satisfies
// memorystore.PubSubClient.
//
// Because memorystore.PubSubClient uses only standard library types, this
// package is maintained as an independent Go module so core MemoryStore users
// do not incur Azure SDK or AMQP dependencies.
//
// Azure Service Bus Topics and Subscriptions require the Standard or Premium
// Service Bus tier (the Basic tier supports queues only).
//
// # Authentication
//
// Use NewClient with any azcore.TokenCredential (for example,
// azidentity.NewDefaultAzureCredential for Managed Identity / Entra ID in
// production) or NewClientFromConnectionString for SAS connection strings and
// local development.
//
// # Subscription Lifecycle Modes
//
// By default, Subscribe creates a unique ephemeral subscription per instance
// ("ms-<uuid>", within Azure's 50-character subscription name limit) with
// AutoDeleteOnIdle set to 24 hours ("P1D"), and deletes it on Unsubscribe or
// Close. For least-privilege RBAC environments (Data Sender / Data Receiver
// without namespace management permissions), configure a pre-provisioned
// subscription using WithSubscriptionName or WithSubscriptionNameFunc together
// with WithAutoManage(false). Use WithLogger to capture structured warnings for
// transient receive, settlement, slow-consumer, and cleanup errors.
package azure
