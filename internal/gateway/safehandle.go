package gateway

import (
	"log/slog"
	"net"
	"runtime/debug"
)

// safeHandle runs a proxy's per-connection handler under a panic-recovering
// wrapper. A malformed or malicious client can drive a handler into an
// unhandled panic; without recovery that panic unwinds the per-connection
// goroutine and crashes the whole process, taking down every listener. Here
// the panic is logged with a stack trace, the connection is closed, and the
// failure stays scoped to that one connection.
func safeHandle(conn net.Conn, log *slog.Logger, proxy string, fn func(net.Conn)) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("proxy: recovered panic in connection handler",
				"proxy", proxy, "panic", r, "stack", string(debug.Stack()))
			conn.Close() //nolint:errcheck
		}
	}()
	fn(conn)
}
