"""
配置管理模块

负责 Agent 本地配置文件 (agent.yaml) 的读写与校验。
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Optional

import yaml

logger = logging.getLogger(__name__)

# 默认配置文件路径
def _default_config_dir() -> Path:
    if os.name == "nt":
        return Path(os.environ.get("APPDATA", ".")) / "collei-agent"
    # 非 Windows：root 使用系统目录，普通用户使用 XDG 用户目录
    if os.getuid() == 0:
        return Path("/etc/collei-agent")
    xdg_config = os.environ.get("XDG_CONFIG_HOME", "")
    base = Path(xdg_config) if xdg_config else Path.home() / ".config"
    return base / "collei-agent"

DEFAULT_CONFIG_DIR = _default_config_dir()
DEFAULT_CONFIG_PATH = DEFAULT_CONFIG_DIR / "agent.yaml"


@dataclass
class AgentConfig:
    """Agent 本地持久化配置"""

    server_url: str = ""
    uuid: str = ""
    token: str = ""
    network_interface: str = ""  # 指定监测网卡名称（为空则使用全部网卡汇总）

    # --- 运行时参数（不持久化） ---
    reg_token: str = field(default="", repr=False)
    name: str = ""
    report_interval: float = 3.0  # 上报间隔（秒）
    verify_interval: float = 5.0  # 等待审批轮询间隔（秒）
    force_register: bool = False  # 强制重新注册（覆盖本地配置）

    # 配置文件路径（运行时）
    config_path: str = field(default="", repr=False)

    # ---- 序列化 ----

    _PERSISTENT_KEYS = ("server_url", "uuid", "token", "network_interface")

    def to_yaml_dict(self) -> dict:
        """仅导出需要持久化的字段"""
        return {k: getattr(self, k) for k in self._PERSISTENT_KEYS if getattr(self, k)}

    def is_registered(self) -> bool:
        """是否已完成注册（本地存有 token）"""
        return bool(self.server_url and self.token)

    def save(self, path: Optional[str | Path] = None) -> Path:
        """将配置保存到 YAML 文件"""
        p = Path(path or self.config_path or DEFAULT_CONFIG_PATH)
        p.parent.mkdir(parents=True, exist_ok=True)
        data = self.to_yaml_dict()
        with open(p, "w", encoding="utf-8") as f:
            yaml.safe_dump(data, f, default_flow_style=False, allow_unicode=True)
        logger.info("配置已保存至 %s", p)
        return p

    @classmethod
    def load(cls, path: Optional[str | Path] = None) -> "AgentConfig":
        """从 YAML 文件加载配置"""
        p = Path(path or DEFAULT_CONFIG_PATH)
        cfg = cls(config_path=str(p))
        if p.exists():
            with open(p, "r", encoding="utf-8") as f:
                data = yaml.safe_load(f) or {}
            cfg.server_url = data.get("server_url", "")
            cfg.uuid = data.get("uuid", "")
            cfg.token = data.get("token", "")
            cfg.network_interface = data.get("network_interface", "")
            logger.info("已加载配置文件 %s", p)
        else:
            logger.info("配置文件不存在: %s，将使用默认配置", p)
        return cfg

    def validate_for_register(self) -> None:
        """校验自动注册所需参数"""
        if not self.server_url:
            raise ValueError("缺少 server_url（--url）")
        if not self.reg_token:
            raise ValueError("缺少 reg_token（--reg-token）")

    def validate_for_passive(self) -> None:
        """校验被动注册所需参数"""
        if not self.server_url:
            raise ValueError("缺少 server_url（--url）")
        if not self.token:
            raise ValueError("缺少 token（--token）")
