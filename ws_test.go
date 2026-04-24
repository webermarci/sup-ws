package ws_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/webermarci/sup"
	ws "github.com/webermarci/sup-ws"
)

func newTestServer(t *testing.T, handler func(context.Context, *websocket.Conn)) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		handler(ctx, conn)
	}))

	t.Cleanup(srv.Close)
	return srv
}

func wsURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func runSupervisor(ctx context.Context, actor sup.Actor, opts ...sup.SupervisorOption) <-chan error {
	done := make(chan error, 1)

	supervisorOpts := append([]sup.SupervisorOption{
		sup.WithActor(actor),
	}, opts...)

	supervisor := sup.NewSupervisor(supervisorOpts...)

	go func() {
		done <- supervisor.Run(ctx)
	}()

	return done
}

func wait[T any](t *testing.T, ch <-chan T, d time.Duration, name string) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatalf("timeout waiting for %s", name)
		var zero T
		return zero
	}
}

func TestWSActorReceivesTextMessage(t *testing.T) {
	accepted := make(chan struct{}, 1)
	received := make(chan ws.Message, 1)

	server := newTestServer(t, func(ctx context.Context, conn *websocket.Conn) {
		accepted <- struct{}{}

		if err := conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
			t.Errorf("server write: %v", err)
			return
		}

		<-ctx.Done()
	})

	actor := ws.NewActor(
		wsURL(server),
		func(msg ws.Message) {
			received <- msg
		},
		ws.WithPingInterval(time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runSupervisor(ctx, actor, sup.WithPolicy(sup.Temporary))

	wait(t, accepted, time.Second, "server accept")
	msg := wait(t, received, 2*time.Second, "received text message")

	if msg.Type != ws.MessageText {
		t.Fatalf("expected message type %v, got %v", ws.MessageText, msg.Type)
	}
	if string(msg.Data) != "hello" {
		t.Fatalf("expected payload %q, got %q", "hello", string(msg.Data))
	}

	cancel()
	<-done
}

func TestWSActorReceivesBinaryMessage(t *testing.T) {
	accepted := make(chan struct{}, 1)
	received := make(chan ws.Message, 1)
	payload := []byte{0x01, 0x02, 0x03, 0x04}

	server := newTestServer(t, func(ctx context.Context, conn *websocket.Conn) {
		accepted <- struct{}{}

		if err := conn.Write(ctx, websocket.MessageBinary, payload); err != nil {
			t.Errorf("server write: %v", err)
			return
		}

		<-ctx.Done()
	})

	actor := ws.NewActor(
		wsURL(server),
		func(msg ws.Message) {
			received <- msg
		},
		ws.WithPingInterval(time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runSupervisor(ctx, actor, sup.WithPolicy(sup.Temporary))

	wait(t, accepted, time.Second, "server accept")
	msg := wait(t, received, 2*time.Second, "received binary message")

	if msg.Type != ws.MessageBinary {
		t.Fatalf("expected message type %v, got %v", ws.MessageBinary, msg.Type)
	}
	if string(msg.Data) != string(payload) {
		t.Fatalf("expected payload %v, got %v", payload, msg.Data)
	}

	cancel()
	<-done
}

func TestWSActorSend(t *testing.T) {
	accepted := make(chan struct{}, 1)
	serverReceived := make(chan ws.Message, 1)

	server := newTestServer(t, func(ctx context.Context, conn *websocket.Conn) {
		accepted <- struct{}{}

		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Errorf("server read: %v", err)
			return
		}

		serverReceived <- ws.Message{
			Type: ws.MessageType(typ),
			Data: data,
		}
	})

	actor := ws.NewActor(
		wsURL(server),
		func(msg ws.Message) {},
		ws.WithPingInterval(time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runSupervisor(ctx, actor, sup.WithPolicy(sup.Temporary))

	wait(t, accepted, time.Second, "server accept")

	if err := actor.Send(ws.MessageText, []byte("ping from client")); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	msg := wait(t, serverReceived, 2*time.Second, "server received message")

	if msg.Type != ws.MessageText {
		t.Fatalf("expected message type %v, got %v", ws.MessageText, msg.Type)
	}
	if string(msg.Data) != "ping from client" {
		t.Fatalf("expected payload %q, got %q", "ping from client", string(msg.Data))
	}

	cancel()
	<-done
}

type testObserver struct {
	connectCh chan string
	messageCh chan ws.Message
	failureCh chan error
}

func (o *testObserver) OnConnect(url string) {
	o.connectCh <- url
}

func (o *testObserver) OnMessage(msg ws.Message, _ time.Duration) {
	o.messageCh <- msg
}

func (o *testObserver) OnFailure(err error) {
	o.failureCh <- err
}

func TestWSActorObserver(t *testing.T) {
	observer := &testObserver{
		connectCh: make(chan string, 1),
		messageCh: make(chan ws.Message, 1),
		failureCh: make(chan error, 1),
	}

	server := newTestServer(t, func(ctx context.Context, conn *websocket.Conn) {
		if err := conn.Write(ctx, websocket.MessageText, []byte("observed")); err != nil {
			t.Errorf("server write: %v", err)
			return
		}

		time.Sleep(50 * time.Millisecond)
		_ = conn.Close(websocket.StatusInternalError, "boom")
	})

	actor := ws.NewActor(
		wsURL(server),
		func(msg ws.Message) {},
		ws.WithObserver(observer),
		ws.WithPingInterval(time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runSupervisor(ctx, actor, sup.WithPolicy(sup.Temporary))

	url := wait(t, observer.connectCh, time.Second, "observer connect")
	if url == "" {
		t.Fatal("expected non-empty connect url")
	}

	msg := wait(t, observer.messageCh, 2*time.Second, "observer message")
	if msg.Type != ws.MessageText {
		t.Fatalf("expected message type %v, got %v", ws.MessageText, msg.Type)
	}
	if string(msg.Data) != "observed" {
		t.Fatalf("expected payload %q, got %q", "observed", string(msg.Data))
	}

	err := wait(t, observer.failureCh, 2*time.Second, "observer failure")
	if err == nil {
		t.Fatal("expected failure error")
	}

	cancel()
	<-done
}

func TestWSActorReconnectsUnderSupervisor(t *testing.T) {
	var connections atomic.Int32
	connected := make(chan int32, 4)

	server := newTestServer(t, func(ctx context.Context, conn *websocket.Conn) {
		n := connections.Add(1)
		connected <- n

		if n == 1 {
			_ = conn.Close(websocket.StatusInternalError, "force reconnect")
			return
		}

		<-ctx.Done()
	})

	actor := ws.NewActor(
		wsURL(server),
		func(msg ws.Message) {},
		ws.WithPingInterval(time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runSupervisor(
		ctx,
		actor,
		sup.WithPolicy(sup.Permanent),
		sup.WithRestartDelay(50*time.Millisecond),
	)

	first := wait(t, connected, time.Second, "first connection")
	second := wait(t, connected, 2*time.Second, "second connection")

	if first != 1 {
		t.Fatalf("expected first connection count 1, got %d", first)
	}
	if second < 2 {
		t.Fatalf("expected reconnect to produce count >= 2, got %d", second)
	}

	cancel()
	<-done
}

func TestWSActorReturnsContextErrorOnCancel(t *testing.T) {
	accepted := make(chan struct{}, 1)

	server := newTestServer(t, func(ctx context.Context, conn *websocket.Conn) {
		accepted <- struct{}{}
		<-ctx.Done()
	})

	actor := ws.NewActor(
		wsURL(server),
		func(msg ws.Message) {},
		ws.WithPingInterval(time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := runSupervisor(ctx, actor, sup.WithPolicy(sup.Temporary))

	wait(t, accepted, time.Second, "server accept")
	cancel()

	err := wait(t, done, 2*time.Second, "supervisor completion")
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
