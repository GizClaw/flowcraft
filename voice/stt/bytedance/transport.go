package bytedance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"net/url"

	"github.com/coder/websocket"
)

// wsConn wraps a coder/websocket connection with goroutine-safe read/write
// and the ByteDance binary framing protocol.
type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex // serialises writes
}

func dialWSConn(ctx context.Context, url string, headers http.Header) (*wsConn, error) {
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: headers,
	})
	if err != nil {
		return nil, fmt.Errorf("bytedance stt: dial %s: %w", redactURL(url), err)
	}
	conn.SetReadLimit(4 * 1024 * 1024) // 4 MB
	return &wsConn{conn: conn}, nil
}

func (c *wsConn) close() error {
	return c.conn.Close(websocket.StatusNormalClosure, "")
}

// writeMessage packs a binary protocol message and sends it.
func (c *wsConn) writeMessage(ctx context.Context, msgType uint32, seq int32, data []byte, compress bool) error {
	framed, err := packMessage(msgType, seq, data, compress)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Write(ctx, websocket.MessageBinary, framed)
}

// readFrame reads one binary frame, unpacks the header and decompresses if needed.
func (c *wsConn) readFrame(ctx context.Context) (frameHeader, []byte, error) {
	_, raw, err := c.conn.Read(ctx)
	if err != nil {
		return frameHeader{}, nil, err
	}

	hdr, payload, err := unpackFrame(raw)
	if err != nil {
		return frameHeader{}, nil, err
	}

	if hdr.isGZip() && len(payload) > 0 {
		payload, err = gzipDecompress(payload)
		if err != nil {
			return frameHeader{}, nil, err
		}
	}

	return hdr, payload, nil
}

// --- ASR session ---

type asrSession struct {
	conn *wsConn
	seq  int32
}

func dialASR(ctx context.Context, appID, token, host string, async bool) (*asrSession, error) {
	path := "/api/v3/sauc/bigmodel_nostream"
	if async {
		path = "/api/v3/sauc/bigmodel_async"
	}
	scheme := "wss://"
	if strings.HasPrefix(host, "ws://") || strings.HasPrefix(host, "wss://") {
		scheme = ""
	}
	url := scheme + host + path
	reqID := generateReqID()

	headers := http.Header{
		"X-Api-Resource-Id": {"volc.bigasr.sauc.duration"},
		"X-Api-Request-Id":  {reqID},
		"X-Api-Access-Key":  {token},
		"X-Api-App-Key":     {appID},
	}

	conn, err := dialWSConn(ctx, url, headers)
	if err != nil {
		return nil, err
	}

	return &asrSession{conn: conn}, nil
}

func (s *asrSession) close() error {
	return s.conn.close()
}

func (s *asrSession) sendFullRequest(ctx context.Context, req asrRequestPayload) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	s.seq = 1
	return s.conn.writeMessage(ctx, msgTypeClientFull, s.seq, data, true)
}

func (s *asrSession) sendAudio(ctx context.Context, data []byte) error {
	s.seq++
	return s.conn.writeMessage(ctx, msgTypeClientAudioOnly, s.seq, data, true)
}

func (s *asrSession) sendFinish(ctx context.Context) error {
	s.seq++
	return s.conn.writeMessage(ctx, msgTypeClientAudioOnly, -s.seq, nil, true)
}

func (s *asrSession) read(ctx context.Context) (*asrResponse, error) {
	hdr, payload, err := s.conn.readFrame(ctx)
	if err != nil {
		status := websocket.CloseStatus(err)
		if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
			return &asrResponse{isLast: true}, nil
		}
		return nil, err
	}
	return decodeASRResponse(hdr, payload)
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
