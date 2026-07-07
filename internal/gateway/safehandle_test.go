package gateway

import (
	"net"
	"testing"
	"time"
)

// TestSafeHandleContainsPanic verifies that a panic raised inside a
// per-connection handler is recovered (the goroutine returns instead of
// crashing the process) and the connection is closed.
func TestSafeHandleContainsPanic(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()

	// Set the read deadline while the pipe is still open; a closed net.Pipe
	// rejects SetReadDeadline. This bounds the read below if closure regresses.
	if err := server.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		safeHandle(client, testLogger(), "test", func(net.Conn) {
			panic("boom")
		})
	}()

	select {
	case <-done:
		// safeHandle returned normally => the panic was recovered.
	case <-time.After(2 * time.Second):
		t.Fatal("safeHandle did not return; panic was not recovered")
	}

	// The connection must have been closed by safeHandle: a read on the peer
	// end returns EOF/error rather than blocking.
	if _, err := server.Read(make([]byte, 1)); err == nil {
		t.Error("expected connection to be closed after recovered panic")
	}
}
