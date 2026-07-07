package gateway

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"credential-gateway/internal/config"
)

func TestMysqlReadWritePacket(t *testing.T) {
	payload := []byte("hello, mysql")
	var buf bytes.Buffer
	if err := mysqlWritePacket(&buf, 0, payload); err != nil {
		t.Fatal(err)
	}

	seq, got, err := mysqlReadPacket(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Errorf("seq: got %d, want 0", seq)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %q, want %q", got, payload)
	}
}

func TestMysqlScramble(t *testing.T) {
	// Verify against the reference formula:
	// SHA1(pw) XOR SHA1(nonce + SHA1(SHA1(pw)))
	password := "s3cr3t"
	nonce := make([]byte, 20)
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}

	got := mysqlScramble(nonce, password)

	h1 := sha1.Sum([]byte(password))
	h2 := sha1.Sum(h1[:])
	h := sha1.New()
	h.Write(nonce)
	h.Write(h2[:])
	s := h.Sum(nil)
	want := make([]byte, 20)
	for i := range want {
		want[i] = h1[i] ^ s[i]
	}

	if !bytes.Equal(got, want) {
		t.Errorf("scramble mismatch:\n got  %x\n want %x", got, want)
	}
}

func TestMysqlScrambleEmptyPassword(t *testing.T) {
	if got := mysqlScramble(make([]byte, 20), ""); got != nil {
		t.Errorf("expected nil for empty password, got %x", got)
	}
}

func TestMysqlBuildResponseContainsUser(t *testing.T) {
	nonce := make([]byte, 20)
	resp := mysqlBuildResponse("alice", "pw", nonce, "")
	if !bytes.Contains(resp, []byte("alice")) {
		t.Error("response does not contain username")
	}
	if !bytes.Contains(resp, []byte("mysql_native_password")) {
		t.Error("response does not contain auth plugin name")
	}
}

func TestMysqlBuildResponseWithDatabase(t *testing.T) {
	nonce := make([]byte, 20)
	resp := mysqlBuildResponse("alice", "pw", nonce, "mydb")
	if !bytes.Contains(resp, []byte("mydb")) {
		t.Error("response does not contain database name")
	}
}

// buildHandshakeV10 constructs a minimal HandshakeV10 payload for testing.
func buildHandshakeV10(serverVersion string, nonce []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x0a) // protocol version
	buf.WriteString(serverVersion)
	buf.WriteByte(0) // null terminator
	// connection ID
	buf.Write([]byte{1, 0, 0, 0})
	// auth-plugin-data part 1 (8 bytes)
	buf.Write(nonce[:8])
	buf.WriteByte(0) // filler
	// capability_flags_1 (CLIENT_SSL=0x0800 set so we can test clearing it)
	capLow := uint16(0x0800 | 0xf7ff)
	buf.WriteByte(byte(capLow))
	buf.WriteByte(byte(capLow >> 8))
	buf.WriteByte(45)       // character set
	buf.Write([]byte{2, 0}) // status flags
	// capability_flags_2 with CLIENT_PLUGIN_AUTH (0x0800 in upper half)
	capHigh := uint16(0x0800)
	buf.WriteByte(byte(capHigh))
	buf.WriteByte(byte(capHigh >> 8))
	buf.WriteByte(21)           // auth_plugin_data_len = 21
	buf.Write(make([]byte, 10)) // reserved
	// auth-plugin-data part 2 (13 bytes: 12 + null)
	buf.Write(nonce[8:20])
	buf.WriteByte(0) // null terminator
	buf.WriteString("mysql_native_password")
	buf.WriteByte(0)
	return buf.Bytes()
}

func TestMysqlParseNonce(t *testing.T) {
	want := make([]byte, 20)
	for i := range want {
		want[i] = byte(i + 1)
	}

	payload := buildHandshakeV10("8.0.31", want)
	got, err := mysqlParseNonce(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("nonce mismatch:\n got  %x\n want %x", got, want)
	}
}

// fakeMySQL is a minimal MySQL server for testing: it sends a HandshakeV10,
// reads the client's HandshakeResponse, and replies with an OK packet — or,
// when authSwitchTo is set, an AuthSwitchRequest (0xfe) for that plugin.
type fakeMySQL struct {
	ln           net.Listener
	addr         string
	authSwitchTo string
}

func newFakeMySQL(t *testing.T) *fakeMySQL {
	return newFakeMySQLAuthSwitch(t, "")
}

// newFakeMySQLAuthSwitch builds a fake upstream that replies to the auth attempt
// with an AuthSwitchRequest for plugin instead of an OK packet.
func newFakeMySQLAuthSwitch(t *testing.T, plugin string) *fakeMySQL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeMySQL listen: %v", err)
	}
	fm := &fakeMySQL{ln: ln, addr: ln.Addr().String(), authSwitchTo: plugin}
	go fm.serve()
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	return fm
}

func (f *fakeMySQL) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeMySQL) handle(conn net.Conn) {
	defer conn.Close() //nolint:errcheck
	handshake := buildHandshakeV10("8.0.0", make([]byte, 20))
	if err := mysqlWritePacket(conn, 0, handshake); err != nil {
		return
	}
	if _, _, err := mysqlReadPacket(conn); err != nil {
		return
	}
	if f.authSwitchTo != "" {
		// AuthSwitchRequest: 0xfe + null-terminated plugin name + auth data.
		var b bytes.Buffer
		b.WriteByte(0xfe)
		b.WriteString(f.authSwitchTo)
		b.WriteByte(0)
		b.Write(make([]byte, 21))            // 20-byte nonce + null (auth-plugin-data)
		mysqlWritePacket(conn, 2, b.Bytes()) //nolint:errcheck
		return
	}
	// OK packet: payload begins with 0x00 (not 0xff/0xfe), so the proxy treats
	// auth as accepted and enters the bidirectional pipe.
	mysqlWritePacket(conn, 2, []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}) //nolint:errcheck
}

func startMySQLProxy(t *testing.T, upstream string) *mysqlProxy {
	t.Helper()
	p := &mysqlProxy{
		cfg: config.MySQLService{
			Listen:   "127.0.0.1:0",
			Upstream: upstream,
			User:     "u",
			Password: "p",
		},
		log: testLogger(),
	}
	if err := p.Start(); err != nil {
		t.Fatalf("mysql proxy start: %v", err)
	}
	t.Cleanup(func() { p.Stop(context.Background()) }) //nolint:errcheck
	return p
}

// TestMysqlProxy_ShortHandshakeResponseSurvives feeds the proxy a well-formed
// client frame whose declared payload length is < 4. Before the fix, the
// unguarded clientResp[:4] slice panics in the per-connection goroutine and
// crashes the whole process (aborting this test binary). The connection must
// instead be closed with a logged error while the listener keeps serving.
func TestMysqlProxy_ShortHandshakeResponseSurvives(t *testing.T) {
	upstream := newFakeMySQL(t)
	p := startMySQLProxy(t, upstream.addr)
	addr := p.listener.Addr().String()

	conn1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn1.Close() //nolint:errcheck

	// Read the server handshake the proxy forwards from upstream.
	if _, _, err := mysqlReadPacket(conn1); err != nil {
		t.Fatalf("read forwarded handshake: %v", err)
	}

	// Well-formed frame, 2-byte payload (declared length 2 < 4): 02 00 00 01 aa bb.
	if _, err := conn1.Write([]byte{0x02, 0x00, 0x00, 0x01, 0xaa, 0xbb}); err != nil {
		t.Fatalf("write short frame: %v", err)
	}

	// The proxy rejects the short frame and closes the connection (mysql.go
	// returns, firing defer client.Close()). The next read must see EOF/error.
	if err := conn1.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := conn1.Read(make([]byte, 1)); err == nil {
		t.Error("expected proxy to close the connection after a short handshake response")
	}

	// The listener must still be alive: a fresh connection completes handshake.
	conn2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("listener did not survive (dial): %v", err)
	}
	defer conn2.Close() //nolint:errcheck
	if err := conn2.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := mysqlReadPacket(conn2); err != nil {
		t.Fatalf("listener did not survive (read handshake): %v", err)
	}
}

// TestMysqlProxy_AuthSwitchReturnsCleanErr drives a client through the proxy to
// a fake upstream that requests an auth-switch to caching_sha2_password. The
// proxy must NOT relay the raw 0xfe and close; it must return a well-formed
// MySQL ERR packet that a real client decodes cleanly. The assertions decode the
// wire packet the way a driver does (header, seq, framing) rather than checking
// resp[0] == 0xff.
func TestMysqlProxy_AuthSwitchReturnsCleanErr(t *testing.T) {
	upstream := newFakeMySQLAuthSwitch(t, "caching_sha2_password")
	p := startMySQLProxy(t, upstream.addr)
	addr := p.listener.Addr().String()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	// Read the server handshake the proxy forwards from upstream (seq 0).
	if _, _, err := mysqlReadPacket(conn); err != nil {
		t.Fatalf("read forwarded handshake: %v", err)
	}

	// Send a minimal HandshakeResponse41 (CLIENT_PROTOCOL_41 set) at seq 1. The
	// proxy only reads the capability flags and (absent) database out of it.
	const clientProtocol41 = 0x00000200
	clientResp := make([]byte, 32) // caps(4) + max pkt(4) + charset(1) + reserved(23)
	binary.LittleEndian.PutUint32(clientResp, clientProtocol41)
	if err := mysqlWritePacket(conn, 1, clientResp); err != nil {
		t.Fatalf("write client handshake response: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	// Decode the 4-byte packet header directly and verify the sequence number
	// follows the client's response (seq 1 + 1 = 2).
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatalf("read err packet header: %v", err)
	}
	length := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	if seq := hdr[3]; seq != 2 {
		t.Errorf("err packet seq: got %d, want 2 (client response seq 1 + 1)", seq)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read err packet payload: %v", err)
	}

	// 0xff + 2-byte code + '#' + 5-byte SQLSTATE + message.
	if len(payload) < 9 {
		t.Fatalf("err packet too short: %d bytes (%x)", len(payload), payload)
	}
	if payload[0] != 0xff {
		t.Errorf("err header: got 0x%02x, want 0xff", payload[0])
	}
	if code := uint16(payload[1]) | uint16(payload[2])<<8; code != 1251 {
		t.Errorf("err code: got %d, want 1251", code)
	}
	if payload[3] != '#' {
		t.Errorf("sqlstate marker: got 0x%02x, want '#'", payload[3])
	}
	if sqlState := string(payload[4:9]); sqlState != "08004" {
		t.Errorf("sqlstate: got %q, want 08004", sqlState)
	}
	msg := string(payload[9:])
	if !strings.Contains(msg, "caching_sha2_password") {
		t.Errorf("err message does not name the requested plugin: %q", msg)
	}
	if !strings.Contains(msg, "mysql_native_password") {
		t.Errorf("err message does not name the required plugin: %q", msg)
	}

	// The listener must survive: a fresh connection still gets the handshake.
	conn2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("listener did not survive (dial): %v", err)
	}
	defer conn2.Close() //nolint:errcheck
	if err := conn2.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := mysqlReadPacket(conn2); err != nil {
		t.Fatalf("listener did not survive (read handshake): %v", err)
	}
}

func TestMysqlClearSSL(t *testing.T) {
	nonce := make([]byte, 20)
	payload := buildHandshakeV10("8.0.31", nonce)

	// Confirm CLIENT_SSL is set before clearing.
	pos := 1
	for pos < len(payload) && payload[pos] != 0 {
		pos++
	}
	pos += 1 + 4 + 8 + 1 // null + connection ID + auth part 1 + filler
	capLow := uint16(payload[pos]) | uint16(payload[pos+1])<<8
	if capLow&0x0800 == 0 {
		t.Fatal("test setup: CLIENT_SSL not set before clear")
	}

	mysqlClearSSL(payload)

	capLow = uint16(payload[pos]) | uint16(payload[pos+1])<<8
	if capLow&0x0800 != 0 {
		t.Error("CLIENT_SSL still set after mysqlClearSSL")
	}
}
