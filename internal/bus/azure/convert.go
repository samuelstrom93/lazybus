package azure

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/go-amqp"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// toMessage converts a peeked SDK message. The Dead-letter Markers are
// taken out of the application properties into their own fields;
// properties are sorted by key so the display is stable.
func toMessage(m *azservicebus.ReceivedMessage) bus.Message {
	out := bus.Message{
		Body:                       m.Body,
		MessageID:                  m.MessageID,
		CorrelationID:              str(m.CorrelationID),
		Subject:                    str(m.Subject),
		ContentType:                str(m.ContentType),
		SessionID:                  str(m.SessionID),
		PartitionKey:               str(m.PartitionKey),
		To:                         str(m.To),
		ReplyTo:                    str(m.ReplyTo),
		ReplyToSessionID:           str(m.ReplyToSessionID),
		DeliveryCount:              m.DeliveryCount,
		DeadLetterReason:           str(m.DeadLetterReason),
		DeadLetterErrorDescription: str(m.DeadLetterErrorDescription),
		DeadLetterSource:           str(m.DeadLetterSource),
	}
	if m.SequenceNumber != nil {
		out.SequenceNumber = *m.SequenceNumber
	}
	if m.EnqueuedTime != nil {
		out.EnqueuedTime = *m.EnqueuedTime
	}
	if m.TimeToLive != nil {
		out.TimeToLive = *m.TimeToLive
	}
	for k, v := range m.ApplicationProperties {
		if k == bus.MarkerDeadLetterReason || k == bus.MarkerDeadLetterErrorDescription {
			continue
		}
		out.Properties = append(out.Properties, toProperty(k, v))
	}
	sort.Slice(out.Properties, func(i, j int) bool { return out.Properties[i].Key < out.Properties[j].Key })
	return out
}

// toProperty maps an AMQP application property value to a bus.Property.
// Values outside the seven Service Bus property types are shown as String.
func toProperty(key string, v any) bus.Property {
	p := bus.Property{Key: key}
	switch v := v.(type) {
	case string:
		p.Type, p.Value = bus.TypeString, v
	case int32:
		p.Type, p.Value = bus.TypeInt, v
	case int8:
		p.Type, p.Value = bus.TypeInt, int32(v)
	case int16:
		p.Type, p.Value = bus.TypeInt, int32(v)
	case uint8:
		p.Type, p.Value = bus.TypeInt, int32(v)
	case uint16:
		p.Type, p.Value = bus.TypeInt, int32(v)
	case int64:
		p.Type, p.Value = bus.TypeLong, v
	case int:
		p.Type, p.Value = bus.TypeLong, int64(v)
	case uint32:
		p.Type, p.Value = bus.TypeLong, int64(v)
	case uint64:
		if v <= math.MaxInt64 {
			p.Type, p.Value = bus.TypeLong, int64(v)
		} else {
			p.Type, p.Value = bus.TypeString, fmt.Sprint(v)
		}
	case float64:
		p.Type, p.Value = bus.TypeDouble, v
	case float32:
		p.Type, p.Value = bus.TypeDouble, float64(v)
	case bool:
		p.Type, p.Value = bus.TypeBool, v
	case amqp.UUID:
		p.Type, p.Value = bus.TypeGUID, v.String()
	case time.Time:
		p.Type, p.Value = bus.TypeDateTime, v
	case nil:
		p.Type = bus.TypeString
	default:
		p.Type, p.Value = bus.TypeString, fmt.Sprintf("%v", v)
	}
	return p
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
