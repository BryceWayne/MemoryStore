package aws

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func FuzzAWSExtractPayload(f *testing.F) {
	// Plain payloads
	f.Add("hello world", true, "", false)
	f.Add(`{"o":"node-1","k":["user:1"]}`, true, "plain", true)

	// Raw memorystore_encoding=base64 (valid & invalid)
	f.Add("aGVsbG8td29ybGQ=", true, encodingBase64Value, true)
	f.Add("AAEC/w==", true, encodingBase64Value, true)
	f.Add("", true, encodingBase64Value, true)
	f.Add("!!!not-valid-base64!!!", true, encodingBase64Value, true)

	// SNS Notification envelopes (plain & base64)
	f.Add(`{"Type":"Notification","MessageId":"m1","TopicArn":"arn:aws:sns:us-east-1:123456789012:t","Message":"plain-payload"}`, true, "", false)
	f.Add(`{"Type":"Notification","MessageId":"m2","TopicArn":"arn:aws:sns:us-east-1:123456789012:t","Message":"AQID/w==","MessageAttributes":{"memorystore_encoding":{"Type":"String","Value":"base64"}}}`, true, "", false)
	f.Add(`{"Type":"Notification","MessageId":"m3","TopicArn":"arn:aws:sns:us-east-1:123456789012:t","Message":"not-base64!!!","MessageAttributes":{"memorystore_encoding":{"Type":"String","Value":"base64"}}}`, true, "", false)

	// Partial / non-matching SNS envelopes
	f.Add(`{"Type":"Notification","Message":"missing-ids"}`, true, "", false)
	f.Add(`{"Type":"Notification","MessageId":"m4","Message":"missing-topic-arn"}`, true, "", false)
	f.Add(`{"Type":"SubscriptionConfirmation","MessageId":"m5","TopicArn":"arn:aws:sns:us-east-1:123456789012:t","Message":"confirm"}`, true, "", false)
	f.Add(`{"Type":"Notification","MessageId":"m6","TopicArn":"arn:aws:sns:us-east-1:123456789012:t","Message":null}`, true, "", false)
	f.Add(`{"Type":"Notification","MessageId":"m7","TopicArn":"arn:aws:sns:us-east-1:123456789012:t","Message":123}`, true, "", false)

	// Nil / empty bodies & malformed JSON
	f.Add("", false, "", false)
	f.Add("", false, encodingBase64Value, true)
	f.Add("", true, "", false)
	f.Add("{bad-json", true, "", false)
	f.Add("\xff\xfe\xfd", true, "", false)

	f.Fuzz(func(t *testing.T, body string, hasBody bool, attrEncoding string, hasAttr bool) {
		var msg sqstypes.Message
		if hasBody {
			msg.Body = aws.String(body)
		}
		if hasAttr {
			msg.MessageAttributes = map[string]sqstypes.MessageAttributeValue{
				encodingAttributeKey: {
					DataType:    aws.String("String"),
					StringValue: aws.String(attrEncoding),
				},
			}
		}

		got := extractPayload(msg)
		if got == nil {
			t.Fatal("extractPayload returned nil slice")
		}

		if hasAttr && attrEncoding == encodingBase64Value {
			rawBody := ""
			if hasBody {
				rawBody = body
			}
			if decoded, err := base64.StdEncoding.DecodeString(rawBody); err == nil {
				if !bytes.Equal(got, decoded) {
					t.Fatalf("got %x, want decoded %x", got, decoded)
				}
			} else if !bytes.Equal(got, []byte(rawBody)) {
				t.Fatalf("on invalid base64 got %x, want raw %x", got, []byte(rawBody))
			}
		}
	})
}
