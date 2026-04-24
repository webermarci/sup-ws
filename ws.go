package ws

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/webermarci/sup"
)

// MessageType represents the type of a WebSocket message frame.
type MessageType int

const (
	MessageText   MessageType = MessageType(websocket.MessageText)
	MessageBinary MessageType = MessageType(websocket.MessageBinary)
)

// Message represents a single WebSocket message with its type and payload.
type Message struct {
	Type MessageType
	Data []byte
}

// Observer defines the interface for monitoring WebSocket connection lifecycle and messages.
type Observer interface {
	OnConnect(url string)
	OnMessage(msg Message, duration time.Duration)
	OnFailure(err error)
}

// ActorOption defines a function type for configuring the Actor.
type ActorOption func(*Actor)

// WithMailboxSize sets the size of the actor's outbound mailbox. Default is 10.
func WithMailboxSize(size int) ActorOption {
	return func(a *Actor) {
		a.config.mailboxSize = size
	}
}

// WithTimeout sets the idle read timeout — if no message is received within this
// duration, the connection is considered dead and the actor returns an error,
// allowing the supervisor to reconnect. Default is 30 seconds.
func WithTimeout(d time.Duration) ActorOption {
	return func(a *Actor) {
		a.config.timeout = d
	}
}

// WithPingInterval sets how often the actor sends pings to keep the connection alive
// and detect silent drops. Default is 15 seconds.
func WithPingInterval(d time.Duration) ActorOption {
	return func(a *Actor) {
		a.config.pingInterval = d
	}
}

// WithObserver assigns an Observer to receive notifications about connection events,
// received messages, and failures.
func WithObserver(o Observer) ActorOption {
	return func(a *Actor) {
		a.config.observer = o
	}
}

// WithHTTPClient allows providing a custom http.Client for the WebSocket dial.
func WithHTTPClient(c *http.Client) ActorOption {
	return func(a *Actor) {
		a.config.httpClient = c
	}
}

type actorConfig struct {
	observer     Observer
	httpClient   *http.Client
	mailboxSize  int
	timeout      time.Duration
	pingInterval time.Duration
}

type sendMsg struct {
	msgType MessageType
	data    []byte
}

// Actor connects to a WebSocket endpoint, delivers inbound messages to a handler,
// and exposes a thread-safe Send method for outbound messages. It is designed to run
// under a sup.Supervisor, which handles reconnection on failure.
type Actor struct {
	*sup.Mailbox
	url     string
	handler func(Message)
	config  *actorConfig
}

// NewActor creates a new Actor with the specified URL, inbound message handler,
// and optional configuration options.
func NewActor(url string, handler func(Message), opts ...ActorOption) *Actor {
	a := &Actor{
		url:     url,
		handler: handler,
		config: &actorConfig{
			mailboxSize:  10,
			timeout:      30 * time.Second,
			pingInterval: 15 * time.Second,
		},
	}

	for _, opt := range opts {
		opt(a)
	}

	a.Mailbox = sup.NewMailbox(a.config.mailboxSize)

	return a
}

// Send enqueues an outbound message to be written by the actor's run loop.
// It is safe to call from any goroutine.
func (a *Actor) Send(msgType MessageType, data []byte) error {
	return sup.Cast(a.Mailbox, sendMsg{msgType: msgType, data: data})
}

// Run establishes the WebSocket connection and drives two concurrent concerns:
// reading inbound frames (in a goroutine, since conn.Read is blocking) and writing
// outbound messages from the mailbox. Any failure on either side causes Run to return
// an error, triggering a supervisor restart and reconnection.
func (a *Actor) Run(ctx context.Context) error {
	dialOpts := &websocket.DialOptions{}
	if a.config.httpClient != nil {
		dialOpts.HTTPClient = a.config.httpClient
	}

	conn, _, err := websocket.Dial(ctx, a.url, dialOpts)
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	if a.config.observer != nil {
		a.config.observer.OnConnect(a.url)
	}

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type readResult struct {
		msg Message
		err error
	}

	inbound := make(chan readResult, 1)
	go func() {
		for {
			msgType, data, err := conn.Read(connCtx)
			if err != nil {
				inbound <- readResult{err: err}
				return
			}
			inbound <- readResult{msg: Message{Type: MessageType(msgType), Data: data}}
		}
	}()

	pingTicker := time.NewTicker(a.config.pingInterval)
	defer pingTicker.Stop()

	idleTimer := time.NewTimer(a.config.timeout)
	defer idleTimer.Stop()

	msgStart := time.Now()

	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "shutting down")
			return ctx.Err()

		case <-idleTimer.C:
			return fmt.Errorf("idle timeout after %s", a.config.timeout)

		case res := <-inbound:
			if res.err != nil {
				if a.config.observer != nil {
					a.config.observer.OnFailure(res.err)
				}
				return res.err
			}

			idleTimer.Reset(a.config.timeout)

			if a.config.observer != nil {
				a.config.observer.OnMessage(res.msg, time.Since(msgStart))
			}
			a.handler(res.msg)
			msgStart = time.Now()

		case envelope, ok := <-a.Receive():
			if !ok {
				conn.Close(websocket.StatusNormalClosure, "mailbox closed")
				return nil
			}

			switch m := envelope.(type) {
			case sup.CastRequest[sendMsg]:
				p := m.Payload()
				if err := conn.Write(connCtx, websocket.MessageType(p.msgType), p.data); err != nil {
					if a.config.observer != nil {
						a.config.observer.OnFailure(err)
					}
					return err
				}
			}

		case <-pingTicker.C:
			pingCtx, pingCancel := context.WithTimeout(connCtx, 5*time.Second)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				return fmt.Errorf("ping failed: %w", err)
			}
		}
	}
}
