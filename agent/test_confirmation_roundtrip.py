from __future__ import annotations

import asyncio
import json
import unittest
from typing import Any, AsyncIterator

from agent.generated import executor_pb2
from agent.model import ModelEvent, ModelProvider, ModelUsage, TextDelta, ToolCall, TurnCompleted
from agent.runtime import ExecutorRuntime, PROTOCOL_VERSION


def _capability() -> executor_pb2.Capability:
    """
    Create the campus bus routes listing capability used by the tests.
    
    Returns:
    	executor_pb2.Capability: A capability definition with an optional integer route limit.
    """
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
    """
    Construct confirmation metadata for the campus bus routes capability.
    
    Parameters:
    	confirmation_id (str): Identifier of the confirmation request.
    	status (str): Current confirmation status.
    	target_type (str): Type of the confirmation target.
    
    Returns:
    	ConfirmationInfo: Confirmation metadata for the capability.
    """
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
    """
    Create the initial executor frame for a test run.
    
    Parameters:
    	pending (list[executor_pb2.ConfirmationInfo] | None): Confirmation requests pending at the start of the run.
    
    Returns:
    	executor_pb2.ExecutorFrame: The configured start frame.
    """
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
        """Initialize a scripted model with its first response and an optional second-turn check.
        
        Parameters:
        	first_turn: The model turn to return first.
        	check_second_turn: An optional callable used to validate the messages received on the second turn.
        """
        self._first_turn = first_turn
        self._check = check_second_turn
        self._turn = 0

    async def stream_turn(self, *, model: str, messages: list[dict[str, Any]], tools: list[dict[str, Any]]) -> AsyncIterator[ModelEvent]:
        """
        Stream scripted model events for the initial turn and a fixed final response thereafter.
        
        Parameters:
            messages (list[dict[str, Any]]): Messages supplied for validation on subsequent turns.
        
        Yields:
            ModelEvent: Events produced for the current model turn.
        """
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
    """
    Create a model-event stream containing a completed turn with one tool call.
    
    Parameters:
    	call (ToolCall): The tool call included in the completed turn.
    
    Returns:
    	AsyncIterator[ModelEvent]: A stream containing the completed tool-call event.
    """
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
    """
    Create a model event stream containing a final text response.
    
    Parameters:
    	text (str): The response text to emit.
    
    Returns:
    	AsyncIterator[ModelEvent]: Events for the text response and its completion.
    """
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
        """Return the next executor frame containing the specified body type.
        
        Parameters:
        	output (AsyncIterator[executor_pb2.ExecutorFrame]): The stream of executor frames to inspect.
        	body (str): The expected frame body field name.
        
        Returns:
        	executor_pb2.ExecutorFrame: The first frame whose body matches `body`.
        """
        while True:
            frame = await asyncio.wait_for(anext(output), timeout=1)
            if frame.WhichOneof("body") == body:
                return frame

    def _stream(self) -> tuple[asyncio.Queue, AsyncIterator[executor_pb2.ExecutorFrame]]:
        """Create a request queue and asynchronous iterator for executor frames.
        
        Returns:
            tuple[asyncio.Queue, AsyncIterator[executor_pb2.ExecutorFrame]]: The queue used to submit request frames and an iterator that yields them in order.
        """
        requests: asyncio.Queue[executor_pb2.ExecutorFrame] = asyncio.Queue()

        async def request_iterator() -> AsyncIterator[executor_pb2.ExecutorFrame]:
            """Yield executor frames as they become available from the request queue."""
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

    async def test_waiting_confirmation_does_not_suppress_capability_call(self) -> None:
        # 回归：Python 不做确认决策——waiting 投影不得抑制调用帧，判定与去重
        # 由 Go 内核权威执行；已批准确认仍随重试帧携带。
        """Verify that a waiting confirmation does not suppress the capability call and is not treated as authorization."""
        seen: dict[str, Any] = {}

        def check(messages: list[dict[str, Any]]) -> None:
            seen["payload"] = json.loads(messages[-1]["content"])

        model = ScriptedModel(lambda: _tool_call_turn(ToolCall("call-1", "cap_campus_bus_routes_list", '{"limit":10}')), check)
        requests, iterator = self._stream()
        await requests.put(_start_frame(pending=[_confirmation("conf-2", "waiting")]))
        output = ExecutorRuntime(model).Run(iterator, None)
        await anext(output)
        call = await self._next_body(output, "capability_call")
        self.assertEqual(call.capability_call.confirmation_id, "",
                         "waiting 投影不是授权，重试帧不得携带其标识")
        await requests.put(executor_pb2.ExecutorFrame(
            echo_id="echo", run_id="run", sequence=2,
            capability_result=executor_pb2.CapabilityResult(
                call_id=call.capability_call.call_id,
                capability_id="campus.bus.routes.list",
                success=False,
                error_code="confirmation_required",
                error_message="Capability 调用需要有效确认",
                confirmation=_confirmation("conf-2", "waiting"),
            ),
        ))
        await asyncio.wait_for(anext(output), timeout=1)
        final = await self._next_body(output, "final_message")
        self.assertEqual(final.final_message.text, "已处理。")
        self.assertEqual(seen["payload"]["error"]["code"], "confirmation_required")
        self.assertEqual(seen["payload"]["error"]["confirmation"]["confirmation_id"], "conf-2")

    async def test_sequences_stay_contiguous_across_confirmed_calls(self) -> None:
        # 回归：帧序连续——出站序号只为实际发送的帧递增（空洞会被内核按协议
        # 违例拒绝整个 Run）。
        model = ScriptedModel(lambda: _tool_call_turn(ToolCall("call-1", "cap_campus_bus_routes_list", '{"limit":10}')), lambda messages: None)
        requests, iterator = self._stream()
        await requests.put(_start_frame(pending=[_confirmation("conf-9", "approved")]))
        output = ExecutorRuntime(model).Run(iterator, None)
        frames = [await anext(output)]
        await requests.put(executor_pb2.ExecutorFrame(
            echo_id="echo", run_id="run", sequence=2,
            capability_result=executor_pb2.CapabilityResult(
                call_id="call-1", capability_id="campus.bus.routes.list",
                success=True, payload_json=b'{"routes":[]}',
            ),
        ))
        while True:
            try:
                frames.append(await asyncio.wait_for(anext(output), timeout=1))
            except StopAsyncIteration:
                break
        sequences = [frame.sequence for frame in frames]
        self.assertEqual(sequences, list(range(1, len(sequences) + 1)))
        self.assertEqual(frames[-1].final_message.text, "已处理。")

    async def test_confirmation_required_result_is_surfaced_to_model(self) -> None:
        """
        Verify that a confirmation-required capability result is forwarded to the model with its error code and confirmation details.
        """
        seen: dict[str, Any] = {}

        def check(messages: list[dict[str, Any]]) -> None:
            """
            Extracts the error code and confirmation details from the latest message.
            
            Parameters:
                messages: Model messages whose latest entry contains a JSON-encoded error.
            """
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
        self.assertEqual(seen["confirmation"]["target_type"], "capability")

    async def test_malformed_confirmation_projection_is_rejected(self) -> None:
        requests, request_iterator = self._stream()
        await requests.put(_start_frame(pending=[_confirmation("conf-4", "waiting", target_type="database")]))
        output = ExecutorRuntime(ScriptedModel(lambda: _final_turn("不应到达"))).Run(request_iterator, None)
        await anext(output)
        failure = await asyncio.wait_for(anext(output), timeout=1)
        self.assertEqual(failure.run_failure.code, "protocol_violation")

    async def test_confirmation_required_projection_fields_are_validated(self) -> None:
        # 缺 capability_id 或状态取闭式外值的投影按协议违例拒绝，不转发给模型。
        """
        Validates confirmation metadata in required-confirmation results and rejects malformed projections.
        
        Raises:
            AssertionError: If the executor does not report a protocol violation for invalid confirmation metadata.
        """
        for confirmation in (
            _confirmation("conf-6", "waiting").__class__(
                confirmation_id="conf-6", capability_id="", target_type="capability",
                target_id="campus.bus.routes.list", side_effect="external",
                status="waiting", expires_at="2026-08-29T10:00:00+00:00",
            ),
            _confirmation("conf-7", "revoked"),
        ):
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
                    error_code="confirmation_required",
                    error_message="Capability 调用需要有效确认",
                    confirmation=confirmation,
                ),
            ))
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
