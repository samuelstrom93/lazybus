// Package bus is lazybus's service layer: the domain types and interfaces the
// UI talks to. Azure SDK types never leave this package tree; the UI only sees
// what is declared here.
package bus

import (
	"context"
	"time"
)

// PageSize is the number of messages one Peek call returns at most.
const PageSize = 50

// Dead-letter Markers: application properties Service Bus attaches when it
// dead-letters a message. A repaired message never carries them.
const (
	MarkerDeadLetterReason           = "DeadLetterReason"
	MarkerDeadLetterErrorDescription = "DeadLetterErrorDescription"
)

// Namespace is one Service Bus namespace the user can open.
type Namespace struct {
	Name         string // short name, e.g. "sb-prod-weu"
	FQDN         string // e.g. "sb-prod-weu.servicebus.windows.net"
	Subscription string // Azure subscription it was discovered in; empty for connection-string entries
}

// EntityKind tells queues and topic subscriptions apart. Topics themselves
// are never listed: they have no dead-letter queue.
type EntityKind int

const (
	KindQueue EntityKind = iota
	KindSubscription
)

func (k EntityKind) String() string {
	switch k {
	case KindQueue:
		return "queue"
	case KindSubscription:
		return "subscription"
	}
	return "unknown"
}

// Entity is a queue or a topic subscription with its runtime counts.
type Entity struct {
	// Path is the queue name, or "topic/subscription" for a subscription.
	// Kind disambiguates: queue names may contain '/' too.
	Path            string
	Kind            EntityKind
	ActiveCount     int64
	DeadLetterCount int64
}

// SubQueue selects the active queue or the dead-letter queue of an entity.
type SubQueue int

const (
	DeadLetter SubQueue = iota
	Active
)

func (s SubQueue) String() string {
	if s == Active {
		return "Active"
	}
	return "DLQ"
}

// PropertyType is the wire type of an application property.
type PropertyType int

const (
	TypeString PropertyType = iota
	TypeInt
	TypeLong
	TypeDouble
	TypeBool
	TypeGUID
	TypeDateTime
)

func (t PropertyType) String() string {
	switch t {
	case TypeString:
		return "String"
	case TypeInt:
		return "Int"
	case TypeLong:
		return "Long"
	case TypeDouble:
		return "Double"
	case TypeBool:
		return "Bool"
	case TypeGUID:
		return "Guid"
	case TypeDateTime:
		return "DateTime"
	}
	return "Unknown"
}

// Property is one typed application property. Value holds the Go value that
// matches Type: string, int32, int64, float64, bool, string (Guid) or
// time.Time (DateTime).
type Property struct {
	Key   string
	Type  PropertyType
	Value any
}

// Message is one peeked message. Properties holds the application properties
// without the Dead-letter Markers; those are in DeadLetterReason and
// DeadLetterErrorDescription.
type Message struct {
	SequenceNumber int64
	EnqueuedTime   time.Time
	Body           []byte
	Properties     []Property

	MessageID        string
	CorrelationID    string
	Subject          string
	ContentType      string
	SessionID        string
	PartitionKey     string
	To               string
	ReplyTo          string
	ReplyToSessionID string
	TimeToLive       time.Duration
	DeliveryCount    uint32

	DeadLetterReason           string
	DeadLetterErrorDescription string
	DeadLetterSource           string
}

// PeekRequest asks for up to Max messages (PageSize when zero) starting at
// sequence number FromSequence, in sequence order.
type PeekRequest struct {
	Namespace    Namespace
	Entity       Entity
	SubQueue     SubQueue
	FromSequence int64
	Max          int
}

// Discovery lists the namespaces the user can open.
type Discovery interface {
	Namespaces(ctx context.Context) ([]Namespace, error)
}

// Entities lists the queues and topic subscriptions of a namespace, sorted
// by path.
type Entities interface {
	ListEntities(ctx context.Context, ns Namespace) ([]Entity, error)
}

// Peeker reads messages without locking them. Never emulated with
// peek-lock + abandon.
type Peeker interface {
	Peek(ctx context.Context, req PeekRequest) ([]Message, error)
}

// Browser is everything the UI needs for browsing.
type Browser interface {
	Discovery
	Entities
	Peeker
}
