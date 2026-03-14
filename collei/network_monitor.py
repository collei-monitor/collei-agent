"""
网络监控模块

负责管理探测目标、执行 ICMP/TCP/HTTP 探测，
并缓存探测结果供上报时使用。

设计要点：
  - 每个目标独立线程定时探测，互不干扰
  - 探测结果存入线程安全的缓冲区，上报时一次性取出
  - handle_dispatch() 统一处理 verify 和 report 返回的下发信息
"""

from __future__ import annotations

import logging
import platform
import socket
import statistics
import subprocess
import re
import threading
import time
from dataclasses import dataclass, asdict
from typing import Optional

logger = logging.getLogger(__name__)

# 每次探测发送的次数
PROBE_COUNT = 4
# TCP/HTTP 单次连接超时（秒）
CONNECT_TIMEOUT = 5.0


@dataclass
class ProbeResult:
    """单次探测结果"""
    target_id: int
    time: int
    median_latency: Optional[float] = None
    max_latency: Optional[float] = None
    min_latency: Optional[float] = None
    packet_loss: float = 0.0

    def to_dict(self) -> dict:
        d = asdict(self)
        return {k: v for k, v in d.items() if v is not None}


class NetworkMonitor:
    """
    网络监控管理器

    管理探测目标列表，按各自间隔独立调度探测，
    结果暂存到内部缓冲区供 report 时取出。
    """

    def __init__(self, stop_event: threading.Event) -> None:
        self._stop_event = stop_event

        self.version: Optional[str] = None
        self._targets: list[dict] = []

        # 探测线程管理：target_id → Event（用于停止对应线程）
        self._probe_stops: dict[int, threading.Event] = {}
        self._probe_threads: dict[int, threading.Thread] = {}

        # 探测结果缓冲（线程安全）
        self._results_lock = threading.Lock()
        self._pending_results: list[ProbeResult] = []

    @property
    def targets(self) -> list[dict]:
        return self._targets

    # ---- 下发处理 ----

    def handle_dispatch(self, dispatch: dict | None) -> None:
        """
        统一处理 verify / report 返回的 network_dispatch。

        - dispatch 为 None → 跳过（节点未批准等情况）
        - targets 不为 None → 目标列表有更新，重新调度
        - targets 为 None → 版本号未变，无需操作
        """
        if not dispatch:
            return

        new_version = dispatch.get("version")
        new_targets = dispatch.get("targets")

        if new_targets is not None:
            logger.info(
                "收到探测目标更新: version=%s, %d 个目标",
                new_version, len(new_targets),
            )
            self.version = new_version
            self._targets = new_targets
            self._reschedule_probes()
        elif new_version:
            self.version = new_version
            logger.debug("探测目标无变更 (version=%s)", new_version)

    # ---- 结果管理 ----

    def flush_pending_results(self) -> list[dict]:
        """取出所有待上报的探测结果并清空缓冲区。"""
        with self._results_lock:
            if not self._pending_results:
                return []
            results = [r.to_dict() for r in self._pending_results]
            self._pending_results.clear()
            return results

    # ---- 调度 ----

    def _reschedule_probes(self) -> None:
        """停掉所有旧探测线程，根据新目标列表重建。"""
        self._stop_all_probes()

        for target in self._targets:
            tid = target["id"]
            stop_evt = threading.Event()
            self._probe_stops[tid] = stop_evt
            t = threading.Thread(
                target=self._probe_loop,
                args=(target, stop_evt),
                daemon=True,
                name=f"probe-{tid}",
            )
            self._probe_threads[tid] = t
            t.start()
            logger.debug(
                "启动探测: id=%d name=%s host=%s protocol=%s interval=%ds",
                tid, target.get("name"), target["host"],
                target["protocol"], target.get("interval", 60),
            )

    def _stop_all_probes(self) -> None:
        """通知所有探测线程停止。"""
        for evt in self._probe_stops.values():
            evt.set()
        self._probe_stops.clear()
        self._probe_threads.clear()

    def stop(self) -> None:
        """停止所有探测（Agent 关闭时调用）。"""
        self._stop_all_probes()

    def _probe_loop(self, target: dict, stop_event: threading.Event) -> None:
        """单个目标的定时探测循环。"""
        interval = target.get("interval", 60)
        tid = target["id"]

        while not stop_event.is_set() and not self._stop_event.is_set():
            try:
                result = self._probe_target(target)
                with self._results_lock:
                    self._pending_results.append(result)
                logger.debug(
                    "探测完成: id=%d median=%.1fms loss=%.1f%%",
                    tid,
                    result.median_latency or 0,
                    result.packet_loss,
                )
            except Exception as exc:
                logger.warning("探测异常 (id=%d): %s", tid, exc)

            # 可中断的等待
            stop_event.wait(interval)

    # ---- 探测实现 ----

    def _probe_target(self, target: dict) -> ProbeResult:
        """根据协议执行探测并返回结果。"""
        protocol = target["protocol"]
        host = target["host"]
        port = target.get("port")

        if protocol == "icmp":
            latencies, loss = self._icmp_ping(host)
        elif protocol == "tcp":
            latencies, loss = self._tcp_ping(host, port or 80)
        elif protocol == "http":
            latencies, loss = self._http_ping(host, port)
        else:
            logger.warning("不支持的协议: %s", protocol)
            return ProbeResult(target_id=target["id"], time=int(time.time()), packet_loss=100.0)

        result = ProbeResult(
            target_id=target["id"],
            time=int(time.time()),
            packet_loss=loss,
        )
        if latencies:
            result.median_latency = round(statistics.median(latencies), 2)
            result.max_latency = round(max(latencies), 2)
            result.min_latency = round(min(latencies), 2)

        return result

    @staticmethod
    def _icmp_ping(host: str, count: int = PROBE_COUNT) -> tuple[list[float], float]:
        """
        通过系统 ping 命令执行 ICMP 探测。
        返回 (延迟列表_ms, 丢包率_%)。
        """
        is_win = platform.system() == "Windows"
        cmd = ["ping", "-n" if is_win else "-c", str(count), "-w" if is_win else "-W",
               "3" if is_win else "3", host]

        try:
            proc = subprocess.run(
                cmd, capture_output=True, text=True, timeout=count * 5,
            )
            output = proc.stdout
        except (subprocess.TimeoutExpired, FileNotFoundError):
            return [], 100.0

        # 解析延迟
        latencies: list[float] = []
        if is_win:
            # Windows: "时间=12ms" or "time=12ms" or "time<1ms"
            for m in re.finditer(r"[=<](\d+(?:\.\d+)?)ms", output, re.IGNORECASE):
                latencies.append(float(m.group(1)))
        else:
            # Linux/macOS: "time=12.3 ms"
            for m in re.finditer(r"time[=<](\d+(?:\.\d+)?)\s*ms", output, re.IGNORECASE):
                latencies.append(float(m.group(1)))

        # 解析丢包率
        loss = 100.0
        loss_match = re.search(r"(\d+(?:\.\d+)?)%", output)
        if loss_match:
            loss = float(loss_match.group(1))
        elif latencies:
            loss = round((1 - len(latencies) / count) * 100, 1)

        return latencies, loss

    @staticmethod
    def _tcp_ping(host: str, port: int, count: int = PROBE_COUNT) -> tuple[list[float], float]:
        """
        TCP 连接探测：测量 TCP 握手耗时。
        返回 (延迟列表_ms, 丢包率_%)。
        """
        latencies: list[float] = []
        failures = 0

        for _ in range(count):
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            sock.settimeout(CONNECT_TIMEOUT)
            start = time.perf_counter()
            try:
                sock.connect((host, port))
                elapsed = (time.perf_counter() - start) * 1000
                latencies.append(elapsed)
            except (socket.timeout, OSError):
                failures += 1
            finally:
                sock.close()

        loss = round(failures / count * 100, 1) if count > 0 else 100.0
        return latencies, loss

    @staticmethod
    def _http_ping(host: str, port: int | None, count: int = PROBE_COUNT) -> tuple[list[float], float]:
        """
        HTTP HEAD 请求探测：测量 HTTP 响应耗时。
        返回 (延迟列表_ms, 丢包率_%)。
        """
        import urllib.request

        if host.startswith(("http://", "https://")):
            url = host
        else:
            scheme = "https" if port in (443, None) else "http"
            port_suffix = f":{port}" if port and port not in (80, 443) else ""
            url = f"{scheme}://{host}{port_suffix}"

        latencies: list[float] = []
        failures = 0

        for _ in range(count):
            req = urllib.request.Request(url, method="HEAD")
            start = time.perf_counter()
            try:
                with urllib.request.urlopen(req, timeout=CONNECT_TIMEOUT):
                    elapsed = (time.perf_counter() - start) * 1000
                    latencies.append(elapsed)
            except Exception:
                failures += 1

        loss = round(failures / count * 100, 1) if count > 0 else 100.0
        return latencies, loss
