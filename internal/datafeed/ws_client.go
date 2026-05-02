package datafeed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

const (
	pingInterval = 30 * time.Second
	maxStreams   = 200
	backoffBase  = time.Second
	backoffMax   = 30 * time.Second
)

// WSMessage 原始 Binance WS 消息
type WSMessage struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

type StreamHandler func(msg json.RawMessage, localTime int64)

// WSClient 管理单个 WebSocket 连接（≤5 streams）
type WSClient struct {
	endpoint string
	streams  []string
	handlers map[string]StreamHandler
	mu       sync.RWMutex
	conn     *websocket.Conn
}

func NewWSClient(endpoint string, streams []string, handlers map[string]StreamHandler) (*WSClient, error) {
	if len(streams) > maxStreams {
		return nil, fmt.Errorf("stream count %d exceeds limit %d", len(streams), maxStreams)
	}
	return &WSClient{
		endpoint: endpoint,
		streams:  streams,
		handlers: handlers,
	}, nil
}

func (c *WSClient) SetStreams(streams []string, handlers map[string]StreamHandler) error {
	if len(streams) > maxStreams {
		return fmt.Errorf("stream count %d exceeds limit %d", len(streams), maxStreams)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// 如果新 streams ⊆ 老 streams，仅更新 handlers，不重连（避免无谓断线）
	if streamsSubset(streams, c.streams) {
		c.handlers = handlers
		return nil
	}

	c.streams = streams
	c.handlers = handlers
	if c.conn != nil {
		_ = c.conn.Close() // 触发 connect() 退出并重连
	}
	return nil
}

// streamsSubset reports whether all elements of sub are present in super.
func streamsSubset(sub, super []string) bool {
	if len(sub) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(super))
	for _, s := range super {
		set[s] = struct{}{}
	}
	for _, s := range sub {
		if _, ok := set[s]; !ok {
			return false
		}
	}
	return true
}

func (c *WSClient) Run(ctx context.Context) {
	backoff := backoffBase
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := c.connect(ctx); err != nil {
			log.Error().Err(err).Msg("ws connect failed, retrying")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = minDuration(backoff*2, backoffMax)
			continue
		}
		backoff = backoffBase
	}
}

func (c *WSClient) connect(ctx context.Context) error {
	c.mu.RLock()
	streams := append([]string(nil), c.streams...)
	c.mu.RUnlock()

	// 尚未设置交易对，等待前端验证后再握手
	if len(streams) == 0 {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
		return fmt.Errorf("waiting for symbol to be configured via webui")
	}

	// Binance Futures combined stream 正确路径：/market/stream?streams=...
	// 旧路径 /stream?streams= 已废弃，Binance 会静默不推数据
	path := "/market/stream?streams=" + joinStreams(streams)
	u, err := url.Parse(c.endpoint + path)
	if err != nil {
		return err
	}

	log.Info().Str("url", u.String()).Msg("ws dialing")
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		log.Error().Err(err).Str("url", u.String()).Msg("ws handshake failed")
		return err
	}
	log.Info().Str("url", u.String()).Msg("ws connected")
	defer conn.Close()

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	msgCh := make(chan wsRaw, 256)
	go c.readPump(conn, msgCh)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pingTicker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return fmt.Errorf("ping failed: %w", err)
			}
		case raw, ok := <-msgCh:
			if !ok {
				return fmt.Errorf("connection closed")
			}
			c.dispatch(raw)
		}
	}
}

type wsRaw struct {
	data      []byte
	localTime int64
}

func (c *WSClient) readPump(conn *websocket.Conn, ch chan<- wsRaw) {
	defer close(ch)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Warn().Err(err).Msg("ws read error")
			return
		}
		ch <- wsRaw{data: msg, localTime: time.Now().UnixMilli()}
	}
}

func (c *WSClient) dispatch(raw wsRaw) {
	var msg WSMessage
	if err := json.Unmarshal(raw.data, &msg); err != nil {
		log.Warn().Err(err).Msg("ws message parse failed, skipping")
		return
	}
	c.mu.RLock()
	handler, ok := c.handlers[msg.Stream]
	if !ok {
		handler, ok = c.handlers[strings.ToLower(msg.Stream)]
	}
	c.mu.RUnlock()
	if ok {
		handler(msg.Data, raw.localTime)
		return
	}
	if msg.Stream == "" {
		log.Warn().RawJSON("message", raw.data).Msg("ws message missing stream, skipping")
		return
	}
	log.Warn().Str("stream", msg.Stream).Msg("ws stream has no handler, skipping")
}

func joinStreams(streams []string) string {
	result := ""
	for i, s := range streams {
		if i > 0 {
			result += "/"
		}
		result += s
	}
	return result
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
