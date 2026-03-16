"""
Agent 核心生命周期模块

实现完整的 Agent 状态机：
  启动 → 检查配置 → 注册/验证 → 审批等待 → 上报循环 → 错误处理
"""

from __future__ import annotations

import logging
import signal
import threading
import time
from enum import Enum, auto
from pathlib import Path
from typing import Optional

import httpx

from collei import __version__
from collei.api_client import (
    ColleiApiClient,
    BackoffHelper,
    ApiError,
    TokenInvalid,
    ServerNotApproved,
)
from collei.collector import SystemCollector
from collei.config import AgentConfig, DEFAULT_CONFIG_DIR
from collei.network_monitor import NetworkMonitor

logger = logging.getLogger(__name__)


class AgentState(Enum):
    """Agent 运行状态"""
    INIT = auto()
    REGISTERING = auto()
    VERIFYING = auto()
    WAITING_APPROVAL = auto()
    REPORTING = auto()
    STOPPED = auto()


class ColleiAgent:
    """
    Collei Agent 核心控制器

    管理完整生命周期：初始化 → 注册/验证 → 上报循环
    """

    def __init__(self, config: AgentConfig) -> None:
        self.config = config
        self.state = AgentState.INIT
        self._running = False
        self._stop_event = threading.Event()

        self._api: Optional[ColleiApiClient] = None
        self._ssh_manager = None  # SSHTunnelManager（延迟初始化）
        state_dir = str(Path(config.config_path).parent) if config.config_path else str(DEFAULT_CONFIG_DIR)
        self._collector = SystemCollector(
            network_interface=config.network_interface,
            state_dir=state_dir,
        )
        self._network = NetworkMonitor(stop_event=self._stop_event)
        self._backoff = BackoffHelper(stop_event=self._stop_event)

    # ---- 生命周期 ----

    def run(self) -> None:
        """启动 Agent 主循环（阻塞）"""
        self._running = True
        self._setup_signal_handlers()

        logger.info("Collei Agent v%s 启动", __version__)
        logger.info("控制端: %s", self.config.server_url)

        self._api = ColleiApiClient(self.config.server_url)

        try:
            self._main_loop()
        except KeyboardInterrupt:
            logger.info("收到中断信号，正在停止...")
        finally:
            self._shutdown()

    def stop(self) -> None:
        """请求停止 Agent"""
        logger.info("正在请求停止 Agent...")
        self._running = False
        self._stop_event.set()

    def _interruptible_sleep(self, seconds: float) -> None:
        """可中断的等待，收到停止信号后立即返回"""
        self._stop_event.wait(seconds)

    # ---- 主循环 ----

    def _main_loop(self) -> None:
        """Agent 主状态机循环"""

        # 第一步：注册或验证
        self._initial_handshake()

        # 第二步：进入上报/等待循环
        while self._running:
            try:
                if self.state == AgentState.WAITING_APPROVAL:
                    self._poll_approval()
                elif self.state == AgentState.REPORTING:
                    self._report_loop()
                elif self.state == AgentState.STOPPED:
                    break
                else:
                    logger.error("意外状态: %s，停止运行", self.state)
                    break
            except KeyboardInterrupt:
                raise
            except Exception as exc:
                logger.error("主循环异常: %s", exc, exc_info=True)
                if self._running:
                    self._backoff.wait()

    # ---- 注册 / 验证 ----

    def _initial_handshake(self) -> None:
        """
        初始握手流程：
        - 如果设置了 force_register 且有 reg_token → 强制重新注册
        - 如果本地已有 token → verify 验证身份
        - 如果有 reg_token → register 自动注册
        - 如果有 token 但无 uuid → 被动注册的 verify
        """
        assert self._api is not None

        while self._running:
            try:
                if self.config.force_register and self.config.reg_token:
                    # 强制重新注册模式
                    logger.info("强制重新注册模式已启用，将覆盖本地配置")
                    self._do_register()
                    return
                elif self.config.is_registered():
                    # 本地已有配置，验证身份
                    self._do_verify()
                    return
                elif self.config.reg_token:
                    # 自动注册模式
                    self._do_register()
                    return
                elif self.config.token:
                    # 被动注册（有 token 无 uuid）
                    self._do_verify()
                    return
                else:
                    logger.error("无可用的认证信息（缺少 token 或 reg_token），无法启动")
                    self.state = AgentState.STOPPED
                    return
            except TokenInvalid:
                logger.error("认证失败: Token/密钥无效，请检查配置后重试")
                self.state = AgentState.STOPPED
                return
            except httpx.RequestError as exc:
                logger.warning("连接控制端失败: %s", exc)
                self._backoff.wait()
            except ApiError as exc:
                logger.error("注册/验证 API 错误: %s", exc)
                self._backoff.wait()

    def _do_register(self) -> None:
        """执行自动注册"""
        assert self._api is not None

        self.state = AgentState.REGISTERING
        logger.info("正在自动注册 (全局密钥模式)...")

        hw = self._collector.collect_hardware()
        name = self.config.name or _default_hostname()

        resp = self._api.register(
            reg_token=self.config.reg_token,
            name=name,
            hardware=hw.to_dict(),
            version=__version__,
        )

        # 保存注册返回的凭据
        self.config.uuid = resp.uuid
        self.config.token = resp.token
        self.config.save()
        self._backoff.reset()

        logger.info("注册成功! UUID=%s", resp.uuid)
        logger.info("服务器待审核 (is_approved=0)，进入等待模式")
        self.state = AgentState.WAITING_APPROVAL

    def _do_verify(self) -> None:
        """执行验证"""
        assert self._api is not None

        self.state = AgentState.VERIFYING
        logger.info("正在验证身份...")

        hw = self._collector.collect_hardware()
        name = self.config.name or _default_hostname()

        resp = self._api.verify(
            token=self.config.token,
            name=name,
            hardware=hw.to_dict(),
            version=__version__,
        )

        # 更新本地配置
        self.config.uuid = resp.uuid
        self.config.token = resp.token
        self.config.save()
        self._backoff.reset()

        logger.info("验证成功! UUID=%s, is_approved=%d", resp.uuid, resp.is_approved)

        if resp.is_approved == 1:
            self._network.handle_dispatch(resp.network_dispatch)
            self.state = AgentState.REPORTING
        else:
            logger.info("服务器尚未被批准，进入等待模式")
            self.state = AgentState.WAITING_APPROVAL

    # ---- 审批等待 ----

    def _poll_approval(self) -> None:
        """等待管理员审批，定期轮询 verify 接口"""
        assert self._api is not None

        logger.info("等待审批中... (轮询间隔 %.0fs)", self.config.verify_interval)

        while self._running and self.state == AgentState.WAITING_APPROVAL:
            self._interruptible_sleep(self.config.verify_interval)
            if not self._running:
                break

            try:
                resp = self._api.verify(
                    token=self.config.token,
                    version=__version__,
                )
                if resp.is_approved == 1:
                    logger.info("服务器已被批准! 开始上报数据")
                    self.state = AgentState.REPORTING
                    self._backoff.reset()
                    return
                else:
                    logger.debug("仍在等待审批...")
            except TokenInvalid:
                logger.error("Token 已失效，停止运行")
                self.state = AgentState.STOPPED
                return
            except (httpx.RequestError, ApiError) as exc:
                logger.warning("轮询审批状态失败: %s", exc)

    # ---- 数据上报 ----

    def _report_loop(self) -> None:
        """持续上报循环"""
        assert self._api is not None

        logger.info("进入上报循环 (间隔 %.1fs)", self.config.report_interval)

        # 初始化网络计数器，并等待后台 CPU 采样线程完成首次采样
        self._collector.collect_load()
        self._interruptible_sleep(min(1.0, self.config.report_interval))

        while self._running and self.state == AgentState.REPORTING:
            try:
                # 采集数据
                load = self._collector.collect_load()
                hw_changes = self._collector.collect_hardware_if_changed()
                total_flow_in, total_flow_out = self._collector.collect_total_flow()
                network_results = self._network.flush_pending_results()
                # 上报
                resp = self._api.report(
                    token=self.config.token,
                    hardware=hw_changes,
                    load_data=load.to_dict(),
                    total_flow_in=total_flow_in,
                    total_flow_out=total_flow_out,
                    network_version=self._network.version,
                    network_data=network_results or None,
                )

                if resp.received:
                    self._collector.confirm_net_reported()
                    logger.debug(
                        "上报成功: cpu=%.1f%% ram=%d net_in=%d net_out=%d",
                        load.cpu, load.ram, load.net_in, load.net_out,
                    )
                self._network.handle_dispatch(resp.network_dispatch)
                self._handle_ssh_tunnel(resp.ssh_tunnel)
                self._backoff.reset()

            except ServerNotApproved:
                logger.warning("服务器审批状态已变更，回退至等待模式")
                self.state = AgentState.WAITING_APPROVAL
                return

            except TokenInvalid:
                logger.error("Token 无效，停止上报")
                self.state = AgentState.STOPPED
                return

            except httpx.RequestError as exc:
                logger.warning("上报失败 (网络): %s", exc)
                self._backoff.wait()
                continue

            except ApiError as exc:
                if exc.status_code >= 500:
                    logger.warning(
                        "服务端错误 %d (%s)，退避重试",
                        exc.status_code, exc.detail or "no detail",
                    )
                    self._backoff.wait()
                    continue
                elif exc.status_code == 429:
                    retry_after = _parse_retry_after(exc.headers)
                    if retry_after:
                        logger.warning("请求频率过高，等待 %.0f 秒后重试", retry_after)
                        self._interruptible_sleep(retry_after)
                    else:
                        logger.warning("请求频率过高，退避重试")
                        self._backoff.wait()
                    continue
                else:
                    logger.error("上报异常 %d: %s", exc.status_code, exc.detail)
                    self._backoff.wait()
                    continue

            # 正常间隔等待
            self._interruptible_sleep(self.config.report_interval)

    # ---- 信号处理 & 清理 ----

    def _setup_signal_handlers(self) -> None:
        try:
            signal.signal(signal.SIGTERM, self._handle_signal)
        except (OSError, ValueError):
            # 在某些环境（如线程中）无法注册信号处理器
            pass

    def _handle_signal(self, signum: int, frame) -> None:
        logger.info("收到信号 %d，准备停止", signum)
        self.stop()

    def _shutdown(self) -> None:
        """清理资源"""
        self.state = AgentState.STOPPED
        self._network.stop()
        if self._ssh_manager is not None:
            self._ssh_manager.stop()
            self._ssh_manager = None
        if self._api:
            self._api.close()
            self._api = None
        logger.info("Agent 已停止")


# ---------------------------------------------------------------------------
# SSH 隧道处理
# ---------------------------------------------------------------------------

    def _handle_ssh_tunnel(self, ssh_tunnel: Optional[dict]) -> None:
        """
        根据 report 响应中的 ssh_tunnel 字段管理 SSH 隧道连接。

        - ssh_tunnel.connect = true  → 建立隧道 WS
        - ssh_tunnel.connect = false → 断开隧道 WS
        - ssh_tunnel = null          → 维持现状
        """
        if not self.config.ssh.enabled or ssh_tunnel is None:
            return

        connect = ssh_tunnel.get("connect")

        if connect is True:
            if self._ssh_manager is None:
                from collei.ssh_tunnel import SSHTunnelManager
                self._ssh_manager = SSHTunnelManager(
                    api_url=self.config.server_url,
                    token=self.config.token,
                    ssh_port=self.config.ssh.port,
                )
            self._ssh_manager.connect()
        elif connect is False:
            if self._ssh_manager is not None:
                self._ssh_manager.disconnect()

# ---------------------------------------------------------------------------
# 工具
# ---------------------------------------------------------------------------

def _default_hostname() -> str:
    """获取默认主机名"""
    import socket
    try:
        return socket.gethostname()
    except Exception:
        return "unknown"


def _parse_retry_after(headers: dict) -> float:
    """
    解析 Retry-After 响应头，返回需要等待的秒数。
    支持两种格式：
      - 整数秒: ``Retry-After: 30``
      - HTTP 日期: ``Retry-After: Wed, 21 Oct 2015 07:28:00 GMT``
    解析失败时返回 0。
    """
    value = headers.get("retry-after") or headers.get("Retry-After", "")
    if not value:
        return 0.0
    # 尝试解析为秒数
    try:
        return max(0.0, float(value))
    except ValueError:
        pass
    # 尝试解析为 HTTP 日期
    from email.utils import parsedate_to_datetime
    import datetime
    try:
        dt = parsedate_to_datetime(value)
        delta = (dt - datetime.datetime.now(datetime.timezone.utc)).total_seconds()
        return max(0.0, delta)
    except Exception:
        pass
    return 0.0
