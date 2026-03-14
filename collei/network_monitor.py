"""
网络监控模块

负责管理探测目标、执行 ICMP/TCP/HTTP 探测，
并缓存探测结果供上报时使用。

调度策略 —— 按间隔分组 + 时钟对齐：
  - 将所有目标按 interval 分组，每组共用一个调度线程
  - 对"可对齐"间隔（能整除 60 或被 60 整除），自动对齐到墙钟边界
    例：interval=60 → 每分钟 :00 触发；interval=30 → :00 / :30 触发
  - 相同 interval 的目标天然合并，同一时刻并发探测
  - 探测结果存入线程安全的缓冲区，上报时一次性取出
"""

from __future__ import annotations

import logging
import math
import platform
import socket
import statistics
import subprocess
import re
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, asdict
from typing import Optional

logger = logging.getLogger(__name__)

# 每次探测发送的次数
PROBE_COUNT = 4
# TCP/HTTP 单次连接超时（秒）
CONNECT_TIMEOUT = 5.0
# 并发探测线程池大小
_PROBE_POOL_SIZE = 8


def _is_alignable(interval: int) -> bool:
    """判断探测间隔是否可对齐到墙钟边界。"""
    return interval > 0 and (60 % interval == 0 or interval % 60 == 0)


def _next_aligned_tick(interval: int, now: float | None = None) -> float:
    """
    计算下一个对齐的触发时间戳（秒）。

    对于可对齐间隔，返回 ceil(now / interval) * interval，
    确保触发点落在整分/整秒边界上。
    对于不可对齐间隔，直接返回 now + interval。
    """
    if now is None:
        now = time.time()

    if _is_alignable(interval):
        return math.ceil(now / interval) * interval
    return now + interval


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

    将探测目标按 interval 分组，每组一个调度线程，
    同组目标同一时刻并发探测，结果暂存到缓冲区供 report 时取出。
    """

    def __init__(self, stop_event: threading.Event) -> None:
        self._stop_event = stop_event

        self.version: Optional[str] = None
        self._targets: list[dict] = []

        # 调度线程管理：interval → (stop_event, thread)
        self._group_stops: dict[int, threading.Event] = {}
        self._group_threads: dict[int, threading.Thread] = {}

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
        """
        停掉所有旧调度线程，将目标按 interval 分组后重建。

        同一 interval 的目标共享调度线程，天然实现：
        1) 时钟对齐 — 组内所有目标同时触发
        2) 任务合并 — 新增同 interval 目标自动归入同一组
        """
        self._stop_all_probes()

        # 按 interval 分组
        groups: dict[int, list[dict]] = {}
        for target in self._targets:
            interval = target.get("interval", 60)
            groups.setdefault(interval, []).append(target)

        for interval, targets in groups.items():
            stop_evt = threading.Event()
            self._group_stops[interval] = stop_evt
            t = threading.Thread(
                target=self._interval_loop,
                args=(interval, targets, stop_evt),
                daemon=True,
                name=f"probe-group-{interval}s",
            )
            self._group_threads[interval] = t
            t.start()

            names = ", ".join(t_["name"] for t_ in targets if t_.get("name"))
            aligned = "对齐" if _is_alignable(interval) else "不对齐"
            logger.info(
                "调度组 interval=%ds (%s): %d 个目标 [%s]",
                interval, aligned, len(targets), names,
            )

    def _stop_all_probes(self) -> None:
        """通知所有调度线程停止。"""
        for evt in self._group_stops.values():
            evt.set()
        self._group_stops.clear()
        self._group_threads.clear()

    def stop(self) -> None:
        """停止所有探测（Agent 关闭时调用）。"""
        self._stop_all_probes()

    def _interval_loop(
        self,
        interval: int,
        targets: list[dict],
        stop_event: threading.Event,
    ) -> None:
        """
        单个间隔组的调度循环。

        对齐模式下，首次等待到下一个墙钟边界再开始；
        之后每次计算下一个 tick 并精确等待。
        """
        # 计算首次触发时间
        next_tick = _next_aligned_tick(interval)
        wait = max(0, next_tick - time.time())
        if wait > 0:
            logger.debug("调度组 %ds: 等待 %.1fs 后首次触发 (对齐到 %d)", interval, wait, int(next_tick))
            if stop_event.wait(wait) or self._stop_event.is_set():
                return

        while not stop_event.is_set() and not self._stop_event.is_set():
            # 记录本轮触发时间戳（任务开始时间）
            tick_time = int(time.time())

            # 并发探测本组所有目标
            self._fire_probes(targets, tick_time)

            # 计算下一个 tick 并等待
            next_tick = _next_aligned_tick(interval)
            wait = max(0, next_tick - time.time())
            if wait > 0:
                stop_event.wait(wait)

    def _fire_probes(self, targets: list[dict], tick_time: int) -> None:
        """并发执行一组探测，结果写入缓冲区。"""
        with ThreadPoolExecutor(max_workers=min(_PROBE_POOL_SIZE, len(targets))) as pool:
            futures = {
                pool.submit(self._probe_target, target, tick_time): target
                for target in targets
            }
            for future in as_completed(futures):
                target = futures[future]
                try:
                    result = future.result()
                    with self._results_lock:
                        self._pending_results.append(result)
                    logger.debug(
                        "探测完成: id=%d median=%.1fms loss=%.1f%%",
                        target["id"],
                        result.median_latency or 0,
                        result.packet_loss,
                    )
                except Exception as exc:
                    logger.warning("探测异常 (id=%d): %s", target["id"], exc)

    # ---- 探测实现 ----

    def _probe_target(self, target: dict, tick_time: int) -> ProbeResult:
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
            return ProbeResult(target_id=target["id"], time=tick_time, packet_loss=100.0)

        result = ProbeResult(
            target_id=target["id"],
            time=tick_time,
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
