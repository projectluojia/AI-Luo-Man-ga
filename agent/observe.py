from __future__ import annotations

import contextlib
import contextvars
import datetime
import json
import logging
import os
import sys
import traceback
from typing import Any, Iterator, TextIO


_context_fields: contextvars.ContextVar[dict[str, Any]] = contextvars.ContextVar("log_fields", default={})
_sensitive_markers = ("password", "passwd", "secret", "token", "api_key", "authorization", "cookie", "credential")
_private_fields = {
    "arguments",
    "body",
    "content",
    "error",
    "input_message",
    "message_body",
    "messages",
    "model_message",
    "output_message",
    "payload",
    "prompt",
    "response_body",
    "reason",
    "tool_arguments",
    "tool_result",
    "user_message",
}
_max_length = int(os.getenv("AILUO_LOG_MAX_VALUE_LENGTH", "4096"))
_environment = os.getenv("AILUO_ENVIRONMENT", "development")
_service = "ailuo-python-agent"

# 正式配置完成前禁止 Python 的 lastResort handler 输出未脱敏异常。
logging.getLogger().addHandler(logging.NullHandler())


def _is_sensitive(key: str) -> bool:
    normalized = key.lower().replace("-", "_")
    private_suffixes = ("_arguments", "_body", "_content", "_messages", "_payload", "_prompt")
    return (
        normalized in _private_fields
        or normalized.startswith("raw_")
        or normalized.endswith(private_suffixes)
        or any(marker in normalized for marker in _sensitive_markers)
    )


def _clean(key: str, value: Any) -> Any:
    normalized = key.lower().replace("-", "_")
    safe_token_counts = {
        "input_tokens",
        "output_tokens",
        "total_tokens",
        "cached_tokens",
        "reasoning_tokens",
        "max_input_tokens",
        "max_output_tokens",
        "max_total_tokens",
        "token_count",
    }
    if _is_sensitive(key) and not (
        normalized in safe_token_counts
        and isinstance(value, int)
        and not isinstance(value, bool)
        and value >= 0
    ):
        return "[已脱敏]"
    if isinstance(value, str) and len(value) > _max_length:
        return value[:_max_length] + "…[已截断]"
    if isinstance(value, dict):
        return {child_key: _clean(child_key, child) for child_key, child in value.items()}
    if isinstance(value, (list, tuple)):
        return [_clean(key, child) for child in value]
    return value


class _ChineseFormatter(logging.Formatter):
    levels = {
        logging.DEBUG: "调试",
        logging.INFO: "信息",
        logging.WARNING: "警告",
        logging.ERROR: "错误",
        logging.CRITICAL: "严重",
    }

    def __init__(self, output_format: str) -> None:
        super().__init__()
        self.output_format = output_format

    def format(self, record: logging.LogRecord) -> str:
        fields = dict(_context_fields.get())
        fields.update(getattr(record, "fields", {}))
        fields = {key: _clean(key, value) for key, value in fields.items() if value not in (None, "")}
        timestamp = datetime.datetime.fromtimestamp(record.created, datetime.timezone.utc).isoformat()
        level = self.levels.get(record.levelno, record.levelname)
        message = _clean("message", record.getMessage())
        if record.exc_info:
            exception_type = record.exc_info[0]
            fields["exception_type"] = exception_type.__name__ if exception_type else "UnknownException"
            fields["stack_frames"] = [
                f"{os.path.basename(frame.filename)}:{frame.lineno} {frame.name}"
                for frame in traceback.extract_tb(record.exc_info[2])
            ]
        if self.output_format == "json":
            return json.dumps({
                "time": timestamp,
                "level": record.levelname,
                "message": message,
                "service": _service,
                "environment": _environment,
                **fields,
            }, ensure_ascii=False, separators=(",", ":"))
        details = " ".join(f"{key}={json.dumps(value, ensure_ascii=False)}" for key, value in sorted(fields.items()))
        suffix = f" {details}" if details else ""
        return f"{timestamp} {level} [{_service}] {message}{suffix}"


class Logger:
    def __init__(self, component: str) -> None:
        self._logger = logging.getLogger(component)
        if not self._logger.handlers:
            self._logger.addHandler(logging.NullHandler())
        self._component = component

    def debug(self, message: str, **fields: Any) -> None:
        self._logger.debug(message, extra={"fields": {"component": self._component, **fields}})

    def info(self, message: str, **fields: Any) -> None:
        self._logger.info(message, extra={"fields": {"component": self._component, **fields}})

    def warning(self, message: str, **fields: Any) -> None:
        self._logger.warning(message, extra={"fields": {"component": self._component, **fields}})

    def exception(self, message: str, **fields: Any) -> None:
        self._logger.exception(message, extra={"fields": {"component": self._component, **fields}})


def configure(stream: TextIO | None = None) -> None:
    global _environment, _max_length

    _environment = os.getenv("AILUO_ENVIRONMENT", "development")
    try:
        _max_length = max(1, int(os.getenv("AILUO_LOG_MAX_VALUE_LENGTH", "4096")))
    except ValueError:
        _max_length = 4096
    level_name = os.getenv("AILUO_LOG_LEVEL", "INFO").upper()
    level = getattr(logging, level_name, logging.INFO)
    output_format = os.getenv("AILUO_LOG_FORMAT", "console").lower()
    handler = logging.StreamHandler(stream or sys.stdout)
    handler.setFormatter(_ChineseFormatter("json" if output_format == "json" else "console"))
    root = logging.getLogger()
    root.handlers.clear()
    root.addHandler(handler)
    root.setLevel(level)
    for third_party in ("openai", "httpx", "httpcore", "grpc"):
        logging.getLogger(third_party).setLevel(logging.CRITICAL + 1)


def get_logger(component: str) -> Logger:
    return Logger(component)


@contextlib.contextmanager
def bind(**fields: Any) -> Iterator[None]:
    merged = dict(_context_fields.get())
    merged.update(fields)
    token = _context_fields.set(merged)
    try:
        yield
    finally:
        try:
            _context_fields.reset(token)
        except ValueError:
            # 异步生成器可能由事件循环的独立清理任务关闭，此时令牌不属于当前上下文。
            pass
