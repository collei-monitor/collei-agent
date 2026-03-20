"""
远程任务执行模块

负责接收控制面板下发的任务，异步执行并上报结果。
支持任务类型：shell、command、script、upgrade_agent。
"""

from __future__ import annotations

import json
import logging
import os
import subprocess
import tempfile
import threading
from concurrent.futures import ThreadPoolExecutor
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from collei.api_client import ColleiApiClient

logger = logging.getLogger(__name__)

# 输出最大长度限制（1MB），防止内存耗尽
MAX_OUTPUT_LENGTH = 1 * 1024 * 1024
# 中间上报缓冲区阈值
INTERMEDIATE_REPORT_THRESHOLD = 4096


class TaskExecutor:
    """
    任务执行器

    从 report 响应中接收待执行任务列表，使用线程池并发执行，
    并通过 API 客户端上报执行状态与结果。
    """

    def __init__(self, api: ColleiApiClient, max_workers: int = 4) -> None:
        self._api = api
        self._pool = ThreadPoolExecutor(max_workers=max_workers)
        self._active_tasks: set[str] = set()  # 正在执行的 execution_id 集合
        self._lock = threading.Lock()

    def handle_pending_tasks(self, tasks: list[dict] | None) -> None:
        """处理 report 响应中的 pending_tasks 列表"""
        if not tasks:
            return

        for task in tasks:
            execution_id = task.get("execution_id", "")
            if not execution_id:
                logger.warning("收到无 execution_id 的任务，跳过")
                continue

            with self._lock:
                if execution_id in self._active_tasks:
                    logger.debug("任务 %s 已在执行中，跳过", execution_id)
                    continue
                self._active_tasks.add(execution_id)

            logger.info(
                "接收任务: execution_id=%s type=%s timeout=%ss",
                execution_id, task.get("type"), task.get("timeout_sec"),
            )
            self._pool.submit(self._execute_task, task)

    def shutdown(self) -> None:
        """关闭线程池，等待所有任务完成"""
        self._pool.shutdown(wait=False)
        logger.info("任务执行器已关闭")

    # ---- 任务执行 ----

    def _execute_task(self, task: dict) -> None:
        """执行单个任务并上报结果"""
        execution_id = task["execution_id"]
        task_type = task.get("type", "")
        timeout_sec = task.get("timeout_sec", 300)

        try:
            payload_str = task.get("payload", "{}")
            payload = json.loads(payload_str)
        except (json.JSONDecodeError, TypeError) as e:
            logger.error("任务 %s payload 解析失败: %s", execution_id, e)
            self._report_status(execution_id, "failed", exit_code=-1,
                                output=f"Payload 解析失败: {e}")
            self._finish_task(execution_id)
            return

        # 上报 running 状态
        self._report_status(execution_id, "running")

        try:
            if task_type in ("shell", "command"):
                self._exec_shell(execution_id, payload, timeout_sec)
            elif task_type == "script":
                self._exec_script(execution_id, payload, timeout_sec)
            elif task_type == "upgrade_agent":
                self._exec_upgrade(execution_id, payload, timeout_sec)
            else:
                self._report_status(
                    execution_id, "failed", exit_code=-1,
                    output=f"不支持的任务类型: {task_type}",
                )
        except Exception as e:
            logger.error("任务 %s 执行异常: %s", execution_id, e, exc_info=True)
            self._report_status(
                execution_id, "failed", exit_code=-1,
                output=_truncate(f"执行异常: {e}"),
            )
        finally:
            self._finish_task(execution_id)

    def _exec_shell(self, execution_id: str, payload: dict, timeout_sec: int) -> None:
        """执行 shell / command 类型任务"""
        command = payload.get("command", "")
        if not command:
            self._report_status(execution_id, "failed", exit_code=-1,
                                output="命令为空")
            return

        try:
            process = subprocess.Popen(
                command,
                shell=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
            )
        except OSError as e:
            self._report_status(execution_id, "failed", exit_code=-1,
                                output=_truncate(f"启动进程失败: {e}"))
            return

        self._stream_output(execution_id, process, timeout_sec)

    def _exec_script(self, execution_id: str, payload: dict, timeout_sec: int) -> None:
        """执行 script 类型任务：将脚本写入临时文件并执行"""
        script_content = payload.get("script", "")
        args = payload.get("args", [])
        if not script_content:
            self._report_status(execution_id, "failed", exit_code=-1,
                                output="脚本内容为空")
            return

        # 写入临时文件
        tmp_fd, tmp_path = tempfile.mkstemp(suffix=".sh", prefix="collei_task_")
        try:
            with os.fdopen(tmp_fd, "w") as f:
                f.write(script_content)
            os.chmod(tmp_path, 0o700)

            cmd = [tmp_path] + [str(a) for a in args]
            try:
                process = subprocess.Popen(
                    cmd,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.STDOUT,
                    text=True,
                )
            except OSError as e:
                self._report_status(execution_id, "failed", exit_code=-1,
                                    output=_truncate(f"启动脚本失败: {e}"))
                return

            self._stream_output(execution_id, process, timeout_sec)
        finally:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass

    def _exec_upgrade(self, execution_id: str, payload: dict, timeout_sec: int) -> None:
        """执行 upgrade_agent 类型任务（预留）"""
        version = payload.get("version", "")
        url = payload.get("url", "")
        self._report_status(
            execution_id, "failed", exit_code=-1,
            output=f"Agent 升级功能暂未实现 (version={version}, url={url})",
        )

    # ---- 输出流式处理 ----

    def _stream_output(
        self, execution_id: str, process: subprocess.Popen, timeout_sec: int,
    ) -> None:
        """
        流式读取进程输出，定期上报中间状态，处理超时。
        多次上报的 output 在后端会追加（不覆盖）。
        """
        buffer = ""
        total_length = 0
        timed_out = False

        timer = threading.Timer(timeout_sec, lambda: process.kill())
        timer.start()

        try:
            assert process.stdout is not None
            for line in process.stdout:
                # 限制总输出长度
                if total_length >= MAX_OUTPUT_LENGTH:
                    remaining = "[... 输出已达上限，截断 ...]\n"
                    if remaining not in buffer:
                        buffer += remaining
                    # 消费但不再累积
                    continue

                buffer += line
                total_length += len(line)

                # 每积累一定量的输出就上报一次中间状态
                if len(buffer) >= INTERMEDIATE_REPORT_THRESHOLD:
                    self._report_status(execution_id, "running", output=buffer)
                    buffer = ""

        except Exception:
            # 进程被 kill 时 stdout 可能异常
            pass
        finally:
            timer.cancel()

        try:
            exit_code = process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            exit_code = -1
            timed_out = True

        if timed_out or (process.returncode is None):
            self._report_status(
                execution_id, "timeout", exit_code=-1,
                output=buffer + f"\n任务超时 ({timeout_sec}s)",
            )
        else:
            status = "success" if exit_code == 0 else "failed"
            self._report_status(
                execution_id, status, exit_code=exit_code,
                output=buffer if buffer else None,
            )

    # ---- 状态上报 ----

    def _report_status(
        self,
        execution_id: str,
        status: str,
        exit_code: int | None = None,
        output: str | None = None,
    ) -> None:
        """上报任务执行状态到控制面板"""
        try:
            self._api.report_task(
                execution_id=execution_id,
                status=status,
                exit_code=exit_code,
                output=output,
            )
            logger.debug("任务状态上报: %s → %s", execution_id[:8], status)
        except Exception as e:
            logger.warning(
                "任务状态上报失败: %s → %s, 错误: %s",
                execution_id[:8], status, e,
            )

    def _finish_task(self, execution_id: str) -> None:
        """从活跃任务集合中移除"""
        with self._lock:
            self._active_tasks.discard(execution_id)


# ---------------------------------------------------------------------------
# 工具
# ---------------------------------------------------------------------------

def _truncate(text: str, max_len: int = MAX_OUTPUT_LENGTH) -> str:
    """截断过长的文本"""
    if len(text) <= max_len:
        return text
    return text[:max_len] + "\n[... 已截断 ...]"
