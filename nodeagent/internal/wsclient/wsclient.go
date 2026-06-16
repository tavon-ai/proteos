// Package wsclient is a minimal, dependency-free RFC 6455 WebSocket client used
// only by the node-agent's host acceptance tests (Phase 9.6b) to drive a booted
// guest agent: open a cwd-scoped terminal session and round-trip the control
// channel (projects.list / kv.*). The node-agent module is deliberately
// dependency-free (no go.sum), so the production websocket libraries used by the
// control plane and guest agent are not available here — hence this small,
// self-contained client.
//
// It implements only what the acceptance test needs: a client handshake over an
// already-connected net.Conn (the node-agent guest tunnel), masked client frames,
// text/binary reads, automatic pong replies, and a clean close. It is NOT a
// general-purpose library and makes simplifying assumptions (single-frame
// messages, no continuation fragmentation on the read path beyond reassembly of
// the payload length).
package wsclient

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// wsGUID is the RFC 6455 magic value appended to Sec-WebSocket-Key to form the
// server's Sec-WebSocket-Accept.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxPayload bounds a single inbound message so a malformed length can't make the
// client allocate unbounded memory. PTY scrollback replay is the largest message
// the guest sends; 16 MiB is well above it.
const maxPayload = 16 << 20

// Conn is an open WebSocket connection to the guest.
type Conn struct {
	conn net.Conn
	r    *bufio.Reader
}

// Dial performs the client handshake over an already-established conn (the guest
// tunnel) for the given host + request path (e.g. "/terminal?session=x&cwd=/y").
// It returns a Conn ready for Read/Write, or an error if the upgrade is rejected.
func Dial(conn net.Conn, host, path string) (*Conn, error) {
	key, err := genKey()
	if err != nil {
		return nil, err
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, fmt.Errorf("write handshake: %w", err)
	}

	br := bufio.NewReaderSize(conn, 64<<10)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("read handshake response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("ws upgrade rejected: %s", resp.Status)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != AcceptKey(key) {
		return nil, fmt.Errorf("ws accept key mismatch: got %q", got)
	}
	return &Conn{conn: conn, r: br}, nil
}

// AcceptKey computes the server's Sec-WebSocket-Accept for a client key per RFC
// 6455. Exported so the white-box test can act as a server.
func AcceptKey(clientKey string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, clientKey+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// WriteText sends a masked text message.
func (c *Conn) WriteText(b []byte) error { return writeFrame(c.conn, opText, b, true) }

// WriteBinary sends a masked binary message.
func (c *Conn) WriteBinary(b []byte) error { return writeFrame(c.conn, opBinary, b, true) }

// Read returns the next text or binary message, transparently replying to pings
// and skipping pongs. A close frame surfaces as io.EOF. isText reports whether the
// message was a text frame.
func (c *Conn) Read() (payload []byte, isText bool, err error) {
	for {
		op, data, err := readFrame(c.r)
		if err != nil {
			return nil, false, err
		}
		switch op {
		case opText:
			return data, true, nil
		case opBinary:
			return data, false, nil
		case opPing:
			// Reply with a pong carrying the same payload (masked client frame).
			if err := writeFrame(c.conn, opPong, data, true); err != nil {
				return nil, false, err
			}
		case opPong:
			// ignore
		case opClose:
			return nil, false, io.EOF
		default:
			return nil, false, fmt.Errorf("unexpected opcode 0x%x", op)
		}
	}
}

// SetDeadline bounds the underlying conn so a stuck read/write fails the test
// instead of hanging it.
func (c *Conn) SetDeadline(t time.Time) error { return c.conn.SetDeadline(t) }

// Close sends a close frame (best-effort) and returns. The caller still closes
// the underlying tunnel conn.
func (c *Conn) Close() error {
	_ = writeFrame(c.conn, opClose, nil, true)
	return nil
}

// --- framing -----------------------------------------------------------------

// writeFrame writes a single, unfragmented frame. When mask is true (the client
// side) the payload is XOR-masked with a random 4-byte key, as RFC 6455 requires
// for client→server frames.
func writeFrame(w io.Writer, opcode byte, payload []byte, mask bool) error {
	var header []byte
	header = append(header, 0x80|opcode) // FIN=1
	n := len(payload)
	maskBit := byte(0)
	if mask {
		maskBit = 0x80
	}
	switch {
	case n <= 125:
		header = append(header, maskBit|byte(n))
	case n <= 0xFFFF:
		header = append(header, maskBit|126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		header = append(header, ext[:]...)
	default:
		header = append(header, maskBit|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}

	body := payload
	if mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		header = append(header, key[:]...)
		body = make([]byte, n)
		for i := range n {
			body[i] = payload[i] ^ key[i%4]
		}
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			return err
		}
	}
	return nil
}

// readFrame reads one frame, unmasking it if the mask bit is set (the server side
// of a test never masks, but be permissive). It rejects oversized payloads.
func readFrame(r *bufio.Reader) (opcode byte, payload []byte, err error) {
	b0, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	opcode = b0 & 0x0F
	// This minimal client does not reassemble fragmented messages; the guest
	// sends each message as a single FIN frame.
	if b0&0x80 == 0 && opcode == opContinuation {
		return 0, nil, errors.New("wsclient: unexpected continuation frame")
	}

	b1, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	masked := b1&0x80 != 0
	length := uint64(b1 & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > maxPayload {
		return 0, nil, fmt.Errorf("wsclient: payload too large (%d)", length)
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(r, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, payload, nil
}

// genKey returns a fresh base64 Sec-WebSocket-Key (16 random bytes).
func genKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}
