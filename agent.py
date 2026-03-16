#!/usr/bin/env python3
"""
Collei Agent - 轻量级服务器监控探针

用法:
    # 自动注册（全局密钥模式）
    python agent.py run --url https://api.example.com --reg-token kfcfkxqsvw50

    # 强制重新注册（覆盖本地已有配置）
    python agent.py run --url https://api.example.com --reg-token kfcfkxqsvw50 --force
  
    # 被动注册（管理员下发 token）
    python agent.py run --url https://api.example.com --token <专属token>

    # 使用已有配置文件启动
    python agent.py run --config /etc/collei-agent/agent.yaml

    # 仅测试数据采集
    python agent.py collect

    # 显示版本
    python agent.py version
"""

from __future__ import annotations

import json
import logging
import sys

import click

from collei import __version__
from collei.config import AgentConfig, DEFAULT_CONFIG_PATH
from collei.collector import SystemCollector
from collei.core import ColleiAgent


def _setup_logging(verbose: bool = False, debug: bool = False) -> None:
    """配置日志"""
    if debug:
        level = logging.DEBUG
    elif verbose:
        level = logging.INFO
    else:
        level = logging.INFO

    fmt = "%(asctime)s [%(levelname)s] %(name)s: %(message)s"
    logging.basicConfig(level=level, format=fmt, datefmt="%Y-%m-%d %H:%M:%S")

    # 降低第三方库日志级别
    if not debug:
        logging.getLogger("httpx").setLevel(logging.WARNING)
        logging.getLogger("httpcore").setLevel(logging.WARNING)


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

@click.group()
@click.option("-v", "--verbose", is_flag=True, help="输出详细日志")
@click.option("--debug", is_flag=True, help="输出调试日志")
@click.pass_context
def cli(ctx: click.Context, verbose: bool, debug: bool) -> None:
    """Collei Agent - 服务器监控探针"""
    ctx.ensure_object(dict)
    ctx.obj["verbose"] = verbose
    ctx.obj["debug"] = debug
    _setup_logging(verbose=verbose, debug=debug)


@cli.command()
@click.option("--url", envvar="COLLEI_URL", help="控制端 API URL")
@click.option("--token", envvar="COLLEI_TOKEN", help="专属通信 token（被动注册）")
@click.option("--reg-token", envvar="COLLEI_REG_TOKEN", help="全局安装密钥（自动注册）")
@click.option("--name", default="", help="服务器显示名称")
@click.option("--config", "config_path", default=None, help="配置文件路径")
@click.option("--interval", default=3.0, type=float, help="上报间隔（秒），默认 3")
@click.option("--force", is_flag=True, help="强制重新注册，覆盖本地已有配置")
@click.pass_context
def run(
    ctx: click.Context,
    url: str | None,
    token: str | None,
    reg_token: str | None,
    name: str,
    config_path: str | None,
    interval: float,
    force: bool,
) -> None:
    """启动 Agent 主程序"""
    logger = logging.getLogger("collei.cli")

    # 加载配置文件
    cfg = AgentConfig.load(config_path)

    # 命令行参数覆盖配置文件
    if url:
        cfg.server_url = url.rstrip("/")
    if token:
        cfg.token = token
    if reg_token:
        cfg.reg_token = reg_token
    if name:
        cfg.name = name
    if interval > 0:
        cfg.report_interval = interval
    if config_path:
        cfg.config_path = config_path
    if force:
        cfg.force_register = True

    # 校验参数
    if not cfg.server_url:
        logger.error("缺少控制端 URL，请使用 --url 参数或在配置文件中指定 server_url")
        sys.exit(1)
    if not cfg.token and not cfg.reg_token:
        logger.error("缺少认证信息，请使用 --token 或 --reg-token 参数")
        sys.exit(1)

    # 启动 Agent
    agent = ColleiAgent(cfg)
    agent.run()


@cli.command()
def collect() -> None:
    """测试系统数据采集（不连接控制端）"""
    collector = SystemCollector()

    click.echo("=== 硬件信息 ===")
    hw = collector.collect_hardware()
    click.echo(json.dumps(hw.to_dict(), indent=2, ensure_ascii=False))

    click.echo("\n=== 实时监控数据 (首次采样) ===")
    load1 = collector.collect_load()
    click.echo(json.dumps(load1.to_dict(), indent=2, ensure_ascii=False))

    total_flow_in, total_flow_out = collector.collect_total_flow()
    click.echo(f"\n=== 累积流量 ===")
    click.echo(f"total_flow_in:  {total_flow_in}")
    click.echo(f"total_flow_out: {total_flow_out}")

    click.echo("\n等待 2 秒后再次采样...")
    import time
    time.sleep(2)

    click.echo("\n=== 实时监控数据 (二次采样) ===")
    load2 = collector.collect_load()
    click.echo(json.dumps(load2.to_dict(), indent=2, ensure_ascii=False))


@cli.command()
def version() -> None:
    """显示版本信息"""
    click.echo(f"Collei Agent v{__version__}")
    click.echo(f"Python {sys.version}")


# ---------------------------------------------------------------------------
# 入口
# ---------------------------------------------------------------------------

if __name__ == "__main__":
    cli()
