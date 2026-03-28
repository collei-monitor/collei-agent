package network

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ProbeCount     = 4
	ConnectTimeout = 5 * time.Second
	probePoolSize  = 8
)

// IsAlignable 检查探测间隔是否可以进行整点对齐。
func IsAlignable(interval int) bool {
	return interval > 0 && (60%interval == 0 || interval%60 == 0)
}

// NextAlignedTick 返回下一个对齐的触发时间戳。
func NextAlignedTick(interval int, now float64) float64 {
	if now == 0 {
		now = float64(time.Now().Unix())
	}
	if IsAlignable(interval) {
		fi := float64(interval)
		return math.Ceil(now/fi) * fi
	}
	return now + float64(interval)
}

// ProbeResult 存储单次探测结果。
type ProbeResult struct {
	TargetID      int      `json:"target_id"`
	Time          int64    `json:"time"`
	MedianLatency *float64 `json:"median_latency,omitempty"`
	MaxLatency    *float64 `json:"max_latency,omitempty"`
	MinLatency    *float64 `json:"min_latency,omitempty"`
	PacketLoss    float64  `json:"packet_loss"`
}

// Dispatch 表示后端下发的网络探测目标配置。
type Dispatch struct {
	Version string   `json:"version,omitempty"`
	Targets []Target `json:"targets,omitempty"`
}

// Target 表示一个网络探测目标。
type Target struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Protocol string `json:"protocol"`
	Port     *int   `json:"port"`
	Interval int    `json:"interval"`
}

// Monitor 管理网络探测。
type Monitor struct {
	ctx context.Context

	version string
	targets []Target

	mu             sync.Mutex
	pendingResults []ProbeResult

	groupCancels map[int]context.CancelFunc // interval -> cancel function
}

// NewMonitor 创建一个新的网络监控器。
func NewMonitor(ctx context.Context) *Monitor {
	return &Monitor{
		ctx:          ctx,
		groupCancels: make(map[int]context.CancelFunc),
	}
}

// Version 返回当前目标列表版本。
func (m *Monitor) Version() string {
	return m.version
}

// HandleDispatch 处理 verify/report 响应中的 network_dispatch。
func (m *Monitor) HandleDispatch(dispatch *Dispatch) {
	if dispatch == nil {
		return
	}

	if dispatch.Targets != nil {
		slog.Info("probe targets updated", "version", dispatch.Version, "count", len(dispatch.Targets))
		m.version = dispatch.Version
		m.targets = dispatch.Targets
		m.rescheduleProbes()
	} else if dispatch.Version != "" {
		m.version = dispatch.Version
		slog.Debug("probe targets unchanged", "version", dispatch.Version)
	}
}

// FlushPendingResults 返回并清空所有缓存的探测结果。
func (m *Monitor) FlushPendingResults() []ProbeResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.pendingResults) == 0 {
		return nil
	}

	results := make([]ProbeResult, len(m.pendingResults))
	copy(results, m.pendingResults)
	m.pendingResults = m.pendingResults[:0]
	return results
}

// RequeueResults 将失败的结果重新放回缓冲区。
func (m *Monitor) RequeueResults(results []ProbeResult) {
	if len(results) == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.pendingResults = append(results, m.pendingResults...)
	slog.Debug("requeued probe results", "count", len(results))
}

// Stop 停止所有探测协程。
func (m *Monitor) Stop() {
	m.stopAllGroups()
}

// --- 调度 ---

func (m *Monitor) rescheduleProbes() {
	m.stopAllGroups()

	// 按间隔分组目标
	groups := make(map[int][]Target)
	for _, t := range m.targets {
		interval := t.Interval
		if interval <= 0 {
			interval = 60
		}
		groups[interval] = append(groups[interval], t)
	}

	for interval, targets := range groups {
		groupCtx, cancel := context.WithCancel(m.ctx)
		m.groupCancels[interval] = cancel

		go m.intervalLoop(interval, targets, groupCtx)

		names := make([]string, 0, len(targets))
		for _, t := range targets {
			if t.Name != "" {
				names = append(names, t.Name)
			}
		}
		aligned := "aligned"
		if !IsAlignable(interval) {
			aligned = "unaligned"
		}
		slog.Info("probe group scheduled",
			"interval", interval, "alignment", aligned,
			"count", len(targets), "targets", strings.Join(names, ", "))
	}
}

func (m *Monitor) stopAllGroups() {
	for _, cancel := range m.groupCancels {
		cancel()
	}
	m.groupCancels = make(map[int]context.CancelFunc)
}

func (m *Monitor) intervalLoop(interval int, targets []Target, ctx context.Context) {
	// 等待第一个对齐时刻
	nextTick := NextAlignedTick(interval, 0)
	wait := time.Duration(float64(time.Second) * (nextTick - float64(time.Now().Unix())))
	if wait > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tickTime := time.Now().Unix()
		m.fireProbes(targets, tickTime)

		nextTick = NextAlignedTick(interval, 0)
		wait = time.Duration(float64(time.Second) * (nextTick - float64(time.Now().Unix())))
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

func (m *Monitor) fireProbes(targets []Target, tickTime int64) {
	type result struct {
		probe  ProbeResult
		target Target
		err    error
	}

	workers := probePoolSize
	if len(targets) < workers {
		workers = len(targets)
	}

	ch := make(chan result, len(targets))
	sem := make(chan struct{}, workers)

	for _, t := range targets {
		sem <- struct{}{}
		go func(target Target) {
			defer func() { <-sem }()
			r, err := probeTarget(target, tickTime)
			ch <- result{probe: r, target: target, err: err}
		}(t)
	}

	for range targets {
		r := <-ch
		if r.err != nil {
			slog.Warn("probe error", "target_id", r.target.ID, "error", r.err)
			continue
		}
		m.mu.Lock()
		m.pendingResults = append(m.pendingResults, r.probe)
		m.mu.Unlock()
		slog.Debug("probe complete",
			"target_id", r.target.ID,
			"median_ms", ptrFloat(r.probe.MedianLatency),
			"loss", r.probe.PacketLoss)
	}
}

// --- 探测实现 ---

func probeTarget(target Target, tickTime int64) (ProbeResult, error) {
	var latencies []float64
	var loss float64

	switch target.Protocol {
	case "icmp":
		latencies, loss = icmpPing(target.Host, ProbeCount)
	case "tcp":
		port := 80
		if target.Port != nil {
			port = *target.Port
		}
		latencies, loss = tcpPing(target.Host, port, ProbeCount)
	case "http":
		var port int
		if target.Port != nil {
			port = *target.Port
		}
		latencies, loss = httpPing(target.Host, port, ProbeCount)
	default:
		return ProbeResult{
			TargetID:   target.ID,
			Time:       tickTime,
			PacketLoss: 100.0,
		}, fmt.Errorf("unsupported protocol: %s", target.Protocol)
	}

	r := ProbeResult{
		TargetID:   target.ID,
		Time:       tickTime,
		PacketLoss: loss,
	}
	if len(latencies) > 0 {
		med := median(latencies)
		mx := max(latencies)
		mn := min(latencies)
		r.MedianLatency = &med
		r.MaxLatency = &mx
		r.MinLatency = &mn
	}
	return r, nil
}

// icmpPing 使用系统 ping 命令。
func icmpPing(host string, count int) ([]float64, float64) {
	isWin := runtime.GOOS == "windows"
	var cmd *exec.Cmd
	if isWin {
		cmd = exec.Command("ping", "-n", strconv.Itoa(count), "-w", "3000", host)
	} else {
		cmd = exec.Command("ping", "-c", strconv.Itoa(count), "-W", "3", host)
	}

	out, err := cmd.Output()
	if err != nil {
		// ping 在丢包时可能返回非零状态码，仍尝试解析输出
		if out == nil {
			return nil, 100.0
		}
	}

	output := string(out)

	// 解析延迟值
	var latencies []float64
	var re *regexp.Regexp
	if isWin {
		re = regexp.MustCompile(`[=<](\d+(?:\.\d+)?)ms`)
	} else {
		re = regexp.MustCompile(`time[=<](\d+(?:\.\d+)?)\s*ms`)
	}
	matches := re.FindAllStringSubmatch(output, -1)
	for _, m := range matches {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			latencies = append(latencies, v)
		}
	}

	// 解析丢包率
	loss := 100.0
	lossRe := regexp.MustCompile(`(\d+(?:\.\d+)?)%`)
	lossMatch := lossRe.FindStringSubmatch(output)
	if lossMatch != nil {
		if v, err := strconv.ParseFloat(lossMatch[1], 64); err == nil {
			loss = v
		}
	} else if len(latencies) > 0 {
		loss = math.Round((1-float64(len(latencies))/float64(count))*1000) / 10
	}

	return latencies, loss
}

// tcpPing 测量 TCP 握手延迟。
func tcpPing(host string, port, count int) ([]float64, float64) {
	addr := fmt.Sprintf("%s:%d", host, port)
	var latencies []float64
	failures := 0

	for i := 0; i < count; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, ConnectTimeout)
		elapsed := time.Since(start).Seconds() * 1000
		if err != nil {
			failures++
			continue
		}
		conn.Close()
		latencies = append(latencies, elapsed)
	}

	loss := 0.0
	if count > 0 {
		loss = math.Round(float64(failures)/float64(count)*1000) / 10
	}
	return latencies, loss
}

// httpPing 测量 HTTP HEAD 请求延迟。
func httpPing(host string, port, count int) ([]float64, float64) {
	var url string
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		url = host
	} else {
		scheme := "https"
		if port != 0 && port != 443 {
			scheme = "http"
		}
		portSuffix := ""
		if port != 0 && port != 80 && port != 443 {
			portSuffix = fmt.Sprintf(":%d", port)
		}
		url = fmt.Sprintf("%s://%s%s", scheme, host, portSuffix)
	}

	client := &http.Client{Timeout: ConnectTimeout}
	var latencies []float64
	failures := 0

	for i := 0; i < count; i++ {
		req, err := http.NewRequest("HEAD", url, nil)
		if err != nil {
			failures++
			continue
		}
		start := time.Now()
		resp, err := client.Do(req)
		elapsed := time.Since(start).Seconds() * 1000
		if err != nil {
			failures++
			continue
		}
		resp.Body.Close()
		latencies = append(latencies, elapsed)
	}

	loss := 0.0
	if count > 0 {
		loss = math.Round(float64(failures)/float64(count)*1000) / 10
	}
	return latencies, loss
}

// --- 数学辅助函数 ---

func median(vals []float64) float64 {
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 0 {
		return math.Round((sorted[n/2-1]+sorted[n/2])/2*100) / 100
	}
	return math.Round(sorted[n/2]*100) / 100
}

func max(vals []float64) float64 {
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return math.Round(m*100) / 100
}

func min(vals []float64) float64 {
	m := vals[0]
	for _, v := range vals[1:] {
		if v < m {
			m = v
		}
	}
	return math.Round(m*100) / 100
}

func ptrFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
