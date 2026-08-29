from __future__ import annotations

import asyncio
import json
import unittest
from typing import Any, AsyncIterator

from agent.generated import executor_pb2
from agent.model import ModelEvent, ModelProvider, ModelUsage, TextDelta, ToolCall, TurnCompleted
from agent.runtime import ExecutorRuntime, PROTOCOL_VERSION


def _capability() -> executor_pb2.Capability:
    return executor_pb2.Capability(
        id="campus.bus.routes.list",
        version="1.0.0",
        name="campus.bus.routes.list",
        description="List routes",
        input_schema_json='{"type":"object","properties":{"limit":{"type":"integer"}},"additionalProperties":false}',
    )


def _confirmation(
    confirmation_id: str,
    status: str,
    target_type: str = "capability",
) -> executor_pb2.ConfirmationInfo:
    return executor_pb2.ConfirmationInfo(
        confirmation_id=confirmation_id,
        capability_id="campus.bus.routes.list",
        target_type=target_type,
        target_id="campus.bus.routes.list",
        side_effect="external",
        status=status,
        expires_at="2026-08-29T10:00:00+00:00",
    )


def _start_frame(pending: list[executor_pb2.ConfirmationInfo] | None = None) -> executor_pb2.ExecutorFrame:
    return executor_pb2.ExecutorFrame(
        echo_id="echo",
        run_id="run",
        sequence=1,
        start_run=executor_pb2.StartRun(
            app_id="campus-services",
            input_message="有哪些线路",
            timezone="Asia/Shanghai",
            model="test-model",
            system_prompt="test",
            max_steps=3,
            protocol_version=PROTOCOL_VERSION,
            max_tool_calls=4,
            max_input_tokens=1000,
            max_output_tokens=1000,
            max_total_tokens=2000,
            max_output_bytes=4096,
            provider_timeout_ms=5000,
            trace_id="11111111111111111111111111111111",
            parent_span_id="2222222222222222",
            capabilities=[_capability()],
            pending_confirmations=pending or [],
        ),
    )


class ScriptedModel(ModelProvider):
    """可编程模型：第一轮按脚本产出事件，第二轮校验收到的结果正文。"""

    def __init__(self, first_turn, check_second_turn=None) -> None:
        self._first_turn = first_turn
        self._check = check_second_turn
        self._turn = 0

    async def stream_turn(self, *, model: str, messages: list[dict[str, Any]], tools: list[dict[str, Any]]) -> AsyncIterator[ModelEvent]:
        self._turn += 1
        if self._turn == 1:
            async for event in self._first_turn():
                yield event
            return
        if self._check is not None:
            self._check(messages)
        async for event in _final_turn("已处理。"):
            yield event


def _tool_call_turn(call: ToolCall) -> AsyncIterator[ModelEvent]:
    async def generate() -> AsyncIterator[ModelEvent]:
        yield TurnCompleted(
            text="",
            tool_calls=[call],
            assistant_message={
                "role": "assistant",
                "content": None,
                "tool_calls": [{"id": call.id, "type": "function", "function": {"name": call.name, "arguments": call.arguments}}],
            },
            usage=ModelUsage(input_tokens=10, output_tokens=2, total_tokens=12),
        )
    return generate()


def _final_turn(text: str) -> AsyncIterator[ModelEvent]:
    async def generate() -> AsyncIterator[ModelEvent]:
        yield TextDelta(text)
        yield TurnCompleted(
            text=text,
            tool_calls=[],
            assistant_message={"role": "assistant", "content": text, "tool_calls": []},
            usage=ModelUsage(input_tokens=10, output_tokens=2, total_tokens=12),
        )
    return generate()


class ConfirmationRoundTripTest(unittest.IsolatedAsyncioTestCase):
    @staticmethod
    async def _next_body(output: AsyncIterator[executor_pb2.ExecutorFrame], body: str) -> executor_pb2.ExecutorFrame:
        while True:
            frame = await asyncio.wait_for(anext(output), timeout=1)
            if frame.WhichOneof("body") == body:
                return frame

    def _stream(self) -> tuple[asyncio.Queue, AsyncIterator[executor_pb2.ExecutorFrame]]:
        requests: asyncio.Queue[executor_pb2.ExecutorFrame] = asyncio.Queue()

        async def request_iterator() -> AsyncIterator[executor_pb2.ExecutorFrame]:
            while True:
                yield await requests.get()

        return requests, request_iterator()

    async def test_approved_confirmation_is_attached_to_retry_call(self) -> None:
        seen: dict[str, Any] = {}

        def check(messages: list[dict[str, Any]]) -> None:
            seen["payload"] = json.loads(messages[-1]["content"])

        model = ScriptedModel(lambda: _tool_call_turn(ToolCall("call-1", "cap_campus_bus_routes_list", '{"limit":10}')), check)
        requests, request_iterator = self._stream()
        await requests.put(_start_frame(pending=[_confirmation("conf-1", "approved")]))
        output = ExecutorRuntime(model).Run(request_iterator, None)
        accepted = await anext(output)
        self.assertEqual(accepted.run_accepted.protocol_version, PROTOCOL_VERSION)
        call = await self._next_body(output, "capability_call")
        self.assertEqual(call.capability_call.confirmation_id, "conf-1")
        await requests.put(executor_pb2.ExecutorFrame(
            echo_id="echo", run_id="run", sequence=2,
            capability_result=executor_pb2.CapabilityResult(
                call_id=call.capability_call.call_id,
                capability_id="campus.bus.routes.list",
                success=True,
                payload_json=b'{"routes":[]}',
            ),
        ))
        await asyncio.wait_for(anext(output), timeout=1)
        final = await self._next_body(output, "final_message")
        self.assertEqual(final.final_message.text, "已处理。")
        self.assertEqual(seen["payload"]["routes"], [])

    async def test_waiting_confirmation_short_circuits_capability_call(self) -> None:
        seen: dict[str, Any] = {}

        def check(messages: list[dict[str, Any]]) -> None:
            payload = json.loads(messages[-1]["content"])
            seen["code"] = payload["error"]["code"]
            seen["confirmation_id"] = payload["error"]["confirmation_id"]

        model = ScriptedModel(lambda: _tool_call_turn(ToolCall("call-1", "cap_campus_bus_routes_list", '{"limit":10}')), check)
        requests, iterator = self._stream()
        await requests.put(_start_frame(pending=[_confirmation("conf-2", "waiting")]))
        output = ExecutorRuntime(model).Run(iterator, None)
        frames: list[executor_pb2.ExecutorFrame] = []
        while True:
            try:
                frames.append(await asyncio.wait_for(anext(output), timeout=1))
            except StopAsyncIteration:
                break
        self.assertTrue(seen["code"], "confirmation_pending")
        self.assertEqual(seen["confirmation_id"], "conf-2")
        self.assertFalse(any(frame.WhichOneof("body") == "capability_call" for frame in frames),
                         "存在等待确认时不得向内核发起 Capability 调用")
        self.assertEqual(frames[-1].final_message.text, "已处理。")

    async def test_confirmation_required_result_is_surfaced_to_model(self) -> None:
        seen: dict[str, Any] = {}

        def check(messages: list[dict[str, Any]]) -> None:
            payload = json.loads(messages[-1]["content"])
            seen["code"] = payload["error"]["code"]
            seen["confirmation"] = payload["error"]["confirmation"]

        model = ScriptedModel(lambda: _tool_call_turn(ToolCall("call-1", "cap_campus_bus_routes_list", '{"limit":10}')), check)
        requests, request_iterator = self._stream()
        await requests.put(_start_frame())
        output = ExecutorRuntime(model).Run(request_iterator, None)
        await anext(output)
        call = await self._next_body(output, "capability_call")
        self.assertEqual(call.capability_call.confirmation_id, "")
        await requests.put(executor_pb2.ExecutorFrame(
            echo_id="echo", run_id="run", sequence=2,
            capability_result=executor_pb2.CapabilityResult(
                call_id=call.capability_call.call_id,
                capability_id="campus.bus.routes.list",
                success=False,
                error_code="confirmation_required",
                error_message="Capability 调用需要有效确认",
                confirmation=_confirmation("conf-3", "waiting"),
            ),
        ))
        final = await self._next_body(output, "final_message")
        self.assertEqual(final.final_message.text, "已处理。")
        self.assertEqual(seen["code"], "confirmation_required")
        self.assertEqual(seen["confirmation"]["confirmation_id"], "conf-3")
        self.assertEqual(seen["confirmation"]["status"], "waiting")

    async def test_malformed_confirmation_projection_is_rejected(self) -> None:
        requests, request_iterator = self._stream()
        await requests.put(_start_frame(pending=[_confirmation("conf-4", "waiting", target_type="database")]))
        output = ExecutorRuntime(ScriptedModel(lambda: _final_turn("不应到达"))).Run(request_iterator, None)
        await anext(output)
        failure = await asyncio.wait_for(anext(output), timeout=1)
        self.assertEqual(failure.run_failure.code, "protocol_violation")

    async def test_non_confirmation_result_may_not_carry_confirmation(self) -> None:
        requests, request_iterator = self._stream()
        await requests.put(_start_frame())
        output = ExecutorRuntime(ScriptedModel(lambda: _tool_call_turn(ToolCall("call-1", "cap_campus_bus_routes_list", '{"limit":10}')))).Run(request_iterator, None)
        await anext(output)
        call = await self._next_body(output, "capability_call")
        await requests.put(executor_pb2.ExecutorFrame(
            echo_id="echo", run_id="run", sequence=2,
            capability_result=executor_pb2.CapabilityResult(
                call_id=call.capability_call.call_id,
                capability_id="campus.bus.routes.list",
                success=False,
                error_code="invalid_arguments",
                error_message="参数无效",
                confirmation=_confirmation("conf-5", "waiting"),
            ),
        ))
        failure = await asyncio.wait_for(anext(output), timeout=1)
        self.assertEqual(failure.run_failure.code, "protocol_violation")


if __name__ == "__main__":
    unittest.main()
