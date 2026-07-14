// Package mq owns Relay's RabbitMQ topology and thin publish/consume helpers.
//
// Topology (see docs/architecture/01-messaging-topology.md):
//
//	relay.events    (direct) --event.received--> relay.event-fanout
//	relay.deliveries(direct) --delivery--------> relay.deliveries
//	relay.retry     (direct) --<tier>----------> relay.retry.<tier>  (TTL, DLX -> relay.deliveries)
//	relay.dlx       (direct) --dead------------> relay.dlq
//
// Every service declares the full topology at startup; declarations are
// idempotent so start order does not matter.
package mq

import (
	"context"
	"fmt"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	EventsExchange     = "relay.events"
	DeliveriesExchange = "relay.deliveries"
	RetryExchange      = "relay.retry"
	DLXExchange        = "relay.dlx"

	EventFanoutQueue = "relay.event-fanout"
	DeliveriesQueue  = "relay.deliveries"
	DeadLetterQueue  = "relay.dlq"

	EventRoutingKey    = "event.received"
	DeliveryRoutingKey = "delivery"
	DeadRoutingKey     = "dead"
)

// RetryTier is one rung of the backoff ladder: a queue with a fixed TTL whose
// dead-letter target is the main deliveries queue.
type RetryTier struct {
	Name string
	TTL  time.Duration
}

// RetryTiers is the exponential backoff schedule. Attempt N that fails is
// parked in tier min(N-1, len-1) before re-entering the deliveries queue.
var RetryTiers = []RetryTier{
	{"10s", 10 * time.Second},
	{"1m", time.Minute},
	{"5m", 5 * time.Minute},
	{"30m", 30 * time.Minute},
	{"2h", 2 * time.Hour},
}

// TierForAttempt returns the tier a failed attempt should be parked in.
func TierForAttempt(attempt int) RetryTier {
	i := attempt - 1
	if i < 0 {
		i = 0
	}
	if i >= len(RetryTiers) {
		i = len(RetryTiers) - 1
	}
	return RetryTiers[i]
}

// Declare sets up all exchanges, queues and bindings. Idempotent.
func Declare(ch *amqp.Channel) error {
	for _, ex := range []string{EventsExchange, DeliveriesExchange, RetryExchange, DLXExchange} {
		if err := ch.ExchangeDeclare(ex, "direct", true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare exchange %s: %w", ex, err)
		}
	}

	declare := func(queue, exchange, key string, args amqp.Table) error {
		if _, err := ch.QueueDeclare(queue, true, false, false, false, args); err != nil {
			return fmt.Errorf("declare queue %s: %w", queue, err)
		}
		if err := ch.QueueBind(queue, key, exchange, false, nil); err != nil {
			return fmt.Errorf("bind queue %s: %w", queue, err)
		}
		return nil
	}

	if err := declare(EventFanoutQueue, EventsExchange, EventRoutingKey, nil); err != nil {
		return err
	}
	if err := declare(DeliveriesQueue, DeliveriesExchange, DeliveryRoutingKey, nil); err != nil {
		return err
	}
	if err := declare(DeadLetterQueue, DLXExchange, DeadRoutingKey, nil); err != nil {
		return err
	}

	// The retry ladder: per-QUEUE TTLs (not per-message) so a short-delay message
	// can never sit blocked behind a long-delay one (no head-of-line blocking).
	for _, tier := range RetryTiers {
		args := amqp.Table{
			"x-message-ttl":             tier.TTL.Milliseconds(),
			"x-dead-letter-exchange":    DeliveriesExchange,
			"x-dead-letter-routing-key": DeliveryRoutingKey,
		}
		if err := declare("relay.retry."+tier.Name, RetryExchange, tier.Name, args); err != nil {
			return err
		}
	}
	return nil
}

// Client wraps a connection + confirm-mode channel for publishing.
type Client struct {
	conn *amqp.Connection
	ch   *amqp.Channel
	mu   sync.Mutex
}

// Connect dials RabbitMQ, opens a confirm-mode channel and declares topology.
func Connect(url string) (*Client, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("dial amqp: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("open channel: %w", err)
	}
	if err := Declare(ch); err != nil {
		conn.Close()
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enable confirms: %w", err)
	}
	return &Client{conn: conn, ch: ch}, nil
}

// Publish sends a persistent JSON message and waits for the broker confirm,
// so a nil error means the broker has taken responsibility for the message.
func (c *Client) Publish(ctx context.Context, exchange, key string, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	conf, err := c.ch.PublishWithDeferredConfirmWithContext(ctx, exchange, key, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		Body:         body,
	})
	if err != nil {
		return fmt.Errorf("publish %s/%s: %w", exchange, key, err)
	}
	acked, err := conf.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("confirm %s/%s: %w", exchange, key, err)
	}
	if !acked {
		return fmt.Errorf("broker nacked publish to %s/%s", exchange, key)
	}
	return nil
}

// Consume opens a dedicated channel with the given prefetch and returns the
// delivery stream for queue.
func (c *Client) Consume(queue string, prefetch int) (<-chan amqp.Delivery, error) {
	ch, err := c.conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open consume channel: %w", err)
	}
	if err := ch.Qos(prefetch, 0, false); err != nil {
		return nil, fmt.Errorf("set qos: %w", err)
	}
	deliveries, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return nil, fmt.Errorf("consume %s: %w", queue, err)
	}
	return deliveries, nil
}

// NotifyClose reports connection-level failures so services can crash and let
// the orchestrator restart them (crash-only recovery).
func (c *Client) NotifyClose() <-chan *amqp.Error {
	return c.conn.NotifyClose(make(chan *amqp.Error, 1))
}

func (c *Client) Close() { c.conn.Close() }
