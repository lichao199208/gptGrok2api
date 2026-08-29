package provider

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// ImagineSocket is a deliberately small RFC 6455 client. The image endpoint
// only needs text frames, so keeping this transport local avoids another
// runtime dependency for the Go service.
type ImagineSocket struct {
	URL     string
	Client  *http.Client
	Timeout time.Duration
	Proxy   *proxyruntime.Manager
}

func NewImagineSocket(urlValue string, client *http.Client, timeout time.Duration) *ImagineSocket {
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &ImagineSocket{URL: urlValue, Client: client, Timeout: timeout}
}

func (i *ImagineSocket) Generate(ctx context.Context, account accounts.Account, prompt, aspectRatio string, count int, enableNSFW, enablePro bool) ([]ImageResult, error) {
	conn, err := i.dial(ctx, account)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.writeText(mustJSON(protocol.BuildResetMessage())); err != nil {
		return nil, err
	}
	requestID := randomUUID()
	if err := conn.writeText(mustJSON(protocol.BuildImagineRequest(requestID, prompt, aspectRatio, enableNSFW, enablePro))); err != nil {
		return nil, err
	}
	results := make([]ImageResult, 0, count)
	seen := map[string]bool{}
	deadline := time.Now().Add(i.Timeout)
	for len(results) < count && time.Now().Before(deadline) {
		frame, err := conn.readText(ctx, deadline)
		if err != nil {
			return nil, err
		}
		if frame == "" {
			continue
		}
		var object map[string]any
		if json.Unmarshal([]byte(frame), &object) != nil {
			continue
		}
		switch fmt.Sprint(object["type"]) {
		case "error":
			return nil, fmt.Errorf("imagine websocket error: %s", fmt.Sprint(object["err_msg"]))
		case "image":
			urlValue, _ := object["url"].(string)
			blob, _ := object["blob"].(string)
			if blob == "" {
				continue
			}
			id := imageIDFromURL(urlValue)
			if seen[id] {
				continue
			}
			seen[id] = true
			result := ImageResult{URL: absoluteAssetURL(urlValue)}
			if raw, decodeErr := base64.StdEncoding.DecodeString(blob); decodeErr == nil {
				result.Base64 = base64.StdEncoding.EncodeToString(raw)
				result.MIME = "image/jpeg"
			}
			results = append(results, result)
		case "json":
			if fmt.Sprint(object["current_status"]) == "completed" && boolValue(object["moderated"]) {
				continue
			}
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("imagine websocket returned no images")
	}
	return results, nil
}

func (i *ImagineSocket) dial(ctx context.Context, account accounts.Account) (*wsConn, error) {
	parsed, err := url.Parse(i.URL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("invalid imagine websocket URL")
	}
	address := parsed.Host
	if !strings.Contains(address, ":") {
		if parsed.Scheme == "wss" {
			address += ":443"
		} else {
			address += ":80"
		}
	}
	proxyURL := ""
	if i.Proxy != nil {
		proxyURL = i.Proxy.Resolve(account.Fields, false)
	}
	network, err := proxyruntime.DialContext(ctx, address, proxyURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "wss" {
		tlsNetwork := tls.Client(network, &tls.Config{ServerName: parsed.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsNetwork.HandshakeContext(ctx); err != nil {
			_ = network.Close()
			return nil, err
		}
		network = tlsNetwork
	}
	conn := &wsConn{conn: network, read: bufio.NewReader(network)}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		network.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	cookie := firstString(account.Fields["cookie_header"], "")
	if cookie == "" {
		token := strings.TrimPrefix(account.Token, "sso=")
		cookie = "sso=" + token + "; sso-rw=" + token
	}
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nOrigin: https://grok.com\r\nReferer: https://grok.com/imagine\r\nCookie: %s\r\nUser-Agent: Mozilla/5.0\r\n\r\n", parsed.RequestURI(), parsed.Host, key, cookie)
	if _, err := io.WriteString(network, request); err != nil {
		network.Close()
		return nil, err
	}
	response, err := http.ReadResponse(conn.read, &http.Request{Method: http.MethodGet, URL: parsed})
	if err != nil {
		network.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		network.Close()
		return nil, fmt.Errorf("imagine websocket handshake returned HTTP %d", response.StatusCode)
	}
	accept := response.Header.Get("Sec-WebSocket-Accept")
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if accept != base64.StdEncoding.EncodeToString(hash[:]) {
		network.Close()
		return nil, fmt.Errorf("invalid websocket handshake")
	}
	return conn, nil
}

type wsConn struct {
	conn net.Conn
	read *bufio.Reader
}

func (w *wsConn) Close() error { return w.conn.Close() }

func (w *wsConn) writeText(value string) error {
	return w.writeFrame(0x1, []byte(value))
}

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	key := make([]byte, 4)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	header := []byte{0x80 | opcode}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, 0x80|byte(length))
	case length <= 65535:
		header = append(header, 0x80|126, byte(length>>8), byte(length))
	default:
		header = append(header, 0x80|127)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(length))
		header = append(header, buf[:]...)
	}
	header = append(header, key...)
	for index := range payload {
		payload[index] ^= key[index%4]
	}
	if _, err := w.conn.Write(header); err != nil {
		return err
	}
	_, err := w.conn.Write(payload)
	return err
}

func (w *wsConn) readText(ctx context.Context, deadline time.Time) (string, error) {
	if err := w.conn.SetReadDeadline(deadline); err != nil {
		return "", err
	}
	for {
		header := make([]byte, 2)
		if _, err := io.ReadFull(w.read, header); err != nil {
			return "", err
		}
		fin := header[0]&0x80 != 0
		opcode := header[0] & 0x0f
		masked := header[1]&0x80 != 0
		length := int64(header[1] & 0x7f)
		if length == 126 {
			var buf [2]byte
			if _, err := io.ReadFull(w.read, buf[:]); err != nil {
				return "", err
			}
			length = int64(binary.BigEndian.Uint16(buf[:]))
		}
		if length == 127 {
			var buf [8]byte
			if _, err := io.ReadFull(w.read, buf[:]); err != nil {
				return "", err
			}
			value := binary.BigEndian.Uint64(buf[:])
			if value > 32<<20 {
				return "", fmt.Errorf("websocket frame too large")
			}
			length = int64(value)
		}
		if length > 32<<20 {
			return "", fmt.Errorf("websocket frame too large")
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(w.read, mask[:]); err != nil {
				return "", err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(w.read, payload); err != nil {
			return "", err
		}
		if masked {
			for index := range payload {
				payload[index] ^= mask[index%4]
			}
		}
		if opcode == 0x8 {
			return "", io.EOF
		}
		if opcode == 0x9 {
			_ = w.writeFrame(0xA, payload)
			continue
		}
		if opcode != 0x1 && opcode != 0x0 {
			continue
		}
		if !fin {
			continue
		}
		return string(payload), nil
	}
}

func mustJSON(value map[string]any) string { raw, _ := json.Marshal(value); return string(raw) }

func randomUUID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func imageIDFromURL(value string) string {
	if value == "" {
		return randomUUID()
	}
	parts := strings.Split(strings.Trim(value, "/"), "/")
	last := parts[len(parts)-1]
	if dot := strings.IndexByte(last, '.'); dot > 0 {
		last = last[:dot]
	}
	return last
}

func boolValue(value any) bool { typed, _ := value.(bool); return typed }
