package cdp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	wsGUID              = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxWSMessageBytes   = 128 * 1024 * 1024
	defaultWriteTimeout = 15 * time.Second
)

// WSConn is a minimal pure-stdlib RFC 6455 WebSocket client for local Chrome DevTools Protocol.
type WSConn struct {
	conn            net.Conn
	br              *bufio.Reader
	wmu             sync.Mutex
	maxMessageBytes uint64
	writeTimeout    time.Duration
}

// DialWS opens a client WebSocket connection to ws://host:port/path.
func DialWS(ctx context.Context, rawURL string) (*WSConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid websocket url %q: %w", rawURL, err)
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("unsupported websocket scheme %q (expected ws://)", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, err
	}
	secKey := base64.StdEncoding.EncodeToString(keyBytes)
	reqURI := u.RequestURI()
	if reqURI == "" {
		reqURI = "/"
	}

	handshake := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		reqURI, u.Host, secKey,
	)

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}

	if _, err := io.WriteString(conn, handshake); err != nil {
		_ = conn.Close()
		return nil, err
	}

	br := bufio.NewReaderSize(conn, 64*1024)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket upgrade failed: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket upgrade returned HTTP %d", resp.StatusCode)
	}

	h := sha1.Sum([]byte(secKey + wsGUID))
	wantAccept := base64.StdEncoding.EncodeToString(h[:])
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != wantAccept {
		_ = conn.Close()
		return nil, fmt.Errorf("invalid Sec-WebSocket-Accept %q", got)
	}

	_ = conn.SetDeadline(time.Time{})
	return &WSConn{conn: conn, br: br}, nil
}

// WriteText sends a masked UTF-8 text frame.
func (w *WSConn) WriteText(payload []byte) error {
	return w.writeFrame(0x1, payload)
}

func (w *WSConn) writeFrame(opcode byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()

	var hdr [14]byte
	hdr[0] = 0x80 | (opcode & 0x0f)
	n := len(payload)
	offset := 2
	switch {
	case n < 126:
		hdr[1] = 0x80 | byte(n)
	case n <= 65535:
		hdr[1] = 0x80 | 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		offset = 4
	default:
		hdr[1] = 0x80 | 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		offset = 10
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	copy(hdr[offset:offset+4], mask[:])
	offset += 4

	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}

	timeout := w.writeTimeout
	if timeout <= 0 {
		timeout = defaultWriteTimeout
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(timeout))
	defer func() { _ = w.conn.SetWriteDeadline(time.Time{}) }()

	if _, err := w.conn.Write(hdr[:offset]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := w.conn.Write(masked); err != nil {
			return err
		}
	}
	return nil
}

// ReadText reads the next complete text message, automatically replying to Ping frames and reassembling fragments.
func (w *WSConn) ReadText() ([]byte, error) {
	maxBytes := w.maxMessageBytes
	if maxBytes == 0 {
		maxBytes = maxWSMessageBytes
	}
	var msgBuf bytes.Buffer
	inMessage := false

	for {
		var b2 [2]byte
		if _, err := io.ReadFull(w.br, b2[:]); err != nil {
			return nil, err
		}
		fin := (b2[0] & 0x80) != 0
		opcode := b2[0] & 0x0f
		masked := (b2[1] & 0x80) != 0
		length := uint64(b2[1] & 0x7f)

		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return nil, err
			}
			length = binary.BigEndian.Uint64(ext[:])
		}

		if length > maxBytes {
			return nil, fmt.Errorf("websocket frame too large (%d bytes)", length)
		}

		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(w.br, mask[:]); err != nil {
				return nil, err
			}
		}

		data := make([]byte, int(length))
		if length > 0 {
			if _, err := io.ReadFull(w.br, data); err != nil {
				return nil, err
			}
		}
		if masked {
			for i := range data {
				data[i] ^= mask[i%4]
			}
		}

		switch opcode {
		case 0x8:
			return nil, io.EOF
		case 0x9:
			_ = w.writeFrame(0xA, data)
			continue
		case 0xA:
			continue
		case 0x1, 0x2:
			if inMessage {
				return nil, errors.New("unexpected websocket data frame during fragmented message")
			}
			msgBuf.Reset()
			msgBuf.Write(data)
			if fin {
				inMessage = false
				return append([]byte(nil), msgBuf.Bytes()...), nil
			}
			inMessage = true
		case 0x0:
			if !inMessage {
				return nil, errors.New("unexpected websocket continuation frame")
			}
			if uint64(msgBuf.Len())+uint64(len(data)) > maxBytes {
				return nil, fmt.Errorf("fragmented websocket message exceeds %d bytes", maxBytes)
			}
			msgBuf.Write(data)
			if fin {
				inMessage = false
				return append([]byte(nil), msgBuf.Bytes()...), nil
			}
		}
	}
}

// Close sends a close frame and closes the underlying TCP socket.
func (w *WSConn) Close() error {
	_ = w.writeFrame(0x8, nil)
	return w.conn.Close()
}
