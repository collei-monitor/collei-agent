"""
API 客户端模块

负责与 Collei 控制端的 HTTP 通信，包含注册、验证和数据上报接口。
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from enum import Enum
from typing import Any, Optional

import httpx

logger = logging.getLogger(__name__)

# 默认超时（秒）
DEFAULT_TIMEOUT = 15.0


class ApiError(Exception):
    """API 调用异常"""

    def __init__(self, status_code: int, detail: str = "", headers: Optional[dict] = None):
        self.status_code = status_code
        self.detail = detail
        self.headers: dict = headers or {}
        super().__init__(f"HTTP {status_code}: {detail}")


class TokenInvalid(ApiError):
    """401 - Token 无效"""
    pass


class ServerNotApproved(ApiError):
    """403 - 服务器未被批准"""
    pass


class RegistrationNotConfigured(ApiError):
    """503 - 未配置全局注册密钥"""
    pass


# ---------------------------------------------------------------------------
# 响应数据模型
# ---------------------------------------------------------------------------

@dataclass
class RegisterResponse:
    uuid: str
    token: str


@dataclass
class VerifyResponse:
    uuid: str
    token: str
    is_approved: int
    network_dispatch: Optional[dict] = None


@dataclass
class ReportResponse:
    uuid: str
    is_approved: int
    received: bool
    network_dispatch: Optional[dict] = None
    ssh_tunnel: Optional[dict] = None
    pending_tasks: Optional[list[dict]] = None


# ---------------------------------------------------------------------------
# API 客户端
# ---------------------------------------------------------------------------

class ColleiApiClient:
    """Collei 控制端 API 客户端"""

    def __init__(self, base_url: str, timeout: float = DEFAULT_TIMEOUT) -> None:
        self.base_url = base_url.rstrip("/")
        self._agent_base = f"{self.base_url}/api/v1/agent"
        self.timeout = timeout
        self._client = httpx.Client(timeout=timeout)

    def close(self) -> None:
        self._client.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()

    # ---- 公开接口 ----

    def register(
        self,
        reg_token: str,
        name: str,
        hardware: Optional[dict] = None,
        version: str = "",
    ) -> RegisterResponse:
        """
        自动注册（全局密钥模式）
        POST /api/v1/agent/register
        """
        payload: dict[str, Any] = {
            "reg_token": reg_token,
            "name": name,
        }
        if hardware:
            payload.update(hardware)
        if version:
            payload["version"] = version

        data = self._post(f"{self._agent_base}/register", payload)
        return RegisterResponse(uuid=data["uuid"], token=data["token"])

    def verify(
        self,
        token: str,
        name: str = "",
        hardware: Optional[dict] = None,
        version: str = "",
    ) -> VerifyResponse:
        """
        被动注册验证 / 身份验证
        POST /api/v1/agent/verify
        """
        payload: dict[str, Any] = {"token": token}
        if name:
            payload["name"] = name
        if hardware:
            payload.update(hardware)
        if version:
            payload["version"] = version

        data = self._post(f"{self._agent_base}/verify", payload)
        return VerifyResponse(
            uuid=data["uuid"],
            token=data["token"],
            is_approved=data.get("is_approved", 0),
            network_dispatch=data.get("network_dispatch"),
        )

    def report(
        self,
        token: str,
        hardware: Optional[dict] = None,
        load_data: Optional[dict] = None,
        total_flow_in: Optional[int] = None,
        total_flow_out: Optional[int] = None,
        network_version: Optional[str] = None,
        network_data: Optional[list[dict]] = None,
    ) -> ReportResponse:
        """
        混合上报（硬件信息 + 监控数据 + 网络探测结果）
        POST /api/v1/agent/report
        """
        payload: dict[str, Any] = {"token": token}
        if hardware:
            payload.update(hardware)
        if load_data:
            payload["load_data"] = load_data
        if total_flow_in is not None:
            payload["total_flow_in"] = total_flow_in
        if total_flow_out is not None:
            payload["total_flow_out"] = total_flow_out
        if network_version is not None:
            payload["network_version"] = network_version
        if network_data:
            payload["network_data"] = network_data
        data = self._post(f"{self._agent_base}/report", payload)
        return ReportResponse(
            uuid=data["uuid"],
            is_approved=data.get("is_approved", 1),
            received=data.get("received", False),
            network_dispatch=data.get("network_dispatch"),
            ssh_tunnel=data.get("ssh_tunnel"),
            pending_tasks=data.get("pending_tasks"),
        )

    def report_task(
        self,
        execution_id: str,
        status: str,
        exit_code: Optional[int] = None,
        output: Optional[str] = None,
    ) -> None:
        """
        上报任务执行结果
        POST /api/v1/agent/tasks/report
        """
        payload: dict[str, Any] = {
            "execution_id": execution_id,
            "status": status,
        }
        if exit_code is not None:
            payload["exit_code"] = exit_code
        if output is not None:
            payload["output"] = output

        self._post(f"{self._agent_base}/tasks/report", payload)

    # ---- HTTP 层 ----

    def _post(self, url: str, payload: dict) -> dict:
        """执行 POST 请求并处理错误"""
        logger.debug("POST %s  payload_keys=%s", url, list(payload.keys()))
        try:
            resp = self._client.post(url, json=payload)
        except httpx.RequestError as exc:
            logger.error("请求失败: %s", exc)
            raise

        return self._handle_response(resp)

    @staticmethod
    def _handle_response(resp: httpx.Response) -> dict:
        """解析响应，按状态码抛出对应异常"""
        if resp.status_code in (200, 201):
            data = resp.json()
            logger.debug("响应: %s", data)
            return data

        # 尝试解析错误详情
        detail = _extract_detail(resp)

        logger.debug("API 错误 %d: %s", resp.status_code, detail)

        headers = dict(resp.headers)
        if resp.status_code == 401:
            raise TokenInvalid(resp.status_code, detail, headers)
        elif resp.status_code == 403:
            raise ServerNotApproved(resp.status_code, detail, headers)
        elif resp.status_code == 503:
            raise RegistrationNotConfigured(resp.status_code, detail, headers)
        else:
            raise ApiError(resp.status_code, detail, headers)


# ---------------------------------------------------------------------------
# 内部工具
# ---------------------------------------------------------------------------

def _extract_detail(resp: httpx.Response) -> str:
    """
    从响应中提取人类可读的错误描述。

    优先级：
      1. JSON body 中的 "detail" 字段
      2. JSON body 的完整字符串表示
      3. 纯文本 body（截断至 200 字符）
      4. HTTP 原因短语（如 "Bad Gateway"）
      5. 状态码字符串
    """
    # 尝试解析 JSON
    try:
        body = resp.json()
        if isinstance(body, dict):
            if "detail" in body:
                return str(body["detail"])
            # 兼容 FastAPI 的 {"message": ...} 等格式
            for key in ("message", "error", "msg"):
                if key in body:
                    return str(body[key])
        return str(body)[:300]
    except Exception:
        pass

    # 纯文本 body
    text = (resp.text or "").strip()
    # 过滤掉 HTML（nginx 错误页等），只保留第一行有意义的内容
    if text and not text.startswith("<"):
        return text[:200]

    # 使用 HTTP 原因短语作为兜底
    reason = getattr(resp, "reason_phrase", None)
    if reason:
        # httpx 返回 bytes，需解码
        if isinstance(reason, bytes):
            reason = reason.decode("utf-8", errors="replace")
        return reason

    return str(resp.status_code)

