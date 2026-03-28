package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/collei-monitor/collei-agent/internal/auth"
)

// Manager 管理直接终端 (ConPTY) 会话的 WebSocket 连接
type Manager struct {
	ctx      context.Context
	apiURL   string
	token    string
	shell    string
	verifier *auth.Verifier // nil = 跳过签名验证

	mu         sync.Mutex
	wanted     bool
	connected  bool
	loopCancel context.CancelFunc
	doneCh     chan struct{}
}

// NewManager 创建一个新的终端 WebSocket 管理器。
func NewManager(ctx context.Context, apiURL, token, shell string, verifier *auth.Verifier) *Manager {
	return &Manager{
		ctx:      ctx,
		apiURL:   strings.TrimRight(apiURL, "/"),
		token:    token,
		shell:    shell,
		verifier: verifier,
	}
}

// IsConnected 报告终端 WebSocket 是否当前正常。
func (m *Manager) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// Connect 请求终端 WebSocket 连接。
func (m *Manager) Connect() {
	m.mu.Lock()
	if m.wanted {
		m.mu.Unlock()
		return
	}
	m.wanted = true
	m.mu.Unlock()

	slog.Info("Terminal: connect requested, starting...")
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

	slog.Info("Terminal: disconnect requested, closing...")
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
	return wsBase + "/api/v1/agent/ws/terminal?token=" + m.token
}

// maintainConnection 采用指数退避策略进行重连。
func (m *Manager) maintainConnection(ctx context.Context) {
	delay := 1.0

	defer func() {
		m.mu.Lock()
		m.connected = false
		close(m.doneCh)
		m.mu.Unlock()
		slog.Info("Terminal: exited")
	}()

	for {
		m.mu.Lock()
		wanted := m.wanted
		m.mu.Unlock()
		if !wanted {
			return
		}

		url := m.wsURL()
		slog.Info("Terminal: connecting", "url", strings.Split(url, "?")[0])

		dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
		ws, _, err := dialer.Dial(url, http.Header{})
		if err != nil {
			slog.Warn("Terminal: connection failed", "error", err)
		} else {
			m.mu.Lock()
			m.connected = true
			m.mu.Unlock()
			delay = 1.0
			slog.Info("Terminal: WebSocket connected")

			// Send capabilities
			shell := m.shell
			if shell == "" {
				shell = defaultShell()
			}
			capMsg, _ := json.Marshal(map[string]interface{}{
				"type":          "capabilities",
				"terminal_mode": "conpty",
				"default_shell": shell,
			})
			if err := ws.WriteMessage(websocket.TextMessage, capMsg); err != nil {
				slog.Warn("Terminal: failed to send capabilities", "error", err)
			} else {
				slog.Debug("Terminal: capabilities sent", "shell", shell)
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
		slog.Info("Terminal: reconnecting", "delay_sec", fmt.Sprintf("%.1f", waitTime))

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

// terminalSessions 追踪活跃的 session_id 到终端的映射。
type terminalSessions struct {
	mu       sync.Mutex
	sessions map[string]Terminal
}

func newTerminalSessions() *terminalSessions {
	return &terminalSessions{sessions: make(map[string]Terminal)}
}

func (s *terminalSessions) add(id string, t Terminal) {
	s.mu.Lock()
	s.sessions[id] = t
	s.mu.Unlock()
}

func (s *terminalSessions) get(id string) Terminal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *terminalSessions) remove(id string) Terminal {
	s.mu.Lock()
	t, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	return t
}

func (s *terminalSessions) closeAll() {
	s.mu.Lock()
	for id, t := range s.sessions {
		t.Close()
		delete(s.sessions, id)
	}
	s.mu.Unlock()
}

// handleMessages 处理终端 WebSocket 消息循环。
func (m *Manager) handleMessages(ws *websocket.Conn, ctx context.Context) {
	sessions := newTerminalSessions()

	var wg sync.WaitGroup
	var pendingSessionID string

	// Close WS on context cancel to break ReadMessage.
	go func() {
		<-ctx.Done()
		ws.Close()
	}()

	defer func() {
		sessions.closeAll()
		wg.Wait()
		slog.Debug("Terminal: message loop ended, all sessions cleaned up")
	}()

	for {
		msgType, msg, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				slog.Info("Terminal: WebSocket closed normally")
				return
			}
			if ctx.Err() != nil {
				return
			}
			slog.Warn("Terminal: read error", "error", err)
			return
		}

		// Binary frame → write to pending session's stdin
		if msgType == websocket.BinaryMessage {
			if pendingSessionID != "" {
				t := sessions.get(pendingSessionID)
				if t != nil {
					if _, writeErr := t.Write(msg); writeErr != nil {
						slog.Warn("Terminal: stdin write failed",
							"session", pendingSessionID, "error", writeErr)
						sendTerminalClosed(ws, pendingSessionID, -1, "stdin_write_error")
						if removed := sessions.remove(pendingSessionID); removed != nil {
							removed.Close()
						}
					}
				}
				pendingSessionID = ""
			}
			continue
		}

		// Text frame → JSON control message
		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			slog.Warn("Terminal: invalid JSON received")
			continue
		}

		msgTypeStr, _ := data["type"].(string)

		switch msgTypeStr {
		case "open_terminal":
			sessionID, _ := data["session_id"].(string)

			// Signature verification for open_terminal
			if m.verifier != nil {
				timestamp, _ := data["timestamp"].(float64)
				nonce, _ := data["nonce"].(string)
				signature, _ := data["signature"].(string)

				err := m.verifier.Verify(&auth.SignedMessage{
					Type:      "open_terminal",
					SessionID: sessionID,
					Timestamp: int64(timestamp),
					Nonce:     nonce,
					Signature: signature,
				})
				if err != nil {
					slog.Warn("Terminal: open_terminal signature rejected",
						"session", sessionID, "error", err)
					sendError(ws, sessionID, "signature_rejected", err.Error())
					continue
				}
			}

			cols := intFromJSON(data["cols"], 120)
			rows := intFromJSON(data["rows"], 40)
			shell, _ := data["shell"].(string)
			if shell == "" {
				shell = m.shell
			}

			slog.Debug("Terminal: open_terminal", "session", sessionID, "cols", cols, "rows", rows, "shell", shell)

			wg.Add(1)
			go func() {
				defer wg.Done()
				m.handleOpenTerminal(ws, sessions, sessionID, shell, cols, rows)
			}()

		case "data":
			pendingSessionID, _ = data["session_id"].(string)

		case "resize":
			sessionID, _ := data["session_id"].(string)
			cols := intFromJSON(data["cols"], 0)
			rows := intFromJSON(data["rows"], 0)
			if t := sessions.get(sessionID); t != nil && cols > 0 && rows > 0 {
				if err := t.Resize(cols, rows); err != nil {
					slog.Warn("Terminal: resize failed", "session", sessionID, "error", err)
				}
			}

		case "close_session":
			sessionID, _ := data["session_id"].(string)
			slog.Debug("Terminal: close_session", "session", sessionID)
			if t := sessions.remove(sessionID); t != nil {
				t.Close()
			}

		case "disconnect":
			slog.Info("Terminal: received disconnect command")
			m.mu.Lock()
			m.wanted = false
			m.mu.Unlock()
			return

		case "ping":
			pong, _ := json.Marshal(map[string]string{"type": "pong"})
			ws.WriteMessage(websocket.TextMessage, pong)

		default:
			slog.Debug("Terminal: unknown message type", "type", msgTypeStr)
		}
	}
}

// handleOpenTerminal 创建一个 ConPTY 会话，并将其桥接到 WebSocket。
func (m *Manager) handleOpenTerminal(ws *websocket.Conn, sessions *terminalSessions, sessionID, shell string, cols, rows int) {
	t, err := Start(shell, cols, rows)
	if err != nil {
		slog.Warn("Terminal: failed to start terminal",
			"session", sessionID, "error", err)
		sendTerminalClosed(ws, sessionID, -1, fmt.Sprintf("start_failed: %v", err))
		return
	}

	sessions.add(sessionID, t)

	slog.Info("Terminal: session started", "session", sessionID)
	readyMsg, _ := json.Marshal(map[string]string{
		"type":       "terminal_ready",
		"session_id": sessionID,
	})
	if err := ws.WriteMessage(websocket.TextMessage, readyMsg); err != nil {
		slog.Warn("Terminal: failed to send terminal_ready", "session", sessionID, "error", err)
		sessions.remove(sessionID)
		t.Close()
		return
	}

	// Bridge terminal stdout → WS
	bridgeTerminalToWS(ws, t, sessionID)

	// Wait for process exit
	exitCode := -1
	if state, err := t.Wait(); err == nil && state != nil {
		exitCode = state.ExitCode()
	}

	sessions.remove(sessionID)
	t.Close()

	sendTerminalClosed(ws, sessionID, exitCode, "process_exited")
}

// bridgeTerminalToWS 从终端读取数据，并通过 WebSocket 发送数据帧。
func bridgeTerminalToWS(ws *websocket.Conn, t Terminal, sessionID string) {
	buf := make([]byte, 32768) // 32KB
	for {
		n, err := t.Read(buf)
		if n > 0 {
			header, _ := json.Marshal(map[string]string{
				"type":       "data",
				"session_id": sessionID,
			})
			if writeErr := ws.WriteMessage(websocket.TextMessage, header); writeErr != nil {
				slog.Debug("Terminal: WS write header failed", "session", sessionID, "error", writeErr)
				return
			}
			if writeErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
				slog.Debug("Terminal: WS write data failed", "session", sessionID, "error", writeErr)
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.Debug("Terminal: read error", "session", sessionID, "error", err)
			} else {
				slog.Debug("Terminal: EOF", "session", sessionID)
			}
			return
		}
	}
}

// sendTerminalClosed 发送一个 terminal_closed JSON 帧。
func sendTerminalClosed(ws *websocket.Conn, sessionID string, exitCode int, reason string) {
	msg, _ := json.Marshal(map[string]interface{}{
		"type":       "terminal_closed",
		"session_id": sessionID,
		"exit_code":  exitCode,
		"reason":     reason,
	})
	_ = ws.WriteMessage(websocket.TextMessage, msg)
}

// sendError 发送一个错误 JSON 帧。
func sendError(ws *websocket.Conn, sessionID, code, message string) {
	msg, _ := json.Marshal(map[string]string{
		"type":       "error",
		"session_id": sessionID,
		"code":       code,
		"message":    message,
	})
	_ = ws.WriteMessage(websocket.TextMessage, msg)
}

// intFromJSON 从 JSON 解码值（float64）中提取一个 int 类型值，并提供回退机制。
func intFromJSON(v interface{}, fallback int) int {
	if f, ok := v.(float64); ok && f > 0 {
		return int(f)
	}
	return fallback
}
