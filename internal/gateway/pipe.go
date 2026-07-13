package gateway

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// connSeq hands out a per-connection id used to correlate a connection's accept
// and close log lines.
var connSeq atomic.Uint64

// connLog carries the state needed to emit a connection's close line after its
// accept line has been logged.
type connLog struct {
	log   *slog.Logger
	proxy string
	id    uint64
	addr  string
	start time.Time
}

// acceptConn logs the accept line for a newly accepted client connection and
// returns a connLog whose close method logs the matching close line. The id
// correlates the two lines; the client RemoteAddr is captured once.
func acceptConn(log *slog.Logger, proxy string, client net.Conn) *connLog {
	c := &connLog{
		log:   log,
		proxy: proxy,
		id:    connSeq.Add(1),
		addr:  client.RemoteAddr().String(),
		start: time.Now(),
	}
	log.Debug(proxy+" proxy: connection accepted", "id", c.id, "client", c.addr)
	return c
}

// close logs the connection-close line with the connection duration and the
// bytes transferred each way, as returned by pipe.
func (c *connLog) close(toUpstream, toClient int64) {
	c.log.Debug(c.proxy+" proxy: connection closed",
		"id", c.id,
		"client", c.addr,
		"duration", time.Since(c.start),
		"bytes_client_to_upstream", toUpstream,
		"bytes_upstream_to_client", toClient)
}

// pipe does a bidirectional copy between client and upstream, returning the
// number of bytes copied client→upstream and upstream→client. clientSrc is the
// reader for client-originated bytes: normally client itself, but the redis
// proxy passes a MultiReader that prepends bytes it had to buffer while
// intercepting AUTH.
//
// When either direction terminates, both connections are closed so the other
// direction unblocks and the goroutines can exit cleanly.
func pipe(client, upstream net.Conn, clientSrc io.Reader) (toUpstream, toClient int64) {
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			client.Close()   //nolint:errcheck
			upstream.Close() //nolint:errcheck
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer closeAll(); toClient, _ = io.Copy(client, upstream) }()
	go func() { defer wg.Done(); defer closeAll(); toUpstream, _ = io.Copy(upstream, clientSrc) }()
	wg.Wait()
	return toUpstream, toClient
}
