package ws

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

type stubConn struct {
	deviceID string
	send     chan Frame
	closed   atomic.Int32
}

func newStubConn(deviceID string) *stubConn {
	return &stubConn{deviceID: deviceID, send: make(chan Frame, 1)}
}

func (c *stubConn) DeviceID() string   { return c.deviceID }
func (c *stubConn) Send() chan<- Frame { return c.send }
func (c *stubConn) Close()             { c.closed.Add(1) }
func (c *stubConn) closedCount() int   { return int(c.closed.Load()) }

func TestHubReplacesConnectionForSameDevice(t *testing.T) {
	h := NewHub()
	old := newStubConn("phone")
	current := newStubConn("phone")

	h.Register(1, old)
	h.Register(1, current)

	if old.closedCount() != 1 {
		t.Fatalf("old connection closed %d times", old.closedCount())
	}
	h.Deliver(context.Background(), 1, Frame{Type: "message"})

	select {
	case got := <-current.send:
		if got.Type != "message" {
			t.Fatalf("frame type = %q", got.Type)
		}
	default:
		t.Fatal("current connection did not receive the frame")
	}
	select {
	case <-old.send:
		t.Fatal("closed connection received a frame")
	default:
	}
}

func TestHubUnregisterKeepsNewerConnection(t *testing.T) {
	h := NewHub()
	old := newStubConn("phone")
	current := newStubConn("phone")
	h.Register(1, old)
	h.Register(1, current)

	h.Unregister(1, old)
	h.Deliver(context.Background(), 1, Frame{Type: "message"})
	select {
	case <-current.send:
	default:
		t.Fatal("newer connection was removed by stale unregister")
	}
}

func TestHubClosesSlowConsumerWithoutBlocking(t *testing.T) {
	h := NewHub()
	slow := newStubConn("slow")
	live := newStubConn("live")
	h.Register(1, slow)
	h.Register(1, live)

	for i := 0; i < 3; i++ {
		h.Deliver(context.Background(), 1, Frame{Type: strconv.Itoa(i)})
	}
	if slow.closedCount() == 0 {
		t.Fatal("slow connection was not closed")
	}
	select {
	case got := <-live.send:
		if got.Type == "" {
			t.Fatal("live connection got empty frame")
		}
	default:
		t.Fatal("live connection did not receive the first frame")
	}
}

func TestHubConcurrentRegisterDeliverUnregister(t *testing.T) {
	h := NewHub()
	const users = 100
	var wg sync.WaitGroup
	for user := int64(1); user <= users; user++ {
		for device := 0; device < 4; device++ {
			wg.Add(1)
			go func(user int64, device int) {
				defer wg.Done()
				conn := newStubConn("device-" + strconv.Itoa(device))
				h.Register(user, conn)
				h.Deliver(context.Background(), user, Frame{Type: "tick"})
				h.Unregister(user, conn)
			}(user, device)
		}
	}
	wg.Wait()
}
