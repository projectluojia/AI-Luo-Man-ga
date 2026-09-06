from __future__ import annotations

import asyncio
import dataclasses
import os
import random
import stat
import time
from collections import defaultdict, deque
from typing import Any, AsyncIterator, Awaitable, Callable

from openai import APIConnectionError, APIStatusError, APITimeoutError, AsyncOpenAI, RateLimitError

from agent.model import CapabilityCall, ModelEvent, ModelUsage, ProviderFailure, TextDelta, TurnCompleted
from agent.observe import get_logger


logger = get_logger("openai_compatible")


class OpenAICompatibleProvider:
    def __init__(
        self,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        client: Any | None = None,
        request_timeout_seconds: float = 30.0,
        readiness_timeout_seconds: float = 3.0,
        max_retries: int = 2,
        retry_base_seconds: float = 0.25,
        retry_max_seconds: float = 2.0,
        requests_per_minute: int = 60,
        max_concurrency: int = 4,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
        random_value: Callable[[], float] = random.random,
        monotonic: Callable[[], float] = time.monotonic,
    ) -> None:
        if (
            request_timeout_seconds < 0.1
            or request_timeout_seconds > 120
            or readiness_timeout_seconds < 0.1
            or readiness_timeout_seconds > 30
            or max_retries < 0
            or max_retries > 5
            or retry_base_seconds <= 0
            or retry_max_seconds < retry_base_seconds
            or retry_max_seconds > 30
            or requests_per_minute < 1
            or requests_per_minute > 10_000
            or max_concurrency < 1
            or max_concurrency > 64
        ):
            raise ValueError("模型 Provider 可靠性配置无效")
        self._request_timeout_seconds = request_timeout_seconds
        self._readiness_timeout_seconds = readiness_timeout_seconds
        self._max_retries = max_retries
        self._retry_base_seconds = retry_base_seconds
        self._retry_max_seconds = retry_max_seconds
        self._requests_per_minute = requests_per_minute
        self._sleep = sleep
        self._random_value = random_value
        self._monotonic = monotonic
        self._concurrency = asyncio.Semaphore(max_concurrency)
        self._rate_lock = asyncio.Lock()
        self._request_times: deque[float] = deque()
        if client is not None:
            self._client = client
            return
        if not api_key:
            raise ValueError("AILUO_MODEL_API_KEY_FILE is required")
        self._client = AsyncOpenAI(
            api_key=api_key,
            base_url=base_url or None,
            max_retries=0,
            timeout=request_timeout_seconds,
        )

    @classmethod
    def from_environment(cls) -> "OpenAICompatibleProvider":
        return cls(
            api_key=_model_api_key(),
            base_url=os.getenv("AILUO_MODEL_BASE_URL") or None,
            request_timeout_seconds=_float_environment("AILUO_MODEL_TIMEOUT_SECONDS", 30.0),
            readiness_timeout_seconds=_float_environment("AILUO_MODEL_READINESS_TIMEOUT_SECONDS", 3.0),
            max_retries=_int_environment("AILUO_MODEL_MAX_RETRIES", 2),
            retry_base_seconds=_float_environment("AILUO_MODEL_RETRY_BASE_SECONDS", 0.25),
            retry_max_seconds=_float_environment("AILUO_MODEL_RETRY_MAX_SECONDS", 2.0),
            requests_per_minute=_int_environment("AILUO_MODEL_REQUESTS_PER_MINUTE", 60),
            max_concurrency=_int_environment("AILUO_MODEL_MAX_CONCURRENCY", 4),
        )

    async def check_readiness(self, model: str) -> bool:
        if not model:
            return False
        try:
            async with asyncio.timeout(self._readiness_timeout_seconds):
                available = await self._client.models.list()
            return any(getattr(candidate, "id", "") == model for candidate in available.data)
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            failure = self._classify(exc)
            logger.warning(
                "模型 Provider 就绪探测失败",
                error_code=failure.code,
                retryable=failure.retryable,
            )
            return False

    async def stream_turn(
        self,
        *,
        model: str,
        messages: list[dict[str, Any]],
        capabilities: list[dict[str, Any]],
    ) -> AsyncIterator[ModelEvent]:
        for attempt in range(self._max_retries + 1):
            started = self._monotonic()
            received_chunk = False
            try:
                await self._acquire_rate_slot()
                async with self._concurrency:
                    logger.info(
                        "开始请求模型流式推理",
                        model=model,
                        attempt=attempt + 1,
                        message_count=len(messages),
                        capability_count=len(capabilities),
                    )
                    async with asyncio.timeout(self._request_timeout_seconds):
                        stream = await self._client.chat.completions.create(
                            model=model,
                            messages=messages,
                        tools=capabilities or None,
                        tool_choice="auto" if capabilities else None,
                            stream=True,
                            stream_options={"include_usage": True},
                        )
                        text_parts: list[str] = []
                        calls: dict[int, dict[str, str]] = defaultdict(lambda: {"id": "", "name": "", "arguments": ""})
                        finish_reason = None
                        usage: ModelUsage | None = None

                        async for chunk in stream:
                            received_chunk = True
                            chunk_usage = getattr(chunk, "usage", None)
                            if chunk_usage is not None:
                                usage = _usage(chunk_usage)
                            if not chunk.choices:
                                continue
                            choice = chunk.choices[0]
                            finish_reason = choice.finish_reason or finish_reason
                            delta = choice.delta
                            if delta.content:
                                text_parts.append(delta.content)
                                yield TextDelta(delta.content)
                            for partial in delta.tool_calls or []:
                                current = calls[partial.index]
                                if partial.id:
                                    current["id"] += partial.id
                                if partial.function and partial.function.name:
                                    current["name"] += partial.function.name
                                if partial.function and partial.function.arguments:
                                    current["arguments"] += partial.function.arguments

                        capability_calls = [
                            CapabilityCall(id=value["id"], name=value["name"], arguments=value["arguments"])
                            for _, value in sorted(calls.items())
                        ]
                        if finish_reason == "tool_calls" and not capability_calls:
                            raise ProviderFailure("provider_protocol_error", False)
                        if finish_reason not in ("stop", "tool_calls"):
                            raise ProviderFailure("provider_protocol_error", False)
                        if finish_reason == "stop" and capability_calls:
                            raise ProviderFailure("provider_protocol_error", False)
                        if any(not call.id or not call.name or not call.arguments for call in capability_calls):
                            raise ProviderFailure("provider_protocol_error", False)
                        if usage is None:
                            raise ProviderFailure("provider_protocol_error", False)
                        usage = dataclasses.replace(usage, provider_retries=attempt)

                        text = "".join(text_parts)
                        assistant_message: dict[str, Any] = {
                            "role": "assistant",
                            "content": text or None,
                        }
                        if capability_calls:
                            assistant_message["tool_calls"] = [
                                {
                                    "id": call.id,
                                    "type": "function",
                                    "function": {"name": call.name, "arguments": call.arguments},
                                }
                                for call in capability_calls
                            ]

                        logger.info(
                            "模型流式推理已经完成",
                            model=model,
                            finish_reason=finish_reason,
                            text_length=len(text),
                            capability_count=len(capability_calls),
                            input_tokens=usage.input_tokens if usage else 0,
                            output_tokens=usage.output_tokens if usage else 0,
                            total_tokens=usage.total_tokens if usage else 0,
                            duration_ms=round((self._monotonic() - started) * 1000, 3),
                        )
                        yield TurnCompleted(
                            text=text,
                            capability_calls=capability_calls,
                            assistant_message=assistant_message,
                            usage=usage,
                        )
                        return
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                failure = self._classify(exc)
                if failure.retryable and not received_chunk and attempt < self._max_retries:
                    delay = min(self._retry_max_seconds, self._retry_base_seconds * (2 ** attempt))
                    delay *= 0.5 + (self._random_value() * 0.5)
                    logger.warning(
                        "模型请求将在退避后重试",
                        error_code=failure.code,
                        retryable=True,
                        attempt=attempt + 1,
                        delay_ms=round(delay * 1000, 3),
                    )
                    await self._sleep(delay)
                    continue
                logger.warning(
                    "模型请求失败",
                    error_code=failure.code,
                    retryable=failure.retryable,
                    attempt=attempt + 1,
                    received_stream_data=received_chunk,
                )
                raise failure from None

    async def _acquire_rate_slot(self) -> None:
        async with self._rate_lock:
            while True:
                now = self._monotonic()
                while self._request_times and self._request_times[0] <= now - 60:
                    self._request_times.popleft()
                if len(self._request_times) < self._requests_per_minute:
                    self._request_times.append(now)
                    return
                await self._sleep(max(0.001, self._request_times[0] + 60 - now))

    @staticmethod
    def _classify(exc: Exception) -> ProviderFailure:
        if isinstance(exc, ProviderFailure):
            return exc
        if isinstance(exc, (APITimeoutError, TimeoutError)):
            return ProviderFailure("provider_timeout", True)
        if isinstance(exc, RateLimitError):
            return ProviderFailure("rate_limited", True)
        if isinstance(exc, APIConnectionError):
            return ProviderFailure("provider_unavailable", True)
        if isinstance(exc, APIStatusError):
            if exc.status_code == 429:
                return ProviderFailure("rate_limited", True)
            if exc.status_code >= 500 or exc.status_code in (408, 409):
                return ProviderFailure("provider_unavailable", True)
            return ProviderFailure("provider_rejected", False)
        return ProviderFailure("provider_failure", False)


def _usage(value: Any) -> ModelUsage:
    input_tokens = int(getattr(value, "prompt_tokens", 0) or 0)
    output_tokens = int(getattr(value, "completion_tokens", 0) or 0)
    total_tokens = int(getattr(value, "total_tokens", input_tokens + output_tokens) or 0)
    raw_cost = getattr(value, "cost_microusd", None)
    cost_microusd = int(raw_cost) if raw_cost is not None else None
    return ModelUsage(
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        total_tokens=total_tokens,
        cost_microusd=cost_microusd,
    )


def _int_environment(name: str, default: int) -> int:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} 必须是整数") from exc


def _float_environment(name: str, default: float) -> float:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    try:
        return float(raw)
    except ValueError as exc:
        raise ValueError(f"{name} 必须是数字") from exc


def _model_api_key() -> str:
    secret_file = os.getenv("AILUO_MODEL_API_KEY_FILE", "")
    if not secret_file:
        raise ValueError("AILUO_MODEL_API_KEY_FILE is required")
    if os.name != "posix":
        raise ValueError("当前平台不支持模型密钥文件属主校验，请使用受治理的密钥源")
    flags = os.O_RDONLY
    if hasattr(os, "O_CLOEXEC"):
        flags |= os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(secret_file, flags)
        try:
            metadata = os.fstat(descriptor)
            if (
                not stat.S_ISREG(metadata.st_mode)
                or metadata.st_mode & 0o077
                or metadata.st_size > 16 << 10
                or metadata.st_uid != os.geteuid()
            ):
                raise ValueError("模型密钥文件权限或大小无效")
            with os.fdopen(descriptor, "r", encoding="utf-8") as secret:
                descriptor = -1
                value = secret.read((16 << 10) + 1).strip()
        finally:
            if descriptor >= 0:
                os.close(descriptor)
    except OSError as exc:
        raise ValueError("无法安全读取模型密钥文件") from exc
    if not value or len(value.encode("utf-8")) > 16 << 10:
        raise ValueError("模型密钥文件为空或过大")
    return value
