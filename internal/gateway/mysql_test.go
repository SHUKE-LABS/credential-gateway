package gateway

import (
	"bytes"
	"crypto/sha1"
	"testing"
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
	buf.WriteByte(21) // auth_plugin_data_len = 21
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
