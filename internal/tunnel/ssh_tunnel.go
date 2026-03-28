package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// SSHTunnel 管理 session_id → TCP 连接的映射。
type SSHTunnel struct {
	mu       sync.Mutex
	sessions map[string]net.Conn
}

func newSSHTunnel() *SSHTunnel {
	return &SSHTunnel{sessions: make(map[string]net.Conn)}
}

func (t *SSHTunnel) open(sessionID string, port int) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.sessions[sessionID] = conn
	t.mu.Unlock()
	return conn, nil
}

func (t *SSHTunnel) get(sessionID string) net.Conn {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessions[sessionID]
}

func (t *SSHTunnel) close(sessionID string) {
	t.mu.Lock()
	conn, ok := t.sessions[sessionID]
	if ok {
		delete(t.sessions, sessionID)
	}
	t.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

func (t *SSHTunnel) closeAll() {
	t.mu.Lock()
	for id, conn := range t.sessions {
		conn.Close()
		delete(t.sessions, id)
	}
	t.mu.Unlock()
}

// Manager 管理后端 WebSocket 隧道，用于 Web SSH。
type Manager struct {
	ctx     context.Context
	apiURL  string
	token   string
	sshPort int

	mu         sync.Mutex
	wanted     bool
	connected  bool
	loopCancel context.CancelFunc // cancels the current maintainTunnel loop
	doneCh     chan struct{}      // closed when maintainTunnel exits
}

// NewManager 创建一个新的 SSH 隧道管理器。
func NewManager(ctx context.Context, apiURL, token string, sshPort int) *Manager {
	return &Manager{
		ctx:     ctx,
		apiURL:  strings.TrimRight(apiURL, "/"),
		token:   token,
		sshPort: sshPort,
	}
}

// IsConnected 返回隧道是否当前处于活动状态。
func (m *Manager) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// Connect 请求建立隧道（由 ssh_tunnel.connect=true 触发）。
func (m *Manager) Connect() {
	m.mu.Lock()
	if m.wanted {
		m.mu.Unlock()
		return
	}
	m.wanted = true
	m.mu.Unlock()

	slog.Info("SSH tunnel: connect requested, starting tunnel...")
	m.startLoop()
}

// Disconnect 请求断开隧道（由 ssh_tunnel.connect=false 触发）。
func (m *Manager) Disconnect() {
	m.mu.Lock()
	if !m.wanted {
		m.mu.Unlock()
		return
	}
	m.wanted = false
	m.mu.Unlock()

	slog.Info("SSH tunnel: disconnect requested, closing tunnel...")
	m.signalStop()
}

// Stop 完全关闭管理器。
func (m *Manager) Stop() {
	m.mu.Lock()
	m.wanted = false
	m.mu.Unlock()
	m.signalStop()
	// 给协程一点时间退出
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
			// 上一个协程已停止
		default:
			m.mu.Unlock()
			return // 仍在运行
		}
	}
	loopCtx, cancel := context.WithCancel(m.ctx)
	m.loopCancel = cancel
	m.doneCh = make(chan struct{})
	m.mu.Unlock()

	go m.maintainTunnel(loopCtx)
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
	return wsBase + "/api/v1/agent/ws/ssh?token=" + m.token
}

// maintainTunnel 使用指数退避策略连接和重连。
func (m *Manager) maintainTunnel(ctx context.Context) {
	delay := 1.0

	defer func() {
		m.mu.Lock()
		m.connected = false
		close(m.doneCh)
		m.mu.Unlock()
		slog.Info("SSH tunnel: exited")
	}()

	for {
		m.mu.Lock()
		wanted := m.wanted
		m.mu.Unlock()
		if !wanted {
			return
		}

		url := m.wsURL()
		slog.Info("SSH tunnel: connecting", "url", strings.Split(url, "?")[0])

		dialer := websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
			// Use default TLS from http.DefaultTransport
		}
		header := http.Header{}
		ws, _, err := dialer.Dial(url, header)
		if err != nil {
			slog.Warn("SSH tunnel: connection failed", "error", err)
		} else {
			m.mu.Lock()
			m.connected = true
			m.mu.Unlock()
			delay = 1.0
			slog.Info("SSH tunnel: WebSocket connected")

			// 发送能力声明
			capMsg, _ := json.Marshal(map[string]interface{}{
				"type":     "capabilities",
				"ssh_port": m.sshPort,
			})
			if err := ws.WriteMessage(websocket.TextMessage, capMsg); err != nil {
				slog.Warn("SSH tunnel: failed to send capabilities", "error", err)
			} else {
				slog.Debug("SSH tunnel: capabilities sent", "ssh_port", m.sshPort)
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

		// 指数退避：min(initial * 2^n, 30s) + 抖动
		jitter := rand.Float64() * delay * 0.1
		waitTime := delay + jitter
		slog.Info("SSH tunnel: reconnecting", "delay_sec", fmt.Sprintf("%.1f", waitTime))

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

// handleMessages 处理 WebSocket 消息循环。
func (m *Manager) handleMessages(ws *websocket.Conn, ctx context.Context) {
	tun := newSSHTunnel()

	// 跟踪活动的桥接协程
	var wg sync.WaitGroup
	var pendingSessionID string

	defer func() {
		tun.closeAll()
		wg.Wait()
		slog.Debug("SSH tunnel: message loop ended, all sessions cleaned up")
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// 设置读取超时以便定期检查停止通道
		ws.SetReadDeadline(time.Now().Add(30 * time.Second))
		msgType, msg, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				slog.Info("SSH tunnel: WebSocket closed normally")
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // 读取超时，检查停止信号并继续循环
			}
			slog.Warn("SSH tunnel: read error", "error", err)
			return
		}

		// 二进制帧 → 转发到待处理会话的 TCP 套接字
		if msgType == websocket.BinaryMessage {
			if pendingSessionID != "" {
				conn := tun.get(pendingSessionID)
				if conn != nil {
					if _, writeErr := conn.Write(msg); writeErr != nil {
						slog.Warn("SSH tunnel: TCP write failed",
							"session", pendingSessionID, "error", writeErr)
						sendTunnelClosed(ws, pendingSessionID, "tcp_write_error")
						tun.close(pendingSessionID)
					}
				}
				pendingSessionID = ""
			}
			continue
		}

		// 文本帧 → JSON 控制消息
		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			slog.Warn("SSH tunnel: invalid JSON received")
			continue
		}

		msgTypeStr, _ := data["type"].(string)

		switch msgTypeStr {
		case "open_tunnel":
			sessionID, _ := data["session_id"].(string)
			slog.Debug("SSH tunnel: open_tunnel", "session", sessionID)
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.handleOpenTunnel(ws, tun, sessionID)
			}()

		case "data":
			pendingSessionID, _ = data["session_id"].(string)

		case "close_session":
			sessionID, _ := data["session_id"].(string)
			slog.Debug("SSH tunnel: close_session", "session", sessionID)
			tun.close(sessionID)

		case "disconnect":
			slog.Info("SSH tunnel: received disconnect command")
			m.mu.Lock()
			m.wanted = false
			m.mu.Unlock()
			return

		case "ping":
			pong, _ := json.Marshal(map[string]string{"type": "pong"})
			ws.WriteMessage(websocket.TextMessage, pong)

		default:
			slog.Debug("SSH tunnel: unknown message type", "type", msgTypeStr)
		}
	}
}

// handleOpenTunnel 打开到 localhost:sshPort 的 TCP 连接并进行桥接。
func (m *Manager) handleOpenTunnel(ws *websocket.Conn, tun *SSHTunnel, sessionID string) {
	conn, err := tun.open(sessionID, m.sshPort)
	if err != nil {
		slog.Warn("SSH tunnel: TCP connect to sshd failed",
			"session", sessionID, "port", m.sshPort, "error", err)
		sendTunnelClosed(ws, sessionID, fmt.Sprintf("connection_failed: %v", err))
		return
	}

	slog.Info("SSH tunnel: tunnel established", "session", sessionID)
	readyMsg, _ := json.Marshal(map[string]string{
		"type":       "tunnel_ready",
		"session_id": sessionID,
	})
	if err := ws.WriteMessage(websocket.TextMessage, readyMsg); err != nil {
		slog.Warn("SSH tunnel: failed to send tunnel_ready", "session", sessionID, "error", err)
		tun.close(sessionID)
		return
	}

	// 桥接 TCP → WS（WS → TCP 在消息循环中通过 "data" 消息处理）
	bridgeTCPToWS(ws, conn, sessionID)
	tun.close(sessionID)
}

// bridgeTCPToWS 从 TCP 读取数据，通过 WS 发送 JSON 头部帧 + 二进制数据帧。
func bridgeTCPToWS(ws *websocket.Conn, conn net.Conn, sessionID string) {
	buf := make([]byte, 32768) // 32KB
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			// 发送 JSON 头部帧
			header, _ := json.Marshal(map[string]string{
				"type":       "data",
				"session_id": sessionID,
			})
			if writeErr := ws.WriteMessage(websocket.TextMessage, header); writeErr != nil {
				slog.Debug("SSH tunnel: WS write header failed", "session", sessionID, "error", writeErr)
				return
			}
			// 发送二进制数据帧
			if writeErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
				slog.Debug("SSH tunnel: WS write data failed", "session", sessionID, "error", writeErr)
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.Debug("SSH tunnel: TCP 读取错误", "session", sessionID, "error", err)
			} else {
				slog.Debug("SSH tunnel: TCP EOF", "session", sessionID)
			}
			sendTunnelClosed(ws, sessionID, "tcp_eof")
			return
		}
	}
}

// sendTunnelClosed 发送 tunnel_closed JSON 帧（静默忽略错误）。
func sendTunnelClosed(ws *websocket.Conn, sessionID, reason string) {
	msg, _ := json.Marshal(map[string]string{
		"type":       "tunnel_closed",
		"session_id": sessionID,
		"reason":     reason,
	})
	_ = ws.WriteMessage(websocket.TextMessage, msg)
}
