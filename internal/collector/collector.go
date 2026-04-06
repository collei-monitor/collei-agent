package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

// HardwareInfo 包含服务器静态硬件信息。
type HardwareInfo struct {
	CPUName        string `json:"cpu_name,omitempty"`
	Virtualization string `json:"virtualization,omitempty"`
	Arch           string `json:"arch,omitempty"`
	CPUCores       int    `json:"cpu_cores,omitempty"`
	OS             string `json:"os,omitempty"`
	KernelVersion  string `json:"kernel_version,omitempty"`
	IPv4           string `json:"ipv4,omitempty"`
	IPv6           string `json:"ipv6,omitempty"`
	MemTotal       int64  `json:"mem_total,omitempty"`
	SwapTotal      int64  `json:"swap_total,omitempty"`
	DiskTotal      int64  `json:"disk_total,omitempty"`
	BootTime       int64  `json:"boot_time,omitempty"`
}

// DiskPartition 包含单个磁盘分区的状态快照。
type DiskPartition struct {
	Mount string `json:"mount"`
	FS    string `json:"fs"`
	Total int64  `json:"total"`
	Used  int64  `json:"used"`
}

// NetInterface 包含单个网卡接口的累计收发流量。
type NetInterface struct {
	Name    string `json:"name"`
	RxBytes int64  `json:"rx_bytes"`
	TxBytes int64  `json:"tx_bytes"`
}

// LoadData 包含实时监控数据。
type LoadData struct {
	CPU       float64 `json:"cpu"`
	RAM       int64   `json:"ram"`
	RAMTotal  int64   `json:"ram_total"`
	Swap      int64   `json:"swap"`
	SwapTotal int64   `json:"swap_total"`
	Load      float64 `json:"load"`
	Disk      int64   `json:"disk"`
	DiskTotal int64   `json:"disk_total"`
	NetIn     int64   `json:"net_in"`
	NetOut    int64   `json:"net_out"`
	TCP       int     `json:"tcp"`
	UDP       int     `json:"udp"`
	Process   int     `json:"process"`
}

// SystemCollector 负责采集系统指标。
type SystemCollector struct {
	ctx              context.Context
	networkInterface string
	netStateFile     string

	nicWhitelist []*regexp.Regexp
	nicBlacklist []*regexp.Regexp

	prevNet     *[2]int64 // [rx, tx]
	currentNet  *[2]int64
	prevNetTime *float64

	lastHW     *HardwareInfo
	cachedHW   *HardwareInfo
	lastHWTime float64
	hwCacheTTL float64

	cpuPercent float64
	cpuMu      sync.RWMutex
}

// NewSystemCollector 创建一个新的采集器。
func NewSystemCollector(ctx context.Context, networkInterface, stateDir string, whitelist, blacklist []string) *SystemCollector {
	c := &SystemCollector{
		ctx:              ctx,
		networkInterface: networkInterface,
		hwCacheTTL:       300.0, // 5 minutes
	}
	c.nicWhitelist = compilePatterns(whitelist)
	c.nicBlacklist = compilePatterns(blacklist)
	if stateDir != "" {
		c.netStateFile = filepath.Join(stateDir, "net_state.json")
	}
	c.prevNet = c.loadNetState()

	// 启动后台 CPU 采样协程
	go c.cpuSamplingLoop()

	return c
}

// CollectHardware 采集完整的硬件信息（带缓存）。
func (c *SystemCollector) CollectHardware() *HardwareInfo {
	now := monotonicSeconds()

	if c.cachedHW != nil && (now-c.lastHWTime) < c.hwCacheTTL {
		return c.cachedHW
	}

	ctx := context.Background()

	hw := &HardwareInfo{
		CPUName:        getCPUName(ctx),
		Virtualization: getVirtualization(),
		Arch:           getArch(),
		CPUCores:       getCPUCores(ctx),
		OS:             getOSName(),
		KernelVersion:  getKernelVersion(),
		IPv4:           getIPv4(),
		IPv6:           getIPv6(),
		MemTotal:       getMemTotal(ctx),
		SwapTotal:      getSwapTotal(ctx),
		DiskTotal:      getDiskTotal(),
		BootTime:       getBootTime(ctx),
	}

	c.cachedHW = hw
	c.lastHWTime = now
	return hw
}

// CollectHardwareIfChanged 仅在硬件信息发生变化时返回新值。
func (c *SystemCollector) CollectHardwareIfChanged() *HardwareInfo {
	hw := c.CollectHardware()
	if c.lastHW != nil && *hw == *c.lastHW {
		return nil
	}
	c.lastHW = hw
	return hw
}

// CollectLoad 采集实时监控指标。
func (c *SystemCollector) CollectLoad() *LoadData {
	ctx := context.Background()

	memInfo, _ := mem.VirtualMemoryWithContext(ctx)
	swapInfo, _ := mem.SwapMemoryWithContext(ctx)
	diskUsed, diskTotal := getDiskUsage()
	netIn, netOut := c.calcNetSpeed()
	tcpCount, udpCount := getConnectionCounts()

	var ramUsed, ramTotal, swapUsed, swapTotal int64
	if memInfo != nil {
		ramUsed = int64(memInfo.Total - memInfo.Available)
		ramTotal = int64(memInfo.Total)
	}
	if swapInfo != nil {
		swapUsed = int64(swapInfo.Used)
		swapTotal = int64(swapInfo.Total)
	}

	c.cpuMu.RLock()
	cpuPct := c.cpuPercent
	c.cpuMu.RUnlock()

	return &LoadData{
		CPU:       cpuPct,
		RAM:       ramUsed,
		RAMTotal:  ramTotal,
		Swap:      swapUsed,
		SwapTotal: swapTotal,
		Load:      getLoadAvg(),
		Disk:      diskUsed,
		DiskTotal: diskTotal,
		NetIn:     netIn,
		NetOut:    netOut,
		TCP:       tcpCount,
		UDP:       udpCount,
		Process:   getProcessCount(),
	}
}

// CollectDiskIO 采集当前磁盘分区状态快照，过滤虚拟文件系统。
func (c *SystemCollector) CollectDiskIO() []DiskPartition {
	parts, err := disk.Partitions(false)
	if err != nil {
		slog.Warn("failed to get disk partitions", "error", err)
		return nil
	}

	virtualFS := map[string]bool{
		"tmpfs": true, "devtmpfs": true, "proc": true, "sysfs": true,
		"devpts": true, "cgroup": true, "cgroup2": true, "pstore": true,
		"securityfs": true, "debugfs": true, "configfs": true,
		"fusectl": true, "hugetlbfs": true, "mqueue": true,
		"efivarfs": true, "binfmt_misc": true, "tracefs": true,
		"overlay": true, "squashfs": true,
	}

	var result []DiskPartition
	seen := make(map[string]bool)
	for _, p := range parts {
		if virtualFS[p.Fstype] {
			continue
		}
		if seen[p.Mountpoint] {
			continue
		}
		seen[p.Mountpoint] = true

		usage, err := disk.Usage(p.Mountpoint)
		if err != nil || usage.Total == 0 {
			continue
		}
		result = append(result, DiskPartition{
			Mount: p.Mountpoint,
			FS:    p.Fstype,
			Total: int64(usage.Total),
			Used:  int64(usage.Used),
		})
	}
	return result
}

// CollectNetIO 采集当前网卡接口累计收发流量快照。
func (c *SystemCollector) CollectNetIO() []NetInterface {
	counters, err := gnet.IOCounters(true)
	if err != nil {
		slog.Warn("failed to get net IO counters", "error", err)
		return nil
	}

	var result []NetInterface
	for _, ctr := range counters {
		if ctr.BytesRecv == 0 && ctr.BytesSent == 0 {
			continue
		}
		if !c.nicAllowed(ctr.Name) {
			continue
		}
		result = append(result, NetInterface{
			Name:    ctr.Name,
			RxBytes: int64(ctr.BytesRecv),
			TxBytes: int64(ctr.BytesSent),
		})
	}
	return result
}

// CollectTotalFlow 返回自启动以来的累计网络字节数（rx, tx）。
func (c *SystemCollector) CollectTotalFlow() (int64, int64) {
	rx, tx := c.getNetCounters()
	return rx, tx
}

// ConfirmNetReported 在成功上报后更新基线值。
func (c *SystemCollector) ConfirmNetReported() {
	if c.currentNet != nil {
		c.prevNet = c.currentNet
		now := monotonicSeconds()
		c.prevNetTime = &now
		c.saveNetState(c.currentNet[0], c.currentNet[1])
	}
}

// --- CPU 采样 ---

func (c *SystemCollector) cpuSamplingLoop() {
	// 初始化
	cpu.Percent(0, false)
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		pct, err := cpu.Percent(time.Second, false)
		if err == nil && len(pct) > 0 {
			c.cpuMu.Lock()
			c.cpuPercent = pct[0]
			c.cpuMu.Unlock()
		}
	}
}

// --- 硬件采集辅助函数 ---

func getCPUName(ctx context.Context) string {
	infos, err := cpu.InfoWithContext(ctx)
	if err == nil && len(infos) > 0 && infos[0].ModelName != "" {
		return infos[0].ModelName
	}
	return runtime.GOARCH
}

func getVirtualization() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	out, err := exec.Command("systemd-detect-virt").Output()
	if err == nil {
		virt := strings.TrimSpace(string(out))
		if virt != "" && virt != "none" {
			return virt
		}
	}
	// 回退方案：/sys/hypervisor/type
	data, err := os.ReadFile("/sys/hypervisor/type")
	if err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

func getArch() string {
	machine := runtime.GOARCH
	switch machine {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "arm":
		return "armv7"
	case "386":
		return "i386"
	default:
		return machine
	}
}

func getCPUCores(ctx context.Context) int {
	n, err := cpu.CountsWithContext(ctx, true)
	if err != nil || n == 0 {
		return runtime.NumCPU()
	}
	return n
}

func getOSName() string {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/etc/os-release")
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					name := strings.TrimPrefix(line, "PRETTY_NAME=")
					return strings.Trim(name, "\"")
				}
			}
		}
	}
	info, err := host.Info()
	if err == nil {
		if info.Platform != "" {
			return fmt.Sprintf("%s %s", info.Platform, info.PlatformVersion)
		}
	}
	return fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH)
}

func getKernelVersion() string {
	info, err := host.Info()
	if err == nil && info.KernelVersion != "" {
		return info.KernelVersion
	}
	return ""
}

func getIPv4() string {
	urls := []string{
		"https://api4.ipify.org",
		"https://ipv4.icanhazip.com",
		"https://4.ident.me",
	}

	ch := make(chan string, len(urls))
	client := &http.Client{Timeout: 3 * time.Second}

	for _, u := range urls {
		go func(url string) {
			req, err := http.NewRequest("GET", url, nil)
			if err != nil {
				ch <- ""
				return
			}
			req.Header.Set("User-Agent", "ColleiAgent/1.0")
			resp, err := client.Do(req)
			if err != nil {
				ch <- ""
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				ch <- ""
				return
			}
			ip := strings.TrimSpace(string(body))
			if strings.Contains(ip, ".") && !strings.Contains(ip, ":") {
				ch <- ip
			} else {
				ch <- ""
			}
		}(u)
	}

	for range urls {
		ip := <-ch
		if ip != "" {
			return ip
		}
	}
	return ""
}

func getIPv6() string {
	conn, err := net.DialTimeout("udp6", "[2001:4860:4860::8888]:80", 2*time.Second)
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String()
}

func getMemTotal(ctx context.Context) int64 {
	v, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return 0
	}
	return int64(v.Total)
}

func getSwapTotal(ctx context.Context) int64 {
	v, err := mem.SwapMemoryWithContext(ctx)
	if err != nil {
		return 0
	}
	return int64(v.Total)
}

func getDiskTotal() int64 {
	root := "/"
	if runtime.GOOS == "windows" {
		root = "C:\\"
	}
	usage, err := disk.Usage(root)
	if err != nil {
		return 0
	}
	return int64(usage.Total)
}

func getDiskUsage() (int64, int64) {
	root := "/"
	if runtime.GOOS == "windows" {
		root = "C:\\"
	}
	usage, err := disk.Usage(root)
	if err != nil {
		return 0, 0
	}
	return int64(usage.Used), int64(usage.Total)
}

func getBootTime(ctx context.Context) int64 {
	bt, err := host.BootTimeWithContext(ctx)
	if err != nil {
		return 0
	}
	return int64(bt)
}

func getLoadAvg() float64 {
	avg, err := load.Avg()
	if err != nil || avg == nil {
		// 回退方案：Windows 使用 CPU 百分比
		pct, err := cpu.Percent(0, false)
		if err == nil && len(pct) > 0 {
			return pct[0] / 100.0
		}
		return 0
	}
	return avg.Load1
}

func getProcessCount() int {
	pids, err := process.Pids()
	if err != nil {
		return 0
	}
	return len(pids)
}

func getConnectionCounts() (int, int) {
	conns, err := gnet.Connections("inet")
	if err != nil {
		// 回退方案：解析 Linux /proc/net
		if runtime.GOOS == "linux" {
			tcp := countProcLines("/proc/net/tcp") + countProcLines("/proc/net/tcp6")
			udp := countProcLines("/proc/net/udp") + countProcLines("/proc/net/udp6")
			return tcp, udp
		}
		return 0, 0
	}

	var tcp, udp int
	for _, c := range conns {
		switch c.Type {
		case 1: // SOCK_STREAM = TCP
			tcp++
		case 2: // SOCK_DGRAM = UDP
			udp++
		}
	}
	return tcp, udp
}

func countProcLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) <= 1 {
		return 0
	}
	return len(lines) - 1 // 减去表头行
}

// --- 网络流量 ---

func (c *SystemCollector) calcNetSpeed() (int64, int64) {
	rx, tx := c.getNetCounters()
	now := monotonicSeconds()
	c.currentNet = &[2]int64{rx, tx}

	if c.prevNet == nil || c.prevNetTime == nil {
		c.prevNet = &[2]int64{rx, tx}
		c.prevNetTime = &now
		c.saveNetState(rx, tx)
		return 0, 0
	}

	elapsed := now - *c.prevNetTime
	if elapsed <= 0 {
		return 0, 0
	}

	netIn := rx - c.prevNet[0]
	netOut := tx - c.prevNet[1]

	// 系统重启：计数器重置，差值变为负数
	if netIn < 0 {
		netIn = rx
	}
	if netOut < 0 {
		netOut = tx
	}

	return int64(float64(netIn) / elapsed), int64(float64(netOut) / elapsed)
}

func (c *SystemCollector) getNetCounters() (int64, int64) {
	if c.networkInterface != "" {
		counters, err := gnet.IOCounters(true)
		if err == nil {
			for _, ctr := range counters {
				if ctr.Name == c.networkInterface {
					return int64(ctr.BytesRecv), int64(ctr.BytesSent)
				}
			}
			slog.Warn("specified network interface not found, falling back to total",
				"interface", c.networkInterface)
		}
	}

	counters, err := gnet.IOCounters(false)
	if err != nil || len(counters) == 0 {
		return 0, 0
	}
	return int64(counters[0].BytesRecv), int64(counters[0].BytesSent)
}

// --- 网络状态持久化 ---

type netState struct {
	RX int64 `json:"rx"`
	TX int64 `json:"tx"`
}

func (c *SystemCollector) loadNetState() *[2]int64 {
	if c.netStateFile == "" {
		return nil
	}
	data, err := os.ReadFile(c.netStateFile)
	if err != nil {
		return nil
	}
	var s netState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil
	}
	slog.Debug("restored network counters", "rx", s.RX, "tx", s.TX)
	return &[2]int64{s.RX, s.TX}
}

func (c *SystemCollector) saveNetState(rx, tx int64) {
	if c.netStateFile == "" {
		return
	}
	data, _ := json.Marshal(netState{RX: rx, TX: tx})
	_ = os.WriteFile(c.netStateFile, data, 0o644)
}

// --- 网卡过滤 ---

// defaultNICBlacklist 是留空配置时的默认黑名单正则，
// 过滤 Docker / Kubernetes / 常见虚拟网卡。
var defaultNICBlacklist = []*regexp.Regexp{
	regexp.MustCompile(`^docker\d*$`),
	regexp.MustCompile(`^veth`),
	regexp.MustCompile(`^br-`),
	regexp.MustCompile(`^cni\d*$`),
	regexp.MustCompile(`^flannel`),
	regexp.MustCompile(`^cali`),
	regexp.MustCompile(`^weave`),
	regexp.MustCompile(`^kube-`),
	regexp.MustCompile(`^vxlan`),
	regexp.MustCompile(`^tunl\d*$`),
	regexp.MustCompile(`^dummy`),
	regexp.MustCompile(`^virbr`),
	regexp.MustCompile(`^lxc`),
	regexp.MustCompile(`^lxd`),
	regexp.MustCompile(`^podman`),
}

// compilePatterns 编译正则表达式列表，跳过无效模式。
func compilePatterns(patterns []string) []*regexp.Regexp {
	var res []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			slog.Warn("invalid NIC filter pattern, skipping", "pattern", p, "error", err)
			continue
		}
		res = append(res, re)
	}
	return res
}

// nicAllowed 判断网卡名称是否应被采集。
// 优先级：白名单 > 黑名单 > 默认黑名单。
func (c *SystemCollector) nicAllowed(name string) bool {
	// 白名单模式：仅采集匹配白名单的网卡
	if len(c.nicWhitelist) > 0 {
		for _, re := range c.nicWhitelist {
			if re.MatchString(name) {
				return true
			}
		}
		return false
	}

	// 自定义黑名单：如果配置了自定义黑名单，仅使用自定义黑名单
	if len(c.nicBlacklist) > 0 {
		for _, re := range c.nicBlacklist {
			if re.MatchString(name) {
				return false
			}
		}
		return true
	}

	// 默认黑名单：过滤 Docker/K8s 等虚拟网卡
	for _, re := range defaultNICBlacklist {
		if re.MatchString(name) {
			return false
		}
	}
	return true
}

// UpdateNICFilter 运行时更新网卡过滤规则（用于配置热重载）。
func (c *SystemCollector) UpdateNICFilter(whitelist, blacklist []string) {
	c.nicWhitelist = compilePatterns(whitelist)
	c.nicBlacklist = compilePatterns(blacklist)
	slog.Info("NIC filter updated",
		"whitelist_count", len(c.nicWhitelist),
		"blacklist_count", len(c.nicBlacklist))
}

// --- 工具函数 ---

func monotonicSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}
