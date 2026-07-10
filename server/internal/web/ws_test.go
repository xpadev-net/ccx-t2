package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xpadev/ccx-t2/internal/worker"
)

func TestWorkerLogWebSocketSnapshotPrecedesQueuedLiveOutput(t *testing.T) {
	stream := make(chan []byte, 4)
	registry := worker.NewRegistry()
	registry.Register(worker.Info{WorkerID: "worker-task-001", TaskID: "task-001"})
	subscribed := make(chan struct{})
	captureStarted := make(chan struct{})
	releaseCapture := make(chan struct{})

	server := httptest.NewServer(New(Deps{
		Config:   testConfig(),
		Registry: registry,
		PipeBytes: func(session, window string) (<-chan []byte, func(), error) {
			close(subscribed)
			return stream, func() {}, nil
		},
		CapturePane: func(ctx context.Context, session, window string) ([]byte, error) {
			select {
			case <-subscribed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			close(captureStarted)
			stream <- []byte("live")
			select {
			case <-releaseCapture:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			close(stream)
			return []byte("snapshot"), nil
		},
		AuthDisabled: true,
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/ws/worker/worker-task-001"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	select {
	case <-captureStarted:
	case <-time.After(time.Second):
		t.Fatal("capture did not start after stream subscription")
	}
	close(releaseCapture)

	var snapshot wsMessage
	if err := conn.ReadJSON(&snapshot); err != nil {
		t.Fatalf("ReadJSON snapshot: %v", err)
	}
	if snapshot.Type != "chunk" || snapshot.Data != "snapshot" {
		t.Fatalf("snapshot = %#v, want snapshot chunk before live output", snapshot)
	}
	var live wsMessage
	if err := conn.ReadJSON(&live); err != nil {
		t.Fatalf("ReadJSON live: %v", err)
	}
	if live.Type != "chunk" || live.Data != "live" {
		t.Fatalf("live = %#v, want queued live chunk after snapshot", live)
	}
	var closed wsMessage
	if err := conn.ReadJSON(&closed); err != nil {
		t.Fatalf("ReadJSON closed: %v", err)
	}
	if closed.Type != "closed" {
		t.Fatalf("closed = %#v, want closed", closed)
	}
}

func TestTmuxSharedStreamSignalsSlowSubscriberWithoutStarvingHealthySubscriber(t *testing.T) {
	registry := &tmuxStreamRegistry{}
	source := make(chan []byte)
	cleanupCalled := make(chan struct{})
	pipe := func(session, window string) (<-chan []byte, func(), error) {
		return source, func() { close(cleanupCalled) }, nil
	}

	slow, slowCleanup, err := registry.subscribeWithStatus("session\x00window", "session", "window", pipe)
	if err != nil {
		t.Fatalf("subscribe slow: %v", err)
	}
	healthy, healthyCleanup, err := registry.subscribeWithStatus("session\x00window", "session", "window", pipe)
	if err != nil {
		t.Fatalf("subscribe healthy: %v", err)
	}
	defer slowCleanup()
	defer healthyCleanup()

	healthyChunks := make(chan []byte, 130)
	healthyDone := make(chan struct{})
	healthyFirst := make(chan struct{})
	go func() {
		defer close(healthyDone)
		first := true
		for chunk := range healthy.chunks {
			healthyChunks <- chunk
			if first {
				close(healthyFirst)
				first = false
			}
		}
	}()

	source <- []byte{0}
	select {
	case <-healthyFirst:
	case <-time.After(time.Second):
		t.Fatal("healthy subscriber did not receive the first chunk")
	}
	for i := 1; i < cap(slow.chunks)+2; i++ {
		source <- []byte{byte(i)}
	}
	close(source)

	select {
	case <-slow.slow:
	case <-time.After(time.Second):
		t.Fatal("slow subscriber was not signaled for falling behind")
	}
	select {
	case <-healthyDone:
	case <-time.After(time.Second):
		t.Fatal("healthy subscriber did not finish")
	}

	if got := len(healthyChunks); got != cap(slow.chunks)+2 {
		t.Fatalf("healthy chunk count = %d, want %d", got, cap(slow.chunks)+2)
	}
	select {
	case <-cleanupCalled:
	case <-time.After(time.Second):
		t.Fatal("shared pipe cleanup was not called")
	}
}

func TestWSWriterSerializesConcurrentMessagesAndPing(t *testing.T) {
	const messageCount = 4 * 24
	handlerErrors := make(chan error, 1)
	var pings atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			handlerErrors <- err
			return
		}
		defer conn.Close()
		ctx, cancel := context.WithCancel(context.Background())
		writer := newWSWriterWithPingPeriod(ctx, conn, cancel, 5*time.Millisecond)

		var wg sync.WaitGroup
		for producer := 0; producer < 4; producer++ {
			wg.Add(1)
			go func(producer int) {
				defer wg.Done()
				for sequence := 0; sequence < 24; sequence++ {
					messageType := "chunk"
					if sequence%8 == 0 {
						messageType = "error"
					}
					if err := writer.send(ctx, wsMessage{Type: messageType, Data: string(rune(producer*24 + sequence))}); err != nil {
						handlerErrors <- err
						return
					}
				}
			}(producer)
		}
		wg.Wait()
		time.Sleep(25 * time.Millisecond)
		writer.close()
		cancel()
		handlerErrors <- nil
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/writer"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	conn.SetPingHandler(func(appData string) error {
		pings.Add(1)
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	gotMessages := 0
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
		gotMessages++
	}
	if gotMessages != messageCount {
		t.Fatalf("message count = %d, want %d", gotMessages, messageCount)
	}
	if pings.Load() == 0 {
		t.Fatal("idle writer did not send a ping")
	}
	select {
	case err := <-handlerErrors:
		if err != nil {
			t.Fatalf("writer handler: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer handler did not finish")
	}
}

func TestWSReaderReclaimsPeerThatNeverSendsPong(t *testing.T) {
	readerResult := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			readerResult <- err
			return
		}
		defer conn.Close()
		if err := configureWSReaderWithWait(conn, 25*time.Millisecond); err != nil {
			readerResult <- err
			return
		}
		_, _, err = conn.NextReader()
		readerResult <- err
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/reader"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	select {
	case err := <-readerResult:
		if err == nil {
			t.Fatal("NextReader error = nil, want read deadline")
		}
		var timeoutErr interface{ Timeout() bool }
		if !errors.As(err, &timeoutErr) || !timeoutErr.Timeout() {
			t.Fatalf("NextReader error = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dead peer was not reclaimed within the configured deadline")
	}
}
