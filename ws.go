package ws

import (
	"context"
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

// ActorOption defines a function type for configuring the Actor.
type ActorOption func(*Actor)

// WithMailboxSize sets the size of the actor's outbound mailbox. Default is 10.
func WithMailboxSize(size int) ActorOption {
	return func(a *Actor) {
		a.config.mailboxSize = size
	}
}

// WithPingInterval sets how often the actor sends pings to keep the connection alive
// and detect silent drops. Default is 15 seconds.
func WithPingInterval(d time.Duration) ActorOption {
	return func(a *Actor) {
		a.config.pingInterval = d
	}
}

// WithHTTPClient allows providing a custom http.Client for the WebSocket dial.
func WithHTTPClient(c *http.Client) ActorOption {
	return func(a *Actor) {
		a.config.httpClient = c
	}
}

// WithOnConnect sets a callback that is invoked with the URL whenever a connection is successfully established.
func WithOnConnect(handler func(url string)) ActorOption {
	return func(a *Actor) {
		a.config.onConnect = handler
	}
}

// WithOnMessage sets a callback that is invoked with each received message and the duration since the last message was processed.
func WithOnMessage(handler func(msg Message, duration time.Duration)) ActorOption {
	return func(a *Actor) {
		a.config.onMessage = handler
	}
}

// WithOnError sets a callback that is invoked with any error that causes the actor to fail and trigger a supervisor restart.
func WithOnError(handler func(err error)) ActorOption {
	return func(a *Actor) {
		a.config.onError = handler
	}
}

type actorConfig struct {
	httpClient   *http.Client
	mailboxSize  int
	pingInterval time.Duration
	onConnect    func(url string)
	onMessage    func(msg Message, duration time.Duration)
	onError      func(err error)
}

type sendMsg struct {
	msgType MessageType
	data    []byte
}

// Actor connects to a WebSocket endpoint, delivers inbound messages to a handler,
// and exposes a thread-safe Send method for outbound messages. It is designed to run
// under a sup.Supervisor, which handles reconnection on failure.
type Actor struct {
	*sup.BaseActor
	mailbox *sup.Mailbox
	url     string
	handler func(Message)
	config  *actorConfig
}

// NewActor creates a new Actor with the specified URL, inbound message handler,
// and optional configuration options.
func NewActor(name string, url string, handler func(Message), opts ...ActorOption) *Actor {
	a := &Actor{
		BaseActor: sup.NewBaseActor(name),
		url:       url,
		handler:   handler,
		config: &actorConfig{
			mailboxSize:  10,
			pingInterval: 15 * time.Second,
		},
	}

	for _, opt := range opts {
		opt(a)
	}

	a.mailbox = sup.NewMailbox(a.config.mailboxSize)

	return a
}

// Send enqueues an outbound message to be written by the actor's run loop.
// It is safe to call from any goroutine.
func (a *Actor) Send(msgType MessageType, data []byte) error {
	return sup.Cast(a.mailbox, sendMsg{msgType: msgType, data: data})
}

// Run establishes the WebSocket connection and drives concurrent concerns:
// reading inbound frames, writing outbound frames, and maintaining keep-alive pings.
// Any failure causes Run to return an error, triggering a supervisor restart.
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

	if a.config.onConnect != nil {
		a.config.onConnect(a.url)
	}

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		pingTicker := time.NewTicker(a.config.pingInterval)
		defer pingTicker.Stop()

		for {
			select {
			case <-connCtx.Done():
				return
			case <-pingTicker.C:
				pingCtx, pingCancel := context.WithTimeout(connCtx, 5*time.Second)
				_ = conn.Ping(pingCtx)
				pingCancel()
			}
		}
	}()

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

	msgStart := time.Now()

	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "shutting down")
			return ctx.Err()

		case res := <-inbound:
			if res.err != nil {
				if a.config.onError != nil {
					a.config.onError(res.err)
				}
				return res.err
			}

			if a.config.onMessage != nil {
				a.config.onMessage(res.msg, time.Since(msgStart))
			}
			a.handler(res.msg)
			msgStart = time.Now()

		case envelope, ok := <-a.mailbox.Receive():
			if !ok {
				conn.Close(websocket.StatusNormalClosure, "mailbox closed")
				return nil
			}

			switch m := envelope.(type) {
			case sup.CastRequest[sendMsg]:
				p := m.Payload()
				if err := conn.Write(connCtx, websocket.MessageType(p.msgType), p.data); err != nil {
					if a.config.onError != nil {
						a.config.onError(err)
					}
					return err
				}
			}
		}
	}
}
