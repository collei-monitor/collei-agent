package fileapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/collei-monitor/collei-agent/internal/auth"
)

// Manager 管理文件 API 的 WebSocket 连接。
type Manager struct {
	ctx      context.Context
	apiURL   string
	token    string
	verifier *auth.Verifier

	mu         sync.Mutex
	wanted     bool
	connected  bool
	loopCancel context.CancelFunc
	doneCh     chan struct{}
}

// NewManager 创建一个新的文件 API WebSocket 管理器。
func NewManager(ctx context.Context, apiURL, token string, verifier *auth.Verifier) *Manager {
	return &Manager{
		ctx:      ctx,
		apiURL:   strings.TrimRight(apiURL, "/"),
		token:    token,
		verifier: verifier,
	}
}

// IsConnected 报告文件 API WebSocket 是否当前正常。
func (m *Manager) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// Connect 请求文件 API WebSocket 连接。
func (m *Manager) Connect() {
	m.mu.Lock()
	if m.wanted {
		m.mu.Unlock()
		return
	}
	m.wanted = true
	m.mu.Unlock()

	slog.Info("FileAPI: connect requested, starting...")
	m.startLoop()
}

// Disconnect 请求断开连接。
func (m *Manager) Disconnect() {
	m.mu.Lock()
	if !m.wanted {
		m.mu.Unlock()
		return
	}
	m.wanted = false
	m.mu.Unlock()

	slog.Info("FileAPI: disconnect requested, closing...")
	m.signalStop()
}

// Stop 完全关闭管理器。
func (m *Manager) Stop() {
	m.mu.Lock()
	m.wanted = false
	m.mu.Unlock()
	m.signalStop()
	time.Sleep(200 * time.Millisecond)
}

func (m *Manager) signalStop() {
	m.mu.Lock()
	cancel := m.loopCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) startLoop() {
	m.mu.Lock()
	if m.doneCh != nil {
		select {
		case <-m.doneCh:
		default:
			m.mu.Unlock()
			return
		}
	}
	loopCtx, cancel := context.WithCancel(m.ctx)
	m.loopCancel = cancel
	m.doneCh = make(chan struct{})
	m.mu.Unlock()

	go m.maintainConnection(loopCtx)
}

func (m *Manager) wsURL() string {
	base := m.apiURL
	var wsBase string
	if strings.HasPrefix(base, "https://") {
		wsBase = "wss://" + base[len("https://"):]
	} else if strings.HasPrefix(base, "http://") {
		wsBase = "ws://" + base[len("http://"):]
	} else {
		wsBase = "ws://" + base
	}
	return wsBase + "/api/v1/agent/ws/files?token=" + m.token
}

// maintainConnection reconnects with exponential backoff.
func (m *Manager) maintainConnection(ctx context.Context) {
	delay := 1.0

	defer func() {
		m.mu.Lock()
		m.connected = false
		close(m.doneCh)
		m.mu.Unlock()
		slog.Info("FileAPI: exited")
	}()

	for {
		m.mu.Lock()
		wanted := m.wanted
		m.mu.Unlock()
		if !wanted {
			return
		}

		url := m.wsURL()
		slog.Info("FileAPI: connecting", "url", strings.Split(url, "?")[0])

		dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
		ws, _, err := dialer.Dial(url, http.Header{})
		if err != nil {
			slog.Warn("FileAPI: connection failed", "error", err)
		} else {
			m.mu.Lock()
			m.connected = true
			m.mu.Unlock()
			delay = 1.0
			slog.Info("FileAPI: WebSocket connected")

			// Send capabilities
			capMsg, _ := json.Marshal(map[string]interface{}{
				"type": "capabilities",
				"operations": []string{
					"readdir", "stat", "read", "write",
					"remove", "rename", "mkdir", "rmdir",
				},
			})
			if err := ws.WriteMessage(websocket.TextMessage, capMsg); err != nil {
				slog.Warn("FileAPI: failed to send capabilities", "error", err)
			} else {
				slog.Debug("FileAPI: capabilities sent")
				m.handleMessages(ws, ctx)
			}
			ws.Close()
			m.mu.Lock()
			m.connected = false
			m.mu.Unlock()
		}

		m.mu.Lock()
		wanted = m.wanted
		m.mu.Unlock()
		if !wanted {
			return
		}

		jitter := rand.Float64() * delay * 0.1
		waitTime := delay + jitter
		slog.Info("FileAPI: reconnecting", "delay_sec", fmt.Sprintf("%.1f", waitTime))

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(waitTime * float64(time.Second))):
		}

		delay = delay * 2
		if delay > 30.0 {
			delay = 30.0
		}
	}
}

// handleMessages processes the file API WebSocket message loop.
func (m *Manager) handleMessages(ws *websocket.Conn, ctx context.Context) {
	// Track pending write: when a write command is received, we need to
	// capture the next binary frame as the file payload.
	var pendingWrite map[string]interface{}

	go func() {
		<-ctx.Done()
		ws.Close()
	}()

	for {
		msgType, msg, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				slog.Info("FileAPI: WebSocket closed normally")
				return
			}
			if ctx.Err() != nil {
				return
			}
			slog.Warn("FileAPI: read error", "error", err)
			return
		}

		// Binary frame → file write data
		if msgType == websocket.BinaryMessage {
			if pendingWrite != nil {
				executeWrite(ws, pendingWrite, msg)
				pendingWrite = nil
			}
			continue
		}

		// Text frame → JSON control message
		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			slog.Warn("FileAPI: invalid JSON received")
			continue
		}

		msgTypeStr, _ := data["type"].(string)

		switch msgTypeStr {
		case "readdir":
			handleReaddir(ws, data)

		case "stat":
			handleStat(ws, data)

		case "read":
			handleRead(ws, data)

		case "write":
			if handleWrite(ws, data, m.verifier) {
				pendingWrite = data
			}

		case "remove":
			handleRemove(ws, data, m.verifier)

		case "rename":
			handleRename(ws, data, m.verifier)

		case "mkdir":
			handleMkdir(ws, data, m.verifier)

		case "rmdir":
			handleRmdir(ws, data, m.verifier)

		case "disconnect":
			slog.Info("FileAPI: received disconnect command")
			m.mu.Lock()
			m.wanted = false
			m.mu.Unlock()
			return

		case "ping":
			pong, _ := json.Marshal(map[string]string{"type": "pong"})
			ws.WriteMessage(websocket.TextMessage, pong)

		default:
			slog.Debug("FileAPI: unknown message type", "type", msgTypeStr)
		}
	}
}
