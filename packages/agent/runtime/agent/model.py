from __future__ import annotations

from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from typing import Any, AsyncIterator, Awaitable, Callable


@dataclass(frozen=True)
class ToolCall:
    id: str
    name: str
    arguments: str


@dataclass(frozen=True)
class TextDelta:
    text: str


@dataclass(frozen=True)
class ModelUsage:
    input_tokens: int
    output_tokens: int
    total_tokens: int
    cost_microusd: int | None = None
    provider_retries: int = 0


@dataclass(frozen=True)
class TurnCompleted:
    text: str
    tool_calls: list[ToolCall] = field(default_factory=list)
    assistant_message: dict[str, Any] = field(default_factory=dict)
    usage: ModelUsage | None = None


ModelEvent = TextDelta | TurnCompleted


class ProviderFailure(Exception):
    def __init__(self, code: str, retryable: bool) -> None:
        super().__init__(code)
        self.code = code
        self.retryable = retryable


class ModelProvider(ABC):
    @abstractmethod
    def stream_turn(
        self,
        *,
        model: str,
        messages: list[dict[str, Any]],
        tools: list[dict[str, Any]],
    ) -> AsyncIterator[ModelEvent]:
        raise NotImplementedError

    async def check_readiness(self, model: str) -> bool:
        return bool(model)


CapabilityExecutor = Callable[[ToolCall], Awaitable[str]]
