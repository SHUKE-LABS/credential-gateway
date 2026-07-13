package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"credential-gateway/internal/config"
)

// syncBuffer is a mutex-guarded buffer: the proxy logs the accept/close lines
// from its per-connection goroutine while the test reads them.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogger returns a debug-level logger writing into the returned buffer.
func captureLogger() (*slog.Logger, *syncBuffer) {
	b := &syncBuffer{}
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

// waitForLog polls buf until substr appears or the timeout elapses, returning
// the captured log at that point.
func waitForLog(t *testing.T, buf *syncBuffer, substr string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := buf.String(); strings.Contains(s, substr) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log did not contain %q within timeout; got:\n%s", substr, buf.String())
	return ""
}

// logInt extracts the integer value of a `key=<digits>` slog attribute.
func logInt(t *testing.T, logs, key string) int64 {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(key) + `=(\d+)`).FindStringSubmatch(logs)
	if m == nil {
		t.Fatalf("attribute %q not found in logs:\n%s", key, logs)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", key, err)
	}
	return n
}

// assertLifecycle checks the shared accept/close contract: an accept line and a
// close line, both carrying the client RemoteAddr, the close line carrying a
// duration, and no credential value appearing anywhere.
func assertLifecycle(t *testing.T, buf *syncBuffer, proxy, password string) {
	t.Helper()
	logs := waitForLog(t, buf, proxy+" proxy: connection closed")
	if !strings.Contains(logs, proxy+" proxy: connection accepted") {
		t.Errorf("%s: missing accept line:\n%s", proxy, logs)
	}
	if !strings.Contains(logs, "client=127.0.0.1") {
		t.Errorf("%s: lifecycle lines missing client RemoteAddr:\n%s", proxy, logs)
	}
	if !strings.Contains(logs, "duration=") {
		t.Errorf("%s: close line missing duration:\n%s", proxy, logs)
	}
	if strings.Contains(logs, password) {
		t.Errorf("%s: credential value leaked into lifecycle logs:\n%s", proxy, logs)
	}
}

// TestPipe_ReturnsBidirectionalByteCounts is the authoritative check that pipe
// reports non-zero bytes each way — the guarantee every proxy's close line
// relies on.
func TestPipe_ReturnsBidirectionalByteCounts(t *testing.T) {
	clientProxy, clientApp := net.Pipe()
	upstreamProxy, upstreamApp := net.Pipe()

	type result struct{ toUpstream, toClient int64 }
	resCh := make(chan result, 1)
	go func() {
		u, c := pipe(clientProxy, upstreamProxy, clientProxy)
		resCh <- result{u, c}
	}()

	// client → upstream
	go func() { clientApp.Write([]byte("hello")) }() //nolint:errcheck
	buf := make([]byte, 5)
	if _, err := io.ReadFull(upstreamApp, buf); err != nil {
		t.Fatalf("read at upstream end: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("upstream received %q, want %q", buf, "hello")
	}

	// upstream → client
	go func() { upstreamApp.Write([]byte("hey")) }() //nolint:errcheck
	buf2 := make([]byte, 3)
	if _, err := io.ReadFull(clientApp, buf2); err != nil {
		t.Fatalf("read at client end: %v", err)
	}
	if string(buf2) != "hey" {
		t.Fatalf("client received %q, want %q", buf2, "hey")
	}

	// Closing either end unblocks both io.Copy goroutines.
	clientApp.Close()   //nolint:errcheck
	upstreamApp.Close() //nolint:errcheck

	select {
	case r := <-resCh:
		if r.toUpstream != 5 {
			t.Errorf("toUpstream = %d, want 5", r.toUpstream)
		}
		if r.toClient != 3 {
			t.Errorf("toClient = %d, want 3", r.toClient)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipe did not return after both ends closed")
	}
}

// TestLifecycleLogging_Redis drives a full connection through the redis proxy —
// which now routes its copy through the shared pipe helper — and asserts the
// accept and close lines, non-zero byte counts each way, and that the upstream
// password never appears.
func TestLifecycleLogging_Redis(t *testing.T) {
	const password = "s3cr3t-redis-pw"
	log, buf := captureLogger()
	upstream := newFakeRedis(t, password)
	p := &redisProxy{
		cfg: config.RedisService{Listen: "127.0.0.1:0", Upstream: upstream.addr, Password: password},
		log: log,
	}
	if err := p.Start(); err != nil {
		t.Fatalf("redis proxy start: %v", err)
	}
	t.Cleanup(func() { p.Stop(context.Background()) }) //nolint:errcheck

	if resp := ping(t, p.listener.Addr().String()); resp != "+PONG" {
		t.Fatalf("PING response = %q, want +PONG", resp)
	}

	logs := waitForLog(t, buf, "redis proxy: connection closed")
	if !strings.Contains(logs, "redis proxy: connection accepted") {
		t.Errorf("missing accept line:\n%s", logs)
	}
	if !strings.Contains(logs, "client=127.0.0.1") {
		t.Errorf("lifecycle lines missing client RemoteAddr:\n%s", logs)
	}
	if !strings.Contains(logs, "duration=") {
		t.Errorf("close line missing duration:\n%s", logs)
	}
	if n := logInt(t, logs, "bytes_client_to_upstream"); n <= 0 {
		t.Errorf("bytes_client_to_upstream = %d, want > 0", n)
	}
	if n := logInt(t, logs, "bytes_upstream_to_client"); n <= 0 {
		t.Errorf("bytes_upstream_to_client = %d, want > 0", n)
	}
	if strings.Contains(logs, password) {
		t.Errorf("credential value leaked into lifecycle logs:\n%s", logs)
	}
}

// TestLifecycleLogging_MySQL drives a client through the mysql proxy to the pipe
// stage and asserts the shared lifecycle contract.
func TestLifecycleLogging_MySQL(t *testing.T) {
	const password = "s3cr3t-mysql-pw"
	log, buf := captureLogger()
	upstream := newFakeMySQL(t)
	p := &mysqlProxy{
		cfg: config.MySQLService{Listen: "127.0.0.1:0", Upstream: upstream.addr, User: "u", Password: password},
		log: log,
	}
	if err := p.Start(); err != nil {
		t.Fatalf("mysql proxy start: %v", err)
	}
	t.Cleanup(func() { p.Stop(context.Background()) }) //nolint:errcheck

	conn, err := net.Dial("tcp", p.listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	if _, _, err := mysqlReadPacket(conn); err != nil {
		t.Fatalf("read forwarded handshake: %v", err)
	}
	// Well-formed handshake response (payload >= 4 bytes) so the proxy proceeds
	// through auth into the pipe.
	if err := mysqlWritePacket(conn, 1, make([]byte, 32)); err != nil {
		t.Fatalf("write client handshake response: %v", err)
	}
	// The fake upstream closes after OK, so the proxy closes the client; drain to EOF.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	io.ReadAll(conn)                                      //nolint:errcheck

	assertLifecycle(t, buf, "mysql", password)
}

// TestLifecycleLogging_Postgres drives a client through the postgres proxy
// (MD5 auth) to the pipe stage and asserts the lifecycle contract.
func TestLifecycleLogging_Postgres(t *testing.T) {
	const user, password, db = "proxyuser", "s3cr3t-postgres-pw", "testdb"
	log, buf := captureLogger()

	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamLn.Close() //nolint:errcheck
	go func() {
		for {
			conn, err := upstreamLn.Accept()
			if err != nil {
				return
			}
			go pgFakeUpstreamMD5(conn, user, password)
		}
	}()

	p := &postgresProxy{
		cfg: config.PostgreSQLService{Listen: "127.0.0.1:0", Upstream: upstreamLn.Addr().String(), User: user, Password: password, Database: db},
		log: log,
	}
	if err := p.Start(); err != nil {
		t.Fatalf("postgres proxy start: %v", err)
	}
	t.Cleanup(func() { p.Stop(context.Background()) }) //nolint:errcheck

	client, err := net.Dial("tcp", p.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := pgWriteStartup(client, pgBuildStartup("nobody", db)); err != nil {
		t.Fatal(err)
	}
	// Read AuthenticationOK, then the startup chain up to ReadyForQuery.
	for {
		msg, err := pgReadMessage(client)
		if err != nil {
			t.Fatalf("read startup chain: %v", err)
		}
		if msg[0] == 'Z' {
			break
		}
	}
	// Close the client so the pipe terminates and the close line is logged.
	client.Close() //nolint:errcheck

	assertLifecycle(t, buf, "postgres", password)
}

// TestLifecycleLogging_Oracle drives a client through the oracle proxy to the
// pipe stage and asserts the lifecycle contract.
func TestLifecycleLogging_Oracle(t *testing.T) {
	const user, password, service = "proxyuser", "s3cr3t-oracle-pw", "ORCLPDB1"
	log, buf := captureLogger()

	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamLn.Close() //nolint:errcheck
	go func() {
		for {
			conn, err := upstreamLn.Accept()
			if err != nil {
				return
			}
			go oracleFakeUpstream(conn, user)
		}
	}()

	p := &oracleProxy{
		cfg: config.OracleService{Listen: "127.0.0.1:0", Upstream: upstreamLn.Addr().String(), User: user, Password: password, Service: service},
		log: log,
	}
	if err := p.Start(); err != nil {
		t.Fatalf("oracle proxy start: %v", err)
	}
	t.Cleanup(func() { p.Stop(context.Background()) }) //nolint:errcheck

	client, err := net.Dial("tcp", p.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := tnsWrite(client, tnsConnect, tnsBuildConnect("ANYSVC")); err != nil {
		t.Fatal(err)
	}
	if pt, _, err := tnsRead(client); err != nil || pt != tnsAccept {
		t.Fatalf("read ACCEPT: pt=%d err=%v", pt, err)
	}
	if _, _, err := tnsRead(client); err != nil { // NS from upstream
		t.Fatalf("read NS: %v", err)
	}
	if err := tnsWrite(client, tnsData, tnsDataBody([]byte{0x00, 0x02})); err != nil {
		t.Fatal(err)
	}
	if err := tnsWrite(client, tnsData, tnsBuildO3LOG("wronguser")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tnsRead(client); err != nil { // dummy session key
		t.Fatalf("read session key: %v", err)
	}
	if err := tnsWrite(client, tnsData, tnsBuildO3AUTH("wronguser", make([]byte, 20))); err != nil {
		t.Fatal(err)
	}
	if pt, body, err := tnsRead(client); err != nil || pt != tnsData || !tnsIsAuthOK(body) {
		t.Fatalf("read auth OK: pt=%d err=%v", pt, err)
	}
	// Close the client so the pipe terminates and the close line is logged.
	client.Close() //nolint:errcheck

	assertLifecycle(t, buf, "oracle", password)
}
