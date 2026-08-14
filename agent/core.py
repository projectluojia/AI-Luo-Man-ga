from __future__ import annotations

import asyncio
import json
import re
from dataclasses import dataclass
from typing import Any, AsyncIterator

from agent.model import CapabilityExecutor, ModelProvider, ProviderFailure, TextDelta, ToolCall, TurnCompleted
from agent.observe import get_logger


logger = get_logger("agent_kernel")


@dataclass(frozen=True)
class Capability:
    id: str
    name: str
    description: str
    input_schema: dict[str, Any]

    @property
    def model_name(self) -> str:
        return "cap_" + re.sub(r"[^a-zA-Z0-9_-]", "_", self.id)

    def as_model_tool(self) -> dict[str, Any]:
        return {
            "type": "function",
            "function": {
                "name": self.model_name,
                "description": self.description,
                "parameters": self.input_schema,
                "strict": True,
            },
        }


@dataclass(frozen=True)
class ReplyDelta:
    text: str


@dataclass(frozen=True)
class CapabilityRequested:
    call: ToolCall
    capability_id: str


@dataclass(frozen=True)
class FinalReply:
    text: str


@dataclass(frozen=True)
class UsageReported:
    input_tokens: int
    output_tokens: int
    total_tokens: int
    cost_microusd: int
    provider_retries: int


class BudgetExceeded(Exception):
    pass


AgentEvent = ReplyDelta | CapabilityRequested | FinalReply | UsageReported


class AgentKernel:
    def __init__(self, provider: ModelProvider) -> None:
        self._provider = provider

    async def run(
        self,
        *,
        model: str,
        system_prompt: str,
        input_message: str,
        capabilities: list[Capability],
        execute: CapabilityExecutor,
        max_steps: int,
        max_tool_calls: int = 8,
        max_input_tokens: int = 32768,
        max_output_tokens: int = 8192,
        max_total_tokens: int = 40960,
        max_output_bytes: int = 65536,
        max_cost_microusd: int = 0,
        provider_timeout_seconds: float = 30.0,
    ) -> AsyncIterator[AgentEvent]:
        if not model:
            raise ValueError("RunInput.model is required")
        if max_steps < 1:
            raise ValueError("max_steps must be positive")
        if (
            max_tool_calls < 1
            or max_input_tokens < 1
            or max_output_tokens < 1
            or max_total_tokens < 1
            or max_output_bytes < 1
            or provider_timeout_seconds <= 0
        ):
            raise ValueError("Agent budgets must be positive")

        tools = [capability.as_model_tool() for capability in capabilities]
        projected = {capability.model_name: capability for capability in capabilities}
        if len(projected) != len(capabilities):
            raise ValueError("Capability 投影后的模型名称发生冲突")

        messages: list[dict[str, Any]] = [
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": input_message},
        ]
        seen_call_ids: set[str] = set()
        tool_call_count = 0
        output_bytes = 0
        cumulative_input_tokens = 0
        cumulative_output_tokens = 0
        cumulative_total_tokens = 0
        cumulative_cost_microusd = 0
        cumulative_provider_retries = 0
        for step in range(1, max_steps + 1):
            turn_output_bytes = 0
            logger.info(
                "开始执行模型推理步骤",
                model_step=step,
                max_steps=max_steps,
                message_count=len(messages),
                capability_count=len(capabilities),
            )
            completed: TurnCompleted | None = None
            try:
                async with asyncio.timeout(provider_timeout_seconds):
                    async for model_event in self._provider.stream_turn(
                        model=model,
                        messages=messages,
                        tools=tools,
                    ):
                        if isinstance(model_event, TextDelta):
                            chunk_bytes = len(model_event.text.encode("utf-8"))
                            turn_output_bytes += chunk_bytes
                            output_bytes += chunk_bytes
                            if output_bytes > max_output_bytes:
                                raise BudgetExceeded("Agent output byte budget exceeded")
                            yield ReplyDelta(model_event.text)
                        elif isinstance(model_event, TurnCompleted):
                            completed = model_event
            except TimeoutError as exc:
                raise ProviderFailure("provider_timeout", True) from exc

            if completed is None:
                raise RuntimeError("模型流结束时没有完成事件")
            if turn_output_bytes == 0 and completed.text:
                output_bytes += len(completed.text.encode("utf-8"))
                if output_bytes > max_output_bytes:
                    raise BudgetExceeded("Agent output byte budget exceeded")
            if completed.usage is None:
                raise ProviderFailure("provider_protocol_error", False)
            usage = completed.usage
            if (
                usage.input_tokens < 0
                or usage.output_tokens < 0
                or usage.total_tokens != usage.input_tokens + usage.output_tokens
                or (usage.cost_microusd is not None and usage.cost_microusd < 0)
                or usage.provider_retries < 0
                or usage.provider_retries > 5
            ):
                raise ProviderFailure("provider_protocol_error", False)
            cumulative_input_tokens += usage.input_tokens
            cumulative_output_tokens += usage.output_tokens
            cumulative_total_tokens += usage.total_tokens
            if usage.cost_microusd is not None:
                cumulative_cost_microusd += usage.cost_microusd
            cumulative_provider_retries += usage.provider_retries
            if (
                cumulative_input_tokens > max_input_tokens
                or cumulative_output_tokens > max_output_tokens
                or cumulative_total_tokens > max_total_tokens
                or (max_cost_microusd > 0 and cumulative_cost_microusd > max_cost_microusd)
            ):
                raise BudgetExceeded("Agent token or cost budget exceeded")
            if max_cost_microusd > 0 and usage.cost_microusd is None:
                raise ProviderFailure("provider_protocol_error", False)
            yield UsageReported(
                input_tokens=cumulative_input_tokens,
                output_tokens=cumulative_output_tokens,
                total_tokens=cumulative_total_tokens,
                cost_microusd=cumulative_cost_microusd,
                provider_retries=cumulative_provider_retries,
            )

            messages.append(completed.assistant_message)
            if not completed.tool_calls:
                logger.info(
                    "模型已经生成最终回复",
                    model_step=step,
                    reply_length=len(completed.text),
                )
                yield FinalReply(completed.text)
                return

            tool_call_count += len(completed.tool_calls)
            if tool_call_count > max_tool_calls:
                raise BudgetExceeded("Agent ToolCall budget exceeded")
            for call in completed.tool_calls:
                if not call.id or call.id in seen_call_ids:
                    raise ValueError("模型返回了空白或重复的 ToolCall ID")
                seen_call_ids.add(call.id)
                capability = projected.get(call.name)
                if capability is None:
                    raise PermissionError(f"模型请求了未投影的 Capability：{call.name}")
                try:
                    arguments = json.loads(call.arguments)
                except json.JSONDecodeError as exc:
                    raise ValueError(f"模型生成的 Capability 参数不是有效 JSON：{call.name}") from exc
                if not isinstance(arguments, dict):
                    raise ValueError(f"模型生成的 Capability 参数必须是 JSON 对象：{call.name}")

                yield CapabilityRequested(call=call, capability_id=capability.id)
                result = await execute(call)
                messages.append({
                    "role": "tool",
                    "tool_call_id": call.id,
                    "content": result,
                })
                logger.info(
                    "Capability 结果已经返回模型上下文",
                    model_step=step,
                    call_id=call.id,
                    capability_id=capability.id,
                    result_bytes=len(result.encode("utf-8")),
                )

        raise RuntimeError(f"Agent 达到最大推理步数：{max_steps}")
