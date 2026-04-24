# sup-ws

[![Go Reference](https://pkg.go.dev/badge/github.com/webermarci/sup-ws.svg)](https://pkg.go.dev/github.com/webermarci/sup-ws)
[![Test](https://github.com/webermarci/sup-ws/actions/workflows/test.yml/badge.svg)](https://github.com/webermarci/sup-ws/actions/workflows/test.yml)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

`sup-ws` is a high-reliability WebSocket client implementation for Go, built on top of the [sup](https://github.com/webermarci/sup) actor library. It provides a thread-safe, supervised, and observable way to interact with WebSocket endpoints.

## Why this exists?

WebSocket connections are long-lived and inherently stateful. If two goroutines try to write to the same connection simultaneously, the result is a data race and a broken stream.

This library solves that by treating the WebSocket connection as an **Actor**. All outbound messages are queued in a mailbox and written sequentially by the actor loop, ensuring writes are perfectly serialized. Inbound messages are delivered to a handler function as they arrive.

## Features

* **Actor-Based Concurrency**: Thread-safe outbound writes via `Send`. Multiple goroutines can call `Send` safely; the actor serializes all writes.
* **Supervised Lifecycle**: Designed to run under a `sup.Supervisor`. Any connection failure causes the actor to return a fatal error, letting the supervisor handle reconnection.
* **Binary and Text Support**: Exposes the WebSocket message type (`MessageText` / `MessageBinary`) alongside the payload.
* **Idle Timeout**: Configurable timeout that triggers a reconnect if no message is received within the window.
* **Keepalive Pings**: Configurable ping interval to detect silent connection drops before the idle timeout fires.
* **Rich Observability**: Built-in `Observer` interface to monitor connection events, received messages, and failures.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/webermarci/sup"
	ws "github.com/webermarci/sup-ws"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := func(message ws.Message) {
		fmt.Println(message)
	}

	actor := ws.NewActor("wss://example.com/stream", handler,
		ws.WithTimeout(30*time.Second),
		ws.WithPingInterval(15*time.Second),
	)

	supervisor := sup.NewSupervisor(
		sup.WithActor(actor),
		sup.WithPolicy(sup.Permanent),
		sup.WithRestartDelay(time.Second),
		sup.WithRestartLimit(5, 10*time.Second),
	)

	go supervisor.Run(ctx)

	_ = client.Send(ws.MessageText, []byte(`{"action":"subscribe","channel":"updates"}`))
	_ = client.Send(ws.MessageBinary, []byte{0x01, 0x02, 0x03})

	supervisor.Wait()
}
```

## Using it with [pubsub](https://github.com/webermarci/pubsub)

```go
package main

import (
  "context"
  "fmt"
  "time"

  "github.com/webermarci/pubsub"
  "github.com/webermarci/sup"
  ws "github.com/webermarci/sup-ws"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pubsub := pubsub.New[string, ws.Message](10)
  
	handler := func(message ws.Message) {
		pubsub.Publish("ws", message)
	}
	
	actor := ws.NewActor("wss://example.com/stream", handler,
		ws.WithTimeout(30*time.Second),
		ws.WithPingInterval(15*time.Second),
	)
	
	supervisor := sup.NewSupervisor(
		sup.WithActor(actor),
		sup.WithPolicy(sup.Permanent),
		sup.WithRestartDelay(time.Second),
		sup.WithRestartLimit(5, 10 * time.Second),
	)
	
	go supervisor.Run(ctx)
	
	messages := pubsub.Subscribe(ctx, "ws")
	
	go func() {
		for message := range messages {
			fmt.Println(message)
		}
	}()
	
	supervisor.Wait()
	pubsub.Close()
}
```
