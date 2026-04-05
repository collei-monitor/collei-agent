package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/collei-monitor/collei-agent/internal/collector"
	"github.com/collei-monitor/collei-agent/internal/network"
)

// 默认 HTTP 超时时间。
const DefaultTimeout = 15 * time.Second

// ErrorKind 标识 API 错误子类型。
type ErrorKind int

const (
	ErrGeneric                   ErrorKind = iota
	ErrTokenInvalid                        // 401
	ErrServerNotApproved                   // 403
	ErrRegistrationNotConfigured           // 503
)

// APIError 表示后端返回的非成功 HTTP 响应。
type APIError struct {
	Kind       ErrorKind
	StatusCode int
	Detail     string
	Headers    http.Header
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Detail)
}

// 响应类型。

type RegisterResponse struct {
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

type VerifyResponse struct {
	UUID            string            `json:"uuid"`
	Token           string            `json:"token"`
	IsApproved      int               `json:"is_approved"`
	NetworkDispatch *network.Dispatch `json:"network_dispatch,omitempty"`
}

// SSHTunnelDirective 表示后端对 SSH 隧道的控制指令。
type SSHTunnelDirective struct {
	Connect *bool `json:"connect,omitempty"`
}

// TerminalDirective 表示后端对终端直连的控制指令（ConPTY 模式）。
type TerminalDirective struct {
	Connect *bool `json:"connect,omitempty"`
}

// FileAPIDirective 表示后端对文件 API 的控制指令。
type FileAPIDirective struct {
	Connect *bool `json:"connect,omitempty"`
}

// PendingTask 表示后端下发的待执行任务。
type PendingTask struct {
	ExecutionID string `json:"execution_id"`
	Type        string `json:"type"`
	TimeoutSec  int    `json:"timeout_sec"`
	Payload     string `json:"payload"`
}

// AgentFeatures 描述 Agent 当前启用的功能状态，随 report 上报。
type AgentFeatures struct {
	SSHEnabled      bool `json:"ssh_enabled"`
	TerminalEnabled bool `json:"terminal_enabled"`
	FileAPIEnabled  bool `json:"file_api_enabled"`
	TasksEnabled    bool `json:"tasks_enabled"`
}

type ReportResponse struct {
	UUID            string              `json:"uuid"`
	IsApproved      int                 `json:"is_approved"`
	Received        bool                `json:"received"`
	NetworkDispatch *network.Dispatch   `json:"network_dispatch,omitempty"`
	SSHTunnel       *SSHTunnelDirective `json:"ssh_tunnel,omitempty"`
	Terminal        *TerminalDirective  `json:"terminal,omitempty"`
	FileAPI         *FileAPIDirective   `json:"file_api,omitempty"`
	PendingTasks    []PendingTask       `json:"pending_tasks,omitempty"`
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
func (c *Client) Register(regToken, name string, hardware *collector.HardwareInfo, version string) (*RegisterResponse, error) {
	payload := &struct {
		RegToken string `json:"reg_token"`
		Name     string `json:"name,omitempty"`
		Version  string `json:"version,omitempty"`
		*collector.HardwareInfo
	}{
		RegToken:     regToken,
		Name:         name,
		Version:      version,
		HardwareInfo: hardware,
	}

	var resp RegisterResponse
	if err := c.post(c.agentBase+"/register", payload, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Verify 执行被动注册或身份验证。
func (c *Client) Verify(token, name string, hardware *collector.HardwareInfo, version string) (*VerifyResponse, error) {
	payload := &struct {
		Token   string `json:"token"`
		Name    string `json:"name,omitempty"`
		Version string `json:"version,omitempty"`
		*collector.HardwareInfo
	}{
		Token:        token,
		Name:         name,
		Version:      version,
		HardwareInfo: hardware,
	}

	var resp VerifyResponse
	if err := c.post(c.agentBase+"/verify", payload, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReportParams 聚合 Report 所需的全部参数。
type ReportParams struct {
	Token          string
	Hardware       *collector.HardwareInfo
	LoadData       *collector.LoadData
	TotalFlowIn    int64
	TotalFlowOut   int64
	DiskIO         []collector.DiskPartition
	NetIO          []collector.NetInterface
	NetworkVersion string
	NetworkData    []network.ProbeResult
	Features       *AgentFeatures
}

type reportPayload struct {
	Token string `json:"token"`
	*collector.HardwareInfo
	LoadData       *collector.LoadData       `json:"load_data,omitempty"`
	TotalFlowIn    int64                     `json:"total_flow_in"`
	TotalFlowOut   int64                     `json:"total_flow_out"`
	CurrentDiskIO  []collector.DiskPartition `json:"current_disk_io,omitempty"`
	CurrentNetIO   []collector.NetInterface  `json:"current_net_io,omitempty"`
	NetworkVersion string                    `json:"network_version,omitempty"`
	NetworkData    []network.ProbeResult     `json:"network_data,omitempty"`
	Features       *AgentFeatures            `json:"features,omitempty"`
}

// Report 向服务端发送监控数据。
func (c *Client) Report(params *ReportParams) (*ReportResponse, error) {
	payload := &reportPayload{
		Token:          params.Token,
		HardwareInfo:   params.Hardware,
		LoadData:       params.LoadData,
		TotalFlowIn:    params.TotalFlowIn,
		TotalFlowOut:   params.TotalFlowOut,
		CurrentDiskIO:  params.DiskIO,
		CurrentNetIO:   params.NetIO,
		NetworkVersion: params.NetworkVersion,
		NetworkData:    params.NetworkData,
		Features:       params.Features,
	}

	var resp ReportResponse
	if err := c.post(c.agentBase+"/report", payload, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReportTask 上报任务执行结果。
func (c *Client) ReportTask(executionID, status string, exitCode *int, output *string) error {
	payload := &struct {
		ExecutionID string  `json:"execution_id"`
		Status      string  `json:"status"`
		ExitCode    *int    `json:"exit_code,omitempty"`
		Output      *string `json:"output,omitempty"`
	}{
		ExecutionID: executionID,
		Status:      status,
		ExitCode:    exitCode,
		Output:      output,
	}
	return c.post(c.agentBase+"/tasks/report", payload, nil)
}

// post 发送 POST 请求，解析 JSON 响应并处理错误。
func (c *Client) post(url string, payload any, result any) error {
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

// handleResponse 解析 HTTP 响应并返回类型化错误。
func (c *Client) handleResponse(resp *http.Response, result any) error {
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
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		Detail:     detail,
		Headers:    resp.Header,
	}

	switch resp.StatusCode {
	case 401:
		apiErr.Kind = ErrTokenInvalid
	case 403:
		apiErr.Kind = ErrServerNotApproved
	case 503:
		apiErr.Kind = ErrRegistrationNotConfigured
	}

	return apiErr
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

// IsTokenInvalid 检查错误是否为 token 无效错误。
func IsTokenInvalid(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Kind == ErrTokenInvalid
}

// IsServerNotApproved 检查错误是否为服务器未审批错误。
func IsServerNotApproved(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Kind == ErrServerNotApproved
}

// GetAPIError 从错误中提取 APIError。
func GetAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}
