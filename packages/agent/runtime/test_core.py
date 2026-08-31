from __future__ import annotations

import asyncio
import json
import unittest
from typing import Any, AsyncIterator

from agent.core import AgentKernel, BudgetExceeded, Capability, CapabilityRequested, FinalReply, UsageReported
from agent.model import ModelEvent, ModelProvider, ModelUsage, ProviderFailure, TextDelta, ToolCall, TurnCompleted


class FakeModel(ModelProvider):
    def __init__(self) -> None:
        self.turn = 0

    async def stream_turn(
        self,
        *,
        model: str,
        messages: list[dict[str, Any]],
        tools: list[dict[str, Any]],
    ) -> AsyncIterator[ModelEvent]:
        self.turn += 1
        if self.turn == 1:
            call = ToolCall("call-1", "cap_campus_bus_routes_list", "{}")
            yield TurnCompleted(
                text="",
                tool_calls=[call],
                assistant_message={
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [{
                        "id": call.id,
                        "type": "function",
                        "function": {"name": call.name, "arguments": call.arguments},
                    }],
                },
                usage=ModelUsage(input_tokens=10, output_tokens=2, total_tokens=12),
            )
            return
        self.assert_tool_result(messages)
        yield TextDelta("共有一条线路。")
        yield TurnCompleted(
            text="共有一条线路。",
            assistant_message={"role": "assistant", "content": "共有一条线路。"},
            usage=ModelUsage(input_tokens=20, output_tokens=4, total_tokens=24),
        )

    @staticmethod
    def assert_tool_result(messages: list[dict[str, Any]]) -> None:
        assert messages[-1]["role"] == "tool"
        assert json.loads(messages[-1]["content"])["routes"][0]["id"] == "route-1"


class AgentKernelTest(unittest.IsolatedAsyncioTestCase):
    @staticmethod
    def capability() -> Capability:
        return Capability(
            id="campus.bus.routes.list",
            name="列出校巴线路",
            description="List routes",
            input_schema={"type": "object", "properties": {}, "additionalProperties": False},
        )

    def test_model_tool_normalizes_optional_fields_for_strict_provider_schema(self) -> None:
        capability = Capability(
            id="test.optional",
            name="可选字段",
            description="Optional fields",
            input_schema={
                "type": "object",
                "properties": {"name": {"type": "string"}, "count": {"type": "integer"}},
                "required": ["name"],
                "additionalProperties": False,
            },
        )
        parameters = capability.as_model_tool()["function"]["parameters"]
        self.assertEqual(parameters["required"], ["name", "count"])
        self.assertEqual(parameters["properties"]["count"], {
            "anyOf": [{"type": "integer"}, {"type": "null"}],
        })

    @staticmethod
    async def execute(call: ToolCall) -> str:
        return "{}"

    async def test_model_driven_tool_loop(self) -> None:
        calls: list[ToolCall] = []

        async def execute(call: ToolCall) -> str:
            calls.append(call)
            return json.dumps({"routes": [{"id": "route-1"}]})

        capability = self.capability()
        events = []
        async for event in AgentKernel(FakeModel()).run(
            model="test-model",
            system_prompt="test",
            input_message="有哪些线路",
            capabilities=[capability],
            execute=execute,
            max_steps=3,
        ):
            events.append(event)

        self.assertEqual([call.name for call in calls], [capability.model_name])
        self.assertTrue(any(isinstance(event, CapabilityRequested) for event in events))
        self.assertEqual([event.text for event in events if isinstance(event, FinalReply)], ["共有一条线路。"])

    async def test_rejects_unprojected_capability(self) -> None:
        class BadModel(FakeModel):
            async def stream_turn(self, **kwargs):
                call = ToolCall("bad", "private.tool", "{}")
                yield TurnCompleted(
                    text="",
                    tool_calls=[call],
                    assistant_message={"role": "assistant"},
                    usage=ModelUsage(input_tokens=1, output_tokens=1, total_tokens=2),
                )

        async def execute(call: ToolCall) -> str:
            return "{}"

        with self.assertRaises(PermissionError):
            async for _ in AgentKernel(BadModel()).run(
                model="test",
                system_prompt="test",
                input_message="test",
                capabilities=[],
                execute=execute,
                max_steps=1,
            ):
                pass

    async def test_reports_cumulative_usage_and_enforces_token_budget(self) -> None:
        async def execute(call: ToolCall) -> str:
            return json.dumps({"routes": [{"id": "route-1"}]})

        events = []
        async for event in AgentKernel(FakeModel()).run(
            model="test",
            system_prompt="test",
            input_message="test",
            capabilities=[self.capability()],
            execute=execute,
            max_steps=3,
        ):
            events.append(event)
        usage = [event for event in events if isinstance(event, UsageReported)]
        self.assertEqual([(item.input_tokens, item.output_tokens, item.total_tokens) for item in usage], [
            (10, 2, 12),
            (30, 6, 36),
        ])

        with self.assertRaises(BudgetExceeded):
            async for _ in AgentKernel(FakeModel()).run(
                model="test",
                system_prompt="test",
                input_message="test",
                capabilities=[self.capability()],
                execute=execute,
                max_steps=3,
                max_total_tokens=20,
            ):
                pass

    async def test_requires_provider_usage(self) -> None:
        class MissingUsage(ModelProvider):
            async def stream_turn(self, **kwargs):
                yield TurnCompleted(text="完成", assistant_message={"role": "assistant", "content": "完成"})

        with self.assertRaises(ProviderFailure) as captured:
            async for _ in AgentKernel(MissingUsage()).run(
                model="test",
                system_prompt="test",
                input_message="test",
                capabilities=[],
                execute=self.execute,
                max_steps=1,
            ):
                pass
        self.assertEqual(captured.exception.code, "provider_protocol_error")

    async def test_enforces_tool_call_and_output_byte_budgets(self) -> None:
        capability = self.capability()

        class TooManyCalls(ModelProvider):
            async def stream_turn(self, **kwargs):
                calls = [
                    ToolCall("one", capability.model_name, "{}"),
                    ToolCall("two", capability.model_name, "{}"),
                ]
                yield TurnCompleted(
                    text="",
                    tool_calls=calls,
                    assistant_message={"role": "assistant"},
                    usage=ModelUsage(input_tokens=1, output_tokens=1, total_tokens=2),
                )

        with self.assertRaises(BudgetExceeded):
            async for _ in AgentKernel(TooManyCalls()).run(
                model="test",
                system_prompt="test",
                input_message="test",
                capabilities=[capability],
                execute=self.execute,
                max_steps=1,
                max_tool_calls=1,
            ):
                pass

        class TooMuchOutput(ModelProvider):
            async def stream_turn(self, **kwargs):
                yield TextDelta("超长")
                yield TurnCompleted(
                    text="超长",
                    assistant_message={"role": "assistant", "content": "超长"},
                    usage=ModelUsage(input_tokens=1, output_tokens=1, total_tokens=2),
                )

        with self.assertRaises(BudgetExceeded):
            async for _ in AgentKernel(TooMuchOutput()).run(
                model="test",
                system_prompt="test",
                input_message="test",
                capabilities=[],
                execute=self.execute,
                max_steps=1,
                max_output_bytes=2,
            ):
                pass

    async def test_provider_timeout_and_cancellation_propagation(self) -> None:
        started = asyncio.Event()

        class BlockingModel(ModelProvider):
            async def stream_turn(self, **kwargs):
                started.set()
                await asyncio.Event().wait()
                if False:
                    yield TextDelta("")

        with self.assertRaises(ProviderFailure) as captured:
            async for _ in AgentKernel(BlockingModel()).run(
                model="test",
                system_prompt="test",
                input_message="test",
                capabilities=[],
                execute=self.execute,
                max_steps=1,
                provider_timeout_seconds=0.01,
            ):
                pass
        self.assertEqual(captured.exception.code, "provider_timeout")
        self.assertTrue(captured.exception.retryable)

        async def consume():
            async for _ in AgentKernel(BlockingModel()).run(
                model="test",
                system_prompt="test",
                input_message="test",
                capabilities=[],
                execute=self.execute,
                max_steps=1,
                provider_timeout_seconds=10,
            ):
                pass

        task = asyncio.create_task(consume())
        await started.wait()
        task.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await task


if __name__ == "__main__":
    unittest.main()
