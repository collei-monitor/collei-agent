"""
系统数据采集模块

负责采集服务器硬件信息和实时监控指标。
使用 psutil 实现跨平台兼容，同时在 Linux 下优先读取 /proc 获取更精准的数据。
"""

from __future__ import annotations

import json
import logging
import os
import platform
import socket
import subprocess
import time
from dataclasses import dataclass, asdict, field
from typing import Optional, List
import urllib.request


import queue
import threading

import psutil
import cpuinfo

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# 数据模型
# ---------------------------------------------------------------------------

@dataclass
class HardwareInfo:
    """服务器硬件（静态）信息"""
    cpu_name: str = ""
    virtualization: str = ""
    arch: str = ""
    cpu_cores: int = 0
    os: str = ""
    kernel_version: str = ""
    ipv4: str = ""
    ipv6: str = ""
    mem_total: int = 0
    swap_total: int = 0
    disk_total: int = 0
    boot_time: int = 0

    def to_dict(self) -> dict:
        """转为 API 请求字段，过滤空值"""
        return {k: v for k, v in asdict(self).items() if v}


@dataclass
class LoadData:
    """实时监控数据"""
    cpu: float = 0.0
    ram: int = 0
    ram_total: int = 0
    swap: int = 0
    swap_total: int = 0
    load: float = 0.0
    disk: int = 0
    disk_total: int = 0
    net_in: int = 0
    net_out: int = 0
    tcp: int = 0
    udp: int = 0
    process: int = 0

    def to_dict(self) -> dict:
        return asdict(self)


# ---------------------------------------------------------------------------
# 采集器
# ---------------------------------------------------------------------------

class SystemCollector:
    """系统数据采集器"""

    def __init__(self, network_interface: str = "", state_dir: str = "") -> None:
        self._network_interface = network_interface
        self._net_state_file = os.path.join(state_dir, "net_state.json") if state_dir else ""
        self._prev_net: Optional[tuple[int, int]] = self._load_net_state()
        self._current_net: Optional[tuple[int, int]] = None  # 最近一次采集时的系统读数
        self._prev_net_time: Optional[float] = None  # 上次网络基准时间戳
        self._last_hw: Optional[dict] = None

        # 新增缓存机制
        self._cached_hw: Optional[HardwareInfo] = None
        self._last_hw_collect_time: float = 0.0
        self._hw_cache_ttl: float = 300.0  # 缓存 5 分钟 (300秒)

        # 后台持续采样 CPU（避免断网退避后首次读值失真）
        self._cpu_percent: float = 0.0
        _t = threading.Thread(target=self._cpu_sampling_loop, daemon=True)
        _t.start()

    # ---- 硬件信息 ----

    def collect_hardware(self) -> HardwareInfo:
        """采集完整的硬件信息（带缓存）"""
        now = time.monotonic()

        # 如果缓存有效，直接返回
        if self._cached_hw and (now - self._last_hw_collect_time) < self._hw_cache_ttl:
            return self._cached_hw

        # 否则重新采集
        hw = HardwareInfo(
            cpu_name=self._get_cpu_name(),
            virtualization=self._get_virtualization(),
            arch=self._get_arch(),
            cpu_cores=psutil.cpu_count(logical=True) or 0,
            os=self._get_os_name(),
            kernel_version=platform.release(),
            ipv4=self._get_ipv4(),
            ipv6=self._get_ipv6(),
            mem_total=psutil.virtual_memory().total,
            swap_total=psutil.swap_memory().total,
            disk_total=self._get_disk_total(),
            boot_time=int(psutil.boot_time()),
        )

        self._cached_hw = hw
        self._last_hw_collect_time = now
        return hw

    def collect_hardware_if_changed(self) -> Optional[dict]:
        """
        采集硬件信息，仅在检测到变更时返回 dict，否则返回 None。
        首次调用必定返回数据。
        """
        hw = self.collect_hardware().to_dict()
        if hw != self._last_hw:
            self._last_hw = hw
            return hw
        return None

    # ---- 监控数据 ----

    def collect_load(self) -> LoadData:
        """采集实时监控指标"""
        mem = psutil.virtual_memory()
        swap = psutil.swap_memory()
        disk = self._get_disk_usage()

        net_in, net_out = self._calc_net_speed()

        tcp_count, udp_count = self._get_connection_counts()

        return LoadData(
            cpu=self._cpu_percent,
            ram=mem.total - mem.available,
            ram_total=mem.total,
            swap=swap.used,
            swap_total=swap.total,
            load=self._get_load_avg(),
            disk=disk[0],
            disk_total=disk[1],
            net_in=net_in,
            net_out=net_out,
            tcp=tcp_count,
            udp=udp_count,
            process=len(psutil.pids()),
        )

    # ---- 内部实现 ----

    def _cpu_sampling_loop(self) -> None:
        """后台持续采样 CPU 使用率（约 1 秒/次），确保任何时刻读值都是新鲜的"""
        psutil.cpu_percent(interval=None)  # 初始化内部计时器
        while True:
            self._cpu_percent = psutil.cpu_percent(interval=1.0)

    @staticmethod
    def _get_cpu_name() -> str:
        """获取 CPU 型号名称"""
        try:
            return cpuinfo.get_cpu_info().get("brand_raw") or platform.processor() or "Unknown"
        except Exception:
            return platform.processor() or "Unknown"

    @staticmethod
    def _get_virtualization() -> str:
        """检测虚拟化类型"""
        if not _is_linux():
            return ""
        try:
            result = subprocess.run(
                ["systemd-detect-virt"],
                capture_output=True, text=True, timeout=5,
            )
            virt = result.stdout.strip()
            if virt and virt != "none":
                return virt
        except (FileNotFoundError, subprocess.TimeoutExpired):
            pass
        # fallback: 检查 /sys
        hypervisor = "/sys/hypervisor/type"
        if os.path.exists(hypervisor):
            try:
                with open(hypervisor) as f:
                    return f.read().strip()
            except OSError:
                pass
        return ""

    @staticmethod
    def _get_arch() -> str:
        """
        获取系统架构，按文档规范映射：
        x86_64 → amd64, aarch64 → arm64
        """
        machine = platform.machine().lower()
        mapping = {
            "x86_64": "amd64",
            "amd64": "amd64",
            "aarch64": "arm64",
            "arm64": "arm64",
            "armv7l": "armv7",
            "i686": "i386",
            "i386": "i386",
        }
        return mapping.get(machine, machine)

    @staticmethod
    def _get_os_name() -> str:
        """获取操作系统名称"""
        # Linux: /etc/os-release
        if _is_linux() and os.path.exists("/etc/os-release"):
            try:
                with open("/etc/os-release") as f:
                    for line in f:
                        if line.startswith("PRETTY_NAME="):
                            return line.split("=", 1)[1].strip().strip('"')
            except OSError:
                pass
        return f"{platform.system()} {platform.release()}"

    @staticmethod
    def _fetch_ip_from_url(url: str, expected_v4: bool) -> str:
        """底层请求方法：向单个 URL 发起请求并校验"""
        try:
            # 添加 User-Agent 防止被某些防火墙拦截
            req = urllib.request.Request(
                url, headers={'User-Agent': 'Mozilla/5.0 (SystemCollector)'})
            with urllib.request.urlopen(req, timeout=2) as response:
                ip = response.read().decode('utf-8').strip()
                # 严格校验返回格式
                if expected_v4 and "." in ip and ":" not in ip:
                    return ip
                elif not expected_v4 and ":" in ip:
                    return ip
        except Exception:
            pass
        return ""

    @staticmethod
    def _get_fastest_ip(urls: List[str], expected_v4: bool) -> str:
        """并发请求多个接口，返回最快拿到且合法的 IP"""
        result_q: queue.SimpleQueue[str] = queue.SimpleQueue()

        def _worker(url: str) -> None:
            result_q.put(SystemCollector._fetch_ip_from_url(url, expected_v4))

        for url in urls:
            threading.Thread(target=_worker, args=(url,), daemon=True).start()

        # 收集至多 len(urls) 个结果，拿到第一个有效 IP 立即返回，剩余线程后台自生自灭
        for _ in urls:
            ip = result_q.get()
            if ip:
                return ip
        return ""

    @staticmethod
    def _get_ipv4() -> str:
        """获取默认出口 IPv4 地址（并发竞速）"""
        urls = [
            "https://api4.ipify.org",
            "https://ipv4.icanhazip.com",
            "https://4.ident.me"
        ]
        return SystemCollector._get_fastest_ip(urls, expected_v4=True)

    @staticmethod
    def _get_ipv6() -> str:
        """获取默认出口 IPv6 地址"""
        try:
            s = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM)
            s.settimeout(2)
            s.connect(("2001:4860:4860::8888", 80))
            ip = s.getsockname()[0]
            s.close()
            return ip
        except OSError:
            return ""

    @staticmethod
    def _get_disk_total() -> int:
        """获取主磁盘总容量"""
        try:
            root = "/" if _is_linux() else "C:\\"
            usage = psutil.disk_usage(root)
            return usage.total
        except OSError:
            return 0

    @staticmethod
    def _get_disk_usage() -> tuple[int, int]:
        """返回 (已用, 总量) Bytes"""
        try:
            root = "/" if _is_linux() else "C:\\"
            usage = psutil.disk_usage(root)
            return usage.used, usage.total
        except OSError:
            return 0, 0

    @staticmethod
    def _get_load_avg() -> float:
        """获取 1 分钟负载均值"""
        try:
            return os.getloadavg()[0] # type: ignore
        except (OSError, AttributeError):
            # Windows 上不支持 getloadavg，使用 CPU percent 近似
            return psutil.cpu_percent(interval=None) / 100.0

    def _calc_net_speed(self) -> tuple[int, int]:
        """
        计算网络入站/出站速率 (Bytes/s)。
        基于系统网卡总读数减去上一次成功上报时的读数，再除以经过的时间。
        上报失败时不更新基准，确保失败期间的流量在下次成功上报时被补齐。
        Agent 重启后从本地文件恢复上次读数；系统重启后若差值为负则以当前值作为增量。
        """
        rx, tx = self._get_net_counters()
        now = time.monotonic()
        self._current_net = (rx, tx)

        if self._prev_net is None or self._prev_net_time is None:
            self._prev_net = (rx, tx)
            self._prev_net_time = now
            self._save_net_state(rx, tx)
            return 0, 0

        elapsed = now - self._prev_net_time
        if elapsed <= 0:
            return 0, 0

        prev_rx, prev_tx = self._prev_net

        net_in = rx - prev_rx
        net_out = tx - prev_tx

        # 系统重启导致计数器归零，差值为负时以当前值作为增量
        if net_in < 0:
            net_in = rx
        if net_out < 0:
            net_out = tx

        return int(net_in / elapsed), int(net_out / elapsed)

    def confirm_net_reported(self) -> None:
        """
        上报成功后调用：将最近一次采集的系统读数确认为基准，
        后续增量从此基准开始计算。同时持久化到本地文件。
        """
        if self._current_net is not None:
            self._prev_net = self._current_net
            self._prev_net_time = time.monotonic()
            self._save_net_state(*self._current_net)

    def _get_net_counters(self) -> tuple[int, int]:
        """获取网络收发字节数，支持指定网卡"""
        if self._network_interface:
            try:
                per_nic = psutil.net_io_counters(pernic=True)
                if self._network_interface in per_nic:
                    c = per_nic[self._network_interface]
                    return c.bytes_recv, c.bytes_sent
                else:
                    logger.warning("指定网卡 '%s' 不存在，可用网卡: %s，已回退到全部汇总",
                                   self._network_interface, list(per_nic.keys()))
            except Exception as exc:
                logger.warning("获取网卡 '%s' 数据失败: %s", self._network_interface, exc)
        counters = psutil.net_io_counters()
        return counters.bytes_recv, counters.bytes_sent

    def collect_total_flow(self) -> tuple[int, int]:
        """
        获取系统开机以来的累积流量 (total_flow_in, total_flow_out)。
        直接读取网卡的 rx_bytes / tx_bytes 累计值。
        """
        rx, tx = self._get_net_counters()
        return rx, tx

    def _load_net_state(self) -> Optional[tuple[int, int]]:
        """从本地文件恢复上次的网络计数器读数"""
        if not self._net_state_file:
            return None
        try:
            with open(self._net_state_file, "r", encoding="utf-8") as f:
                data = json.load(f)
            rx = int(data["rx"])
            tx = int(data["tx"])
            logger.debug("已从 %s 恢复网络计数器: rx=%d tx=%d", self._net_state_file, rx, tx)
            return (rx, tx)
        except (FileNotFoundError, KeyError, ValueError, json.JSONDecodeError):
            return None
        except OSError as exc:
            logger.debug("读取网络状态文件失败: %s", exc)
            return None

    def _save_net_state(self, rx: int, tx: int) -> None:
        """将当前网络计数器读数持久化到本地文件"""
        if not self._net_state_file:
            return
        try:
            with open(self._net_state_file, "w", encoding="utf-8") as f:
                json.dump({"rx": rx, "tx": tx}, f)
        except OSError as exc:
            logger.debug("保存网络状态文件失败: %s", exc)

    @staticmethod
    def _get_connection_counts() -> tuple[int, int]:
        """获取 TCP / UDP 连接数"""
        tcp = udp = 0
        try:
            conns = psutil.net_connections(kind="inet")
            for c in conns:
                if c.type == socket.SOCK_STREAM:
                    tcp += 1
                elif c.type == socket.SOCK_DGRAM:
                    udp += 1
        except (psutil.AccessDenied, OSError):
            # Linux 下某些情况需要 root 权限
            if _is_linux():
                tcp = _count_proc_lines(
                    "/proc/net/tcp") + _count_proc_lines("/proc/net/tcp6")
                udp = _count_proc_lines(
                    "/proc/net/udp") + _count_proc_lines("/proc/net/udp6")
        return tcp, udp


# ---------------------------------------------------------------------------
# 工具函数
# ---------------------------------------------------------------------------

def _is_linux() -> bool:
    return platform.system() == "Linux"


def _count_proc_lines(path: str) -> int:
    """计算 /proc/net/* 文件的有效行数（排除表头）"""
    try:
        with open(path) as f:
            lines = f.readlines()
            return max(0, len(lines) - 1)
    except OSError:
        return 0
