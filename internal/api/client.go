package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// 默认 HTTP 超时时间。
const DefaultTimeout = 15 * time.Second

// Error types mirroring the Python hierarchy — 映射 Python 异常层次的错误类型。

type ApiError struct {
	StatusCode int
	Detail     string
	Headers    http.Header
}

func (e *ApiError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Detail)
}

type TokenInvalid struct{ ApiError }
type ServerNotApproved struct{ ApiError }
type RegistrationNotConfigured struct{ ApiError }

// 响应类型。

type RegisterResponse struct {
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

type VerifyResponse struct {
	UUID            string                 `json:"uuid"`
	Token           string                 `json:"token"`
	IsApproved      int                    `json:"is_approved"`
	NetworkDispatch map[string]interface{} `json:"network_dispatch,omitempty"`
}

type ReportResponse struct {
	UUID            string                   `json:"uuid"`
	IsApproved      int                      `json:"is_approved"`
	Received        bool                     `json:"received"`
	NetworkDispatch map[string]interface{}   `json:"network_dispatch,omitempty"`
	SSHTunnel       map[string]interface{}   `json:"ssh_tunnel,omitempty"`
	PendingTasks    []map[string]interface{} `json:"pending_tasks,omitempty"`
}

// Client 是 Collei API HTTP 客户端。
type Client struct {
	baseURL   string
	agentBase string
	client    *http.Client
}

// NewClient 创建一个新的 API 客户端。
func NewClient(baseURL string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	base := strings.TrimRight(baseURL, "/")
	return &Client{
		baseURL:   base,
		agentBase: base + "/api/v1/agent",
		client:    &http.Client{Timeout: timeout},
	}
}

// Close 释放 HTTP 客户端资源。
func (c *Client) Close() {
	c.client.CloseIdleConnections()
}

// Register 使用全局注册令牌执行自动注册。
func (c *Client) Register(regToken, name string, hardware map[string]interface{}, version string) (*RegisterResponse, error) {
	payload := map[string]interface{}{
		"reg_token": regToken,
		"name":      name,
	}
	for k, v := range hardware {
		payload[k] = v
	}
	if version != "" {
		payload["version"] = version
	}

	var resp RegisterResponse
	if err := c.post(c.agentBase+"/register", payload, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Verify 执行被动注册或身份验证。
func (c *Client) Verify(token, name string, hardware map[string]interface{}, version string) (*VerifyResponse, error) {
	payload := map[string]interface{}{
		"token": token,
	}
	if name != "" {
		payload["name"] = name
	}
	for k, v := range hardware {
		payload[k] = v
	}
	if version != "" {
		payload["version"] = version
	}

	var resp VerifyResponse
	if err := c.post(c.agentBase+"/verify", payload, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Report 向服务端发送监控数据。
func (c *Client) Report(
	token string,
	hardware map[string]interface{},
	loadData map[string]interface{},
	totalFlowIn, totalFlowOut *int64,
	networkVersion *string,
	networkData []map[string]interface{},
) (*ReportResponse, error) {
	payload := map[string]interface{}{
		"token": token,
	}
	for k, v := range hardware {
		payload[k] = v
	}
	if loadData != nil {
		payload["load_data"] = loadData
	}
	if totalFlowIn != nil {
		payload["total_flow_in"] = *totalFlowIn
	}
	if totalFlowOut != nil {
		payload["total_flow_out"] = *totalFlowOut
	}
	if networkVersion != nil {
		payload["network_version"] = *networkVersion
	}
	if len(networkData) > 0 {
		payload["network_data"] = networkData
	}

	var resp ReportResponse
	if err := c.post(c.agentBase+"/report", payload, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReportTask 上报任务执行结果。
func (c *Client) ReportTask(executionID, status string, exitCode *int, output *string) error {
	payload := map[string]interface{}{
		"execution_id": executionID,
		"status":       status,
	}
	if exitCode != nil {
		payload["exit_code"] = *exitCode
	}
	if output != nil {
		payload["output"] = *output
	}
	return c.post(c.agentBase+"/tasks/report", payload, nil)
}

// post 发送 POST 请求，解析 JSON 响应并处理错误。
func (c *Client) post(url string, payload map[string]interface{}, result interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	slog.Debug("POST request", "url", url)

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	return c.handleResponse(resp, result)
}

// handleResponse 解析 HTTP 响应并抛出类型化错误。
func (c *Client) handleResponse(resp *http.Response, result interface{}) error {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == 200 || resp.StatusCode == 201 {
		if result != nil {
			if err := json.Unmarshal(respBody, result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}
		}
		return nil
	}

	detail := extractDetail(respBody, resp)
	base := ApiError{
		StatusCode: resp.StatusCode,
		Detail:     detail,
		Headers:    resp.Header,
	}

	switch resp.StatusCode {
	case 401:
		return &TokenInvalid{base}
	case 403:
		return &ServerNotApproved{base}
	case 503:
		return &RegistrationNotConfigured{base}
	default:
		return &base
	}
}

// extractDetail 从响应中提取可读的错误描述。
// 优先级：JSON "detail" → "message"/"error"/"msg" → 纯文本 → HTTP 状态码。
func extractDetail(body []byte, resp *http.Response) string {
	// 尝试解析 JSON
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err == nil {
		if detail, ok := data["detail"]; ok {
			return fmt.Sprintf("%v", detail)
		}
		for _, key := range []string{"message", "error", "msg"} {
			if v, ok := data[key]; ok {
				return fmt.Sprintf("%v", v)
			}
		}
		s, _ := json.Marshal(data)
		if len(s) > 300 {
			s = s[:300]
		}
		return string(s)
	}

	// 纯文本（跳过 HTML）
	text := strings.TrimSpace(string(body))
	if text != "" && !strings.HasPrefix(text, "<") {
		if len(text) > 200 {
			text = text[:200]
		}
		return text
	}

	// HTTP 状态文本
	if resp.Status != "" {
		return resp.Status
	}

	return fmt.Sprintf("%d", resp.StatusCode)
}

// IsTokenInvalid 检查错误是否为 TokenInvalid 类型。
func IsTokenInvalid(err error) bool {
	_, ok := err.(*TokenInvalid)
	return ok
}

// IsServerNotApproved 检查错误是否为 ServerNotApproved 类型。
func IsServerNotApproved(err error) bool {
	_, ok := err.(*ServerNotApproved)
	return ok
}

// GetApiError 从任意 API 错误类型中提取 ApiError。
func GetApiError(err error) (*ApiError, bool) {
	switch e := err.(type) {
	case *ApiError:
		return e, true
	case *TokenInvalid:
		return &e.ApiError, true
	case *ServerNotApproved:
		return &e.ApiError, true
	case *RegistrationNotConfigured:
		return &e.ApiError, true
	default:
		return nil, false
	}
}
