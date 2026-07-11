package web

import (
	"bytes"
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

func TestWorkerLogWebSocketSlowEvictionSendsReconnectError(t *testing.T) {
	stream := make(chan []byte)
	registry := worker.NewRegistry()
	registry.Register(worker.Info{WorkerID: "worker-task-001", TaskID: "task-001"})
	subscribed := make(chan struct{})
	evictionObserved := make(chan struct{})

	server := httptest.NewServer(New(Deps{
		Config:   testConfig(),
		Registry: registry,
		PipeBytes: func(session, window string) (<-chan []byte, func(), error) {
			close(subscribed)
			return stream, func() { close(evictionObserved) }, nil
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
	case <-subscribed:
	case <-time.After(time.Second):
		t.Fatal("stream was not subscribed")
	}
	chunk := bytes.Repeat([]byte("burst"), 65536/len("burst"))
	go func() {
		for i := 0; i < 256; i++ {
			stream <- chunk
		}
		close(stream)
	}()
	select {
	case <-evictionObserved:
	case <-time.After(time.Second):
		t.Fatal("slow-client eviction was not observed")
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	for {
		var msg wsMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("ReadJSON slow eviction: %v", err)
		}
		if msg.Type == "error" {
			if msg.Data != "log stream fell behind; reconnecting" {
				t.Fatalf("slow eviction message = %#v, want deterministic reconnect error", msg)
			}
			return
		}
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

func TestTmuxSharedAtomicAttachmentReusesSnapshotAndCleansUpAfterLastSubscriber(t *testing.T) {
	registry := &tmuxStreamRegistry{}
	source := make(chan []byte, 2)
	cleanupCalled := make(chan struct{})
	attachCalls := atomic.Int32{}
	attach := func(context.Context, string, string) (*PaneAttachment, error) {
		attachCalls.Add(1)
		return &PaneAttachment{
			Snapshot: []byte("snapshot"),
			Chunks:   source,
			Cleanup: func() {
				close(cleanupCalled)
				close(source)
			},
		}, nil
	}

	snapshot1, first, firstCleanup, err := registry.subscribeAttachedWithStatus(context.Background(), "session\x00window", "session", "window", attach)
	if err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	snapshot2, second, secondCleanup, err := registry.subscribeAttachedWithStatus(context.Background(), "session\x00window", "session", "window", attach)
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}
	if got := attachCalls.Load(); got != 1 {
		t.Fatalf("attach calls = %d, want one shared attachment", got)
	}
	if string(snapshot1) != "snapshot" || string(snapshot2) != "snapshot" {
		t.Fatalf("snapshots = %q and %q, want shared snapshot", snapshot1, snapshot2)
	}

	source <- []byte("same")
	for name, subscription := range map[string]*tmuxStreamSubscription{"first": first, "second": second} {
		select {
		case chunk := <-subscription.chunks:
			if string(chunk) != "same" {
				t.Fatalf("%s chunk = %q, want same", name, chunk)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s subscriber did not receive live output", name)
		}
	}
	firstCleanup()
	select {
	case <-cleanupCalled:
		t.Fatal("shared attachment cleaned up before last subscriber")
	case <-time.After(25 * time.Millisecond):
	}
	secondCleanup()
	select {
	case <-cleanupCalled:
	case <-time.After(time.Second):
		t.Fatal("shared attachment cleanup was not called after last subscriber")
	}
}

func TestTmuxSharedAtomicAttachmentLateSubscriberReplaysThroughJoinWatermark(t *testing.T) {
	registry := &tmuxStreamRegistry{}
	source := make(chan []byte, 4)
	attach := func(context.Context, string, string) (*PaneAttachment, error) {
		return &PaneAttachment{
			Snapshot: []byte("base"),
			Chunks:   source,
			Cleanup:  func() { close(source) },
		}, nil
	}

	snapshot1, first, firstCleanup, err := registry.subscribeAttachedWithStatus(context.Background(), "session\x00window", "session", "window", attach)
	if err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	if string(snapshot1) != "base" {
		t.Fatalf("first snapshot = %q, want base", snapshot1)
	}
	source <- []byte("X")
	select {
	case chunk := <-first.chunks:
		if string(chunk) != "X" {
			t.Fatalf("first live chunk = %q, want X", chunk)
		}
	case <-time.After(time.Second):
		t.Fatal("first subscriber did not receive X")
	}

	snapshot2, second, secondCleanup, err := registry.subscribeAttachedWithStatus(context.Background(), "session\x00window", "session", "window", attach)
	if err != nil {
		t.Fatalf("late subscribe: %v", err)
	}
	if string(snapshot2) != "base" {
		t.Fatalf("late snapshot = %q, want base", snapshot2)
	}
	select {
	case replay, ok := <-second.initial:
		if !ok || string(replay) != "X" {
			t.Fatalf("late replay = %q, open=%v; want X", replay, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("late subscriber did not receive replay through join watermark")
	}
	select {
	case _, ok := <-second.initial:
		if ok {
			t.Fatal("late replay remained open after watermark")
		}
	case <-time.After(time.Second):
		t.Fatal("late replay did not finish before future live output")
	}

	source <- []byte("Y")
	select {
	case chunk := <-second.chunks:
		if string(chunk) != "Y" {
			t.Fatalf("late future chunk = %q, want Y", chunk)
		}
	case <-time.After(time.Second):
		t.Fatal("late subscriber did not receive future output")
	}
	firstCleanup()
	secondCleanup()
}

func TestTmuxAtomicAttachmentDoesNotBlockUnrelatedKeys(t *testing.T) {
	registry := &tmuxStreamRegistry{}
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	makeAttachment := func() *PaneAttachment {
		chunks := make(chan []byte)
		var once sync.Once
		return &PaneAttachment{
			Chunks: chunks,
			Cleanup: func() {
				once.Do(func() { close(chunks) })
			},
		}
	}
	attach := func(_ context.Context, _ string, window string) (*PaneAttachment, error) {
		if window == "slow" {
			close(slowStarted)
			<-releaseSlow
		}
		return makeAttachment(), nil
	}

	slowResult := make(chan error, 1)
	go func() {
		_, _, cleanup, err := registry.subscribeAttachedWithStatus(context.Background(), "session\x00slow", "session", "slow", attach)
		if cleanup != nil {
			defer cleanup()
		}
		slowResult <- err
	}()
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow attachment did not start")
	}

	fastResult := make(chan error, 1)
	go func() {
		_, _, cleanup, err := registry.subscribeAttachedWithStatus(context.Background(), "session\x00fast", "session", "fast", attach)
		if cleanup != nil {
			cleanup()
		}
		fastResult <- err
	}()
	select {
	case err := <-fastResult:
		if err != nil {
			t.Fatalf("fast attachment: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated fast attachment was blocked by slow tmux I/O")
	}
	close(releaseSlow)
	select {
	case err := <-slowResult:
		if err != nil {
			t.Fatalf("slow attachment: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow attachment did not finish after release")
	}
}

func TestTmuxAtomicAttachmentCleansUpWhenSourceEnds(t *testing.T) {
	registry := &tmuxStreamRegistry{}
	source := make(chan []byte)
	cleaned := make(chan struct{})
	snapshot, subscription, cleanup, err := registry.subscribeAttachedWithStatus(context.Background(), "session\x00window", "session", "window", func(context.Context, string, string) (*PaneAttachment, error) {
		return &PaneAttachment{Snapshot: []byte("snapshot"), Chunks: source, Cleanup: func() { close(cleaned) }}, nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if string(snapshot) != "snapshot" {
		t.Fatalf("snapshot = %q, want snapshot", snapshot)
	}
	close(source)
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("source completion did not clean up attachment")
	}
	select {
	case _, ok := <-subscription.chunks:
		if ok {
			t.Fatal("subscriber stream remained open after source completion")
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber stream did not close after source completion")
	}
	cleanup()
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
