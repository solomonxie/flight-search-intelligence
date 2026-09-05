// Package kafka is the thin plumbing between the agent loop's steps: each
// step is one small message, not a full state snapshot — the database
// (internal/catalog) stays the one source of truth for what a request's
// spec/round-history/status actually is; a message only ever says "go
// check this now." That keeps a message harmless to redeliver (reprocessing
// "check request X" is a no-op if X has nothing new to do) and keeps the
// message schema stable even as Spec/RoundRecord grow.
//
// Two topics chain into each other, one step producing the next:
//
//	agent-decisions {request_id} --agent-worker--> search-tasks {task_id} --collector--> agent-decisions {request_id} --> ...
//
// A worker that decides to finalize a request simply doesn't produce a
// next message — that's the whole "stop" signal, no separate mechanism
// needed. See cmd/agent-worker and cmd/collector for the two consumers,
// and internal/agents for the decision logic each one calls into.
package kafka

import (
	"context"
	"encoding/json"
	"fmt"

	kafkago "github.com/segmentio/kafka-go"
)

const (
	// TopicAgentDecisions carries DecisionTrigger — "this request is ready
	// for a decision." Consumed by cmd/agent-worker.
	TopicAgentDecisions = "agent-decisions"
	// TopicSearchTasks carries SearchTaskTrigger — "this task is ready to
	// run." Consumed by cmd/collector.
	TopicSearchTasks = "search-tasks"
)

// DecisionTrigger is TopicAgentDecisions' message shape.
type DecisionTrigger struct {
	RequestID string `json:"request_id"`
}

// SearchTaskTrigger is TopicSearchTasks' message shape.
type SearchTaskTrigger struct {
	TaskID string `json:"task_id"`
}

// Producer sends triggers onto one topic, keyed so every message for the
// same request/task lands on the same partition — steps within one
// request's chain process in order, not out of order across partitions.
type Producer struct {
	w *kafkago.Writer
}

// NewProducer dials brokers lazily (kafka-go writers connect on first
// Write) for topic.
func NewProducer(brokers []string, topic string) *Producer {
	return &Producer{w: &kafkago.Writer{
		Addr:                   kafkago.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafkago.Hash{}, // key-based partitioning, not round-robin
		AllowAutoTopicCreation: true,            // fine at this project's scale; a real deploy would provision topics explicitly
	}}
}

// Send JSON-encodes value and publishes it keyed by key.
func (p *Producer) Send(ctx context.Context, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("kafka: encoding message: %w", err)
	}
	if err := p.w.WriteMessages(ctx, kafkago.Message{Key: []byte(key), Value: b}); err != nil {
		return fmt.Errorf("kafka: publishing to %s: %w", p.w.Topic, err)
	}
	return nil
}

func (p *Producer) Close() error { return p.w.Close() }

// Consumer reads triggers off one topic as part of consumer group groupID
// — multiple process instances in the same group share the topic's
// partitions rather than each seeing every message, so running more than
// one cmd/collector or cmd/agent-worker just adds capacity.
type Consumer struct {
	r *kafkago.Reader
}

func NewConsumer(brokers []string, topic, groupID string) *Consumer {
	return &Consumer{r: kafkago.NewReader(kafkago.ReaderConfig{
		Brokers: brokers,
		Topic:   topic,
		GroupID: groupID,
	})}
}

// Next blocks until a message arrives (or ctx is cancelled) and decodes it
// into out. It deliberately does NOT commit the read yet — that's what
// the returned commit func is for. Call it only once the work the
// message triggered has actually finished successfully. Committing early
// (as the simpler ReadMessage API does) would mean a crash between
// "message read" and "work done" silently drops that step; committing
// late means the same message gets redelivered to this consumer group
// after a restart or rebalance if the work never finished — safe to
// retry, since every step this project chains through Kafka is a
// database-backed, redo-safe check ("is this request ready to decide?",
// "is this task ready to run?"), not a one-shot side effect.
func (c *Consumer) Next(ctx context.Context, out any) (commit func(context.Context) error, err error) {
	msg, err := c.r.FetchMessage(ctx)
	if err != nil {
		return nil, fmt.Errorf("kafka: reading from %s: %w", c.r.Config().Topic, err)
	}
	commit = func(ctx context.Context) error { return c.r.CommitMessages(ctx, msg) }
	if err := json.Unmarshal(msg.Value, out); err != nil {
		// A poison message (never going to decode successfully) would
		// otherwise block this partition forever — commit past it rather
		// than retry it endlessly, and let the caller log the error.
		return commit, fmt.Errorf("kafka: decoding message: %w", err)
	}
	return commit, nil
}

func (c *Consumer) Close() error { return c.r.Close() }
