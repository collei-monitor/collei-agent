"""
SSH 隧道模块

实现 Agent 到 Backend 的 WebSocket 隧道，用于 Web SSH 功能。
Agent 作为 TCP 代理（Dumb Pipe），在 Backend 与本地 sshd 之间透传字节流。
Agent 不参与任何 SSH 认证逻辑。

隧道 WS 由 /agent/report 响应中的 ssh_tunnel 字段按需触发：
  - ssh_tunnel.connect = true  → 建立 WS 连接
  - ssh_tunnel.connect = false → 断开 WS 连接
  - ssh_tunnel = null          → 维持当前状态
"""

from __future__ import annotations

import asyncio
import json
import logging
import random
import threading
from typing import Optional

logger = logging.getLogger(__name__)


class SSHTunnel:
    """SSH 会话管理器，维护 session_id → TCP 连接映射，支持多会话复用"""

    def __init__(self) -> None:
        self.sessions: dict[str, tuple[asyncio.StreamReader, asyncio.StreamWriter]] = {}

    async def open(
        self, session_id: str, port: int
    ) -> tuple[asyncio.StreamReader, asyncio.StreamWriter]:
        """建立到 localhost:<port> 的 TCP 连接并注册到会话映射"""
        reader, writer = await asyncio.open_connection("127.0.0.1", port)
        self.sessions[session_id] = (reader, writer)
        return reader, writer

    async def close(self, session_id: str) -> None:
        """关闭指定会话的 TCP 连接"""
        pair = self.sessions.pop(session_id, None)
        if pair:
            try:
                pair[1].close()
                await pair[1].wait_closed()
            except Exception:
                pass

    def get(
        self, session_id: str
    ) -> Optional[tuple[asyncio.StreamReader, asyncio.StreamWriter]]:
        return self.sessions.get(session_id)

    async def close_all(self) -> None:
        """关闭所有活跃会话"""
        for session_id in list(self.sessions.keys()):
            await self.close(session_id)


class SSHTunnelManager:
    """
    SSH 隧道管理器

    在独立线程中运行 asyncio 事件循环，管理到 Backend 的 WebSocket 隧道连接。
    由 report 响应中的 ssh_tunnel 字段驱动连接/断开。

    隧道 WS 是长连接，可同时承载多个 SSH 会话，每个 session_id 对应一条
    独立的 TCP 连接到 localhost:<ssh_port>。
    """

    def __init__(self, api_url: str, token: str, ssh_port: int) -> None:
        self._api_url = api_url.rstrip("/")
        self._token = token
        self._ssh_port = ssh_port

        self._wanted = False       # 是否需要维持隧道
        self._connected = False    # 当前是否已连接

        self._loop: Optional[asyncio.AbstractEventLoop] = None
        self._thread: Optional[threading.Thread] = None
        self._async_stop: Optional[asyncio.Event] = None

    @property
    def is_connected(self) -> bool:
        return self._connected

    def connect(self) -> None:
        """请求建立隧道（由 report 响应 ssh_tunnel.connect=true 触发）"""
        if self._wanted:
            return
        self._wanted = True
        logger.info("SSH 隧道: 收到连接指令，正在启动隧道...")
        self._start_thread()

    def disconnect(self) -> None:
        """请求断开隧道（由 report 响应 ssh_tunnel.connect=false 触发）"""
        if not self._wanted:
            return
        self._wanted = False
        logger.info("SSH 隧道: 收到断开指令，正在关闭隧道...")
        self._signal_stop()

    def stop(self) -> None:
        """停止隧道管理器（Agent 关闭时调用）"""
        self._wanted = False
        self._signal_stop()
        if self._thread and self._thread.is_alive():
            self._thread.join(timeout=5)
        self._thread = None

    # ---- 内部方法 ----

    def _signal_stop(self) -> None:
        """线程安全地通知异步事件循环停止"""
        loop = self._loop
        stop_event = self._async_stop
        if loop and stop_event:
            try:
                loop.call_soon_threadsafe(stop_event.set)
            except RuntimeError:
                pass

    def _start_thread(self) -> None:
        if self._thread and self._thread.is_alive():
            return
        self._thread = threading.Thread(
            target=self._run_loop,
            daemon=True,
            name="ssh-tunnel",
        )
        self._thread.start()

    def _run_loop(self) -> None:
        """在独立线程中运行异步事件循环"""
        self._loop = asyncio.new_event_loop()
        asyncio.set_event_loop(self._loop)
        self._async_stop = asyncio.Event()
        try:
            self._loop.run_until_complete(self._maintain_tunnel())
        except Exception as exc:
            logger.error("SSH 隧道事件循环异常退出: %s", exc)
        finally:
            self._loop.close()
            self._loop = None
            self._async_stop = None
            self._connected = False

    def _ws_url(self) -> str:
        """构建隧道 WebSocket URL"""
        base = self._api_url
        if base.startswith("https://"):
            ws_base = "wss://" + base[len("https://"):]
        elif base.startswith("http://"):
            ws_base = "ws://" + base[len("http://"):]
        else:
            ws_base = "ws://" + base
        return f"{ws_base}/api/v1/agent/ws/ssh?token={self._token}"

    # ---- 隧道维持与重连 ----

    async def _maintain_tunnel(self) -> None:
        """维持隧道连接，断线时指数退避重连"""
        import websockets

        assert self._async_stop is not None
        delay = 1.0

        while self._wanted:
            try:
                url = self._ws_url()
                logger.info("SSH 隧道: 正在连接 %s", url.split("?")[0])

                async with websockets.connect(url) as ws:
                    self._connected = True
                    delay = 1.0  # 连接成功后重置退避
                    logger.info("SSH 隧道: WebSocket 连接已建立")

                    # 连接后首帧：能力上报
                    await ws.send(json.dumps({
                        "type": "capabilities",
                        "ssh_port": self._ssh_port,
                    }))
                    logger.debug(
                        "SSH 隧道: 已发送 capabilities (ssh_port=%d)",
                        self._ssh_port,
                    )

                    # 进入消息处理循环
                    await self._handle_messages(ws)

            except asyncio.CancelledError:
                break
            except Exception as exc:
                logger.warning("SSH 隧道连接异常: %s", exc)
            finally:
                self._connected = False

            if not self._wanted:
                break

            # 指数退避重连: min(initial * 2^n, 30s) + jitter
            jitter = random.uniform(0, delay * 0.1)
            wait_time = delay + jitter
            logger.info("SSH 隧道: %.1f 秒后重连...", wait_time)

            try:
                await asyncio.wait_for(
                    self._async_stop.wait(), timeout=wait_time
                )
                break  # stop event 被设置，退出重连
            except asyncio.TimeoutError:
                pass

            delay = min(delay * 2, 30.0)

        self._connected = False
        logger.info("SSH 隧道: 已退出")

    # ---- 消息处理 ----

    async def _handle_messages(self, ws) -> None:
        """处理隧道 WS 消息循环"""
        tunnel = SSHTunnel()
        pending_session_id: Optional[str] = None
        tasks: dict[str, asyncio.Task] = {}

        try:
            async for msg in ws:
                # 检查停止信号
                if self._async_stop and self._async_stop.is_set():
                    break

                # Binary 帧 → 转发到对应 session 的 TCP socket
                if isinstance(msg, bytes):
                    if pending_session_id:
                        pair = tunnel.get(pending_session_id)
                        if pair:
                            try:
                                pair[1].write(msg)
                                await pair[1].drain()
                            except Exception as exc:
                                logger.warning(
                                    "SSH 隧道: TCP 写入失败 (session=%s): %s",
                                    pending_session_id, exc,
                                )
                                await self._send_tunnel_closed(
                                    ws, pending_session_id, "tcp_write_error"
                                )
                                await self._close_session(
                                    tunnel, tasks, pending_session_id
                                )
                        pending_session_id = None
                    continue

                # JSON text 帧
                try:
                    data = json.loads(msg)
                except (json.JSONDecodeError, TypeError):
                    logger.warning("SSH 隧道: 收到无效 JSON")
                    continue

                msg_type = data.get("type")

                if msg_type == "open_tunnel":
                    session_id = data["session_id"]
                    logger.debug("SSH 隧道: 收到 open_tunnel (session=%s)", session_id)
                    task = asyncio.create_task(
                        self._handle_open_tunnel(ws, tunnel, session_id, tasks)
                    )
                    tasks[session_id] = task

                elif msg_type == "data":
                    pending_session_id = data.get("session_id")

                elif msg_type == "close_session":
                    session_id = data["session_id"]
                    logger.debug("SSH 隧道: 收到 close_session (session=%s)", session_id)
                    await self._close_session(tunnel, tasks, session_id)

                elif msg_type == "disconnect":
                    logger.info("SSH 隧道: 收到 disconnect 指令")
                    self._wanted = False
                    break

                elif msg_type == "ping":
                    await ws.send(json.dumps({"type": "pong"}))

                else:
                    logger.debug("SSH 隧道: 收到未知消息类型: %s", msg_type)

        finally:
            # 清理所有活跃会话
            for task in tasks.values():
                if not task.done():
                    task.cancel()
            if tasks:
                await asyncio.gather(*tasks.values(), return_exceptions=True)
            await tunnel.close_all()
            logger.debug("SSH 隧道: 消息循环结束，已清理所有会话")

    async def _handle_open_tunnel(
        self,
        ws,
        tunnel: SSHTunnel,
        session_id: str,
        tasks: dict[str, asyncio.Task],
    ) -> None:
        """处理 open_tunnel 指令：建立到 localhost:ssh_port 的 TCP 连接并启动桥接"""
        try:
            reader, writer = await tunnel.open(session_id, self._ssh_port)
        except Exception as exc:
            logger.warning(
                "SSH 隧道: TCP 连接 sshd 失败 (session=%s, port=%d): %s",
                session_id, self._ssh_port, exc,
            )
            await self._send_tunnel_closed(
                ws, session_id, f"connection_failed: {exc}"
            )
            tasks.pop(session_id, None)
            return

        logger.info("SSH 隧道: 隧道已建立 (session=%s)", session_id)
        await ws.send(json.dumps({
            "type": "tunnel_ready",
            "session_id": session_id,
        }))

        # TCP → WS 方向桥接（WS → TCP 由消息循环中的 data 处理）
        try:
            await self._bridge_tcp_to_ws(reader, ws, session_id)
        except asyncio.CancelledError:
            pass
        except Exception as exc:
            logger.debug(
                "SSH 隧道: TCP→WS 桥接结束 (session=%s): %s", session_id, exc
            )
        finally:
            await tunnel.close(session_id)
            tasks.pop(session_id, None)

    async def _bridge_tcp_to_ws(
        self,
        reader: asyncio.StreamReader,
        ws,
        session_id: str,
    ) -> None:
        """TCP → WS 方向桥接：从 TCP socket 读取数据，发送到 WS"""
        while True:
            data = await reader.read(32768)  # 32KB 块
            if not data:
                # TCP EOF
                logger.debug("SSH 隧道: TCP EOF (session=%s)", session_id)
                await self._send_tunnel_closed(ws, session_id, "tcp_eof")
                return
            # 先发 JSON 帧标明 session_id，再发 Binary 帧
            await ws.send(json.dumps({
                "type": "data",
                "session_id": session_id,
            }))
            await ws.send(data)

    @staticmethod
    async def _send_tunnel_closed(ws, session_id: str, reason: str) -> None:
        """发送 tunnel_closed 帧（静默处理发送失败）"""
        try:
            await ws.send(json.dumps({
                "type": "tunnel_closed",
                "session_id": session_id,
                "reason": reason,
            }))
        except Exception:
            pass

    @staticmethod
    async def _close_session(
        tunnel: SSHTunnel,
        tasks: dict[str, asyncio.Task],
        session_id: str,
    ) -> None:
        """关闭指定会话：取消桥接任务 + 关闭 TCP 连接"""
        task = tasks.pop(session_id, None)
        if task and not task.done():
            task.cancel()
        await tunnel.close(session_id)
        logger.debug("SSH 隧道: 会话已关闭 (session=%s)", session_id)
