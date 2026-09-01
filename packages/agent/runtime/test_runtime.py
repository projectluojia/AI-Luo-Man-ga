from __future__ import annotations

import asyncio
import json
import unittest
from typing import Any, AsyncIterator

from agent.generated import executor_pb2
from agent.model import ModelEvent, ModelProvider, ModelUsage, TextDelta, ToolCall, TurnCompleted
from agent.runtime import ExecutorRuntime, PROTOCOL_VERSION, _is_loopback_address


class RuntimeModel(ModelProvider):
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
            call = ToolCall("call-1", "cap_campus_bus_routes_list", '{"limit":10}')
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
            return
        self.assertEqual(json.loads(messages[-1]["content"])["routes"][0]["id"], "r1")
        yield TextDelta("查到了。")
        yield TurnCompleted(
            text="查到了。",
            assistant_message={"role": "assistant", "content": "查到了。"},
            usage=ModelUsage(input_tokens=20, output_tokens=3, total_tokens=23),
        )

    @staticmethod
    def assertEqual(left, right):
        assert left == right

    async def check_readiness(self, model: str) -> bool:
        return model == "test-model"


class RuntimeTest(unittest.IsolatedAsyncioTestCase):
    def test_insecure_transport_is_limited_to_loopback(self) -> None:
        self.assertTrue(_is_loopback_address("127.0.0.1:50051"))
        self.assertTrue(_is_loopback_address("[::1]:50051"))
        self.assertTrue(_is_loopback_address("unix:/tmp/ailuo-agent.sock"))
        self.assertFalse(_is_loopback_address("0.0.0.0:50051"))
        self.assertFalse(_is_loopback_address("192.0.2.10:50051"))

    async def test_health_requires_protocol_and_real_model_readiness(self) -> None:
        runtime = ExecutorRuntime(RuntimeModel(), model_name="test-model")
        healthy = await runtime.Health(executor_pb2.HealthRequest(
            accepted_protocol_versions=[PROTOCOL_VERSION],
        ), None)
        self.assertTrue(healthy.ready)
        self.assertEqual(healthy.supported_protocol_versions, [PROTOCOL_VERSION])

        mismatch = await runtime.Health(executor_pb2.HealthRequest(
            accepted_protocol_versions=["3.0"],
        ), None)
        self.assertFalse(mismatch.ready)
        self.assertEqual(mismatch.status_code, "protocol_version_mismatch")

        missing = await runtime.Health(executor_pb2.HealthRequest(), None)
        self.assertFalse(missing.ready)
        self.assertEqual(missing.status_code, "protocol_version_mismatch")

        unavailable = await ExecutorRuntime(RuntimeModel(), model_name="missing-model").Health(executor_pb2.HealthRequest(
            accepted_protocol_versions=[PROTOCOL_VERSION],
        ), None)
        self.assertFalse(unavailable.ready)
        self.assertEqual(unavailable.status_code, "provider_unavailable")

    async def test_run_reports_missing_model_as_configuration_error(self) -> None:
        runtime = ExecutorRuntime(RuntimeModel(), model_name="test-model")
        runtime._model_name = ""

        async def request_iterator():
            yield self._start_frame()

        output = runtime.Run(request_iterator(), None)
        accepted = await anext(output)
        failure = await anext(output)
        self.assertEqual(accepted.sequence, 1)
        self.assertEqual(failure.sequence, 2)
        self.assertEqual(failure.run_failure.code, "executor_configuration_unavailable")

    async def test_run_clamps_zero_usage_to_valid_execution_units(self) -> None:
        class ZeroUsageModel(ModelProvider):
            async def stream_turn(self, **kwargs):
                yield TurnCompleted(
                    text="完成",
                    assistant_message={"role": "assistant", "content": "完成"},
                    usage=ModelUsage(input_tokens=0, output_tokens=0, total_tokens=0),
                )

        async def request_iterator():
            yield self._start_frame()

        output = ExecutorRuntime(ZeroUsageModel(), model_name="test-model").Run(request_iterator(), None)
        await anext(output)
        usage = await anext(output)
        final = await anext(output)
        self.assertEqual(usage.resource_usage.execution_units, 1)
        self.assertEqual(final.final_result.payload.data.decode(), "完成")

    async def test_grpc_frames_wrap_real_model_loop(self) -> None:
        requests: asyncio.Queue[executor_pb2.ExecutorFrame] = asyncio.Queue()

        async def request_iterator():
            while True:
                yield await requests.get()

        await requests.put(executor_pb2.ExecutorFrame(
            echo_id="echo",
            run_id="run",
            sequence=1,
            start_run=executor_pb2.StartRun(
                app_id="campus-services",
                input_payload=executor_pb2.Payload(
                    content_type="text/plain; charset=utf-8", data="有哪些线路".encode()
                ),
                context_payload=executor_pb2.Payload(
                    content_type="application/ailuo.context+json",
                    data=b'{"schema_version":"ailuo.context.v1","blocks":[]}',
                ),
                max_steps=3,
                protocol_version=PROTOCOL_VERSION,
                max_capability_calls=4,
                max_execution_units=2000,
                max_output_bytes=4096,
                trace_id="11111111111111111111111111111111",
                parent_span_id="2222222222222222",
                capabilities=[executor_pb2.Capability(
                    id="campus.bus.routes.list",
                    version="1.0.0",
                    name="线路",
                    description="List routes",
                    input_schema_json='{"type":"object","properties":{"limit":{"type":"integer"}},"required":["limit"],"additionalProperties":false}',
                )],
            ),
        ))
        output = ExecutorRuntime(RuntimeModel(), model_name="test-model").Run(request_iterator(), None)
        accepted = await anext(output)
        self.assertEqual(accepted.sequence, 1)
        self.assertEqual(accepted.run_accepted.protocol_version, PROTOCOL_VERSION)
        first_usage = await anext(output)
        self.assertEqual(first_usage.sequence, 2)
        self.assertEqual(first_usage.resource_usage.execution_units, 12)
        call_frame = await anext(output)
        self.assertEqual(call_frame.sequence, 3)
        self.assertEqual(call_frame.capability_call.capability_id, "campus.bus.routes.list")
        await requests.put(executor_pb2.ExecutorFrame(
            echo_id="echo",
            run_id="run",
            sequence=2,
            capability_result=executor_pb2.CapabilityResult(
                call_id=call_frame.capability_call.call_id,
                capability_id="campus.bus.routes.list",
                success=True,
                payload_json=b'{"routes":[{"id":"r1"}]}',
            ),
        ))
        delta = await asyncio.wait_for(anext(output), timeout=1)
        second_usage = await asyncio.wait_for(anext(output), timeout=1)
        final = await asyncio.wait_for(anext(output), timeout=1)
        self.assertEqual(delta.sequence, 4)
        self.assertEqual(second_usage.sequence, 5)
        self.assertEqual(second_usage.resource_usage.execution_units, 35)
        self.assertEqual(final.sequence, 6)
        self.assertEqual(delta.output_delta.payload.data.decode(), "查到了。")
        self.assertEqual(final.final_result.payload.data.decode(), "查到了。")

    async def test_rejects_protocol_mismatch_before_model_execution(self) -> None:
        async def request_iterator():
            yield executor_pb2.ExecutorFrame(
                echo_id="echo",
                run_id="run",
                sequence=1,
                start_run=executor_pb2.StartRun(protocol_version="3.0"),
            )

        output = ExecutorRuntime(RuntimeModel(), model_name="test-model").Run(request_iterator(), None)
        failure = await anext(output)
        self.assertEqual(failure.sequence, 1)
        self.assertEqual(failure.run_failure.code, "protocol_version_mismatch")

    async def test_rejects_unknown_protobuf_fields(self) -> None:
        frame = self._start_frame()
        encoded = frame.SerializeToString() + b"\x98\x06\x01"
        frame_with_unknown = executor_pb2.ExecutorFrame()
        frame_with_unknown.ParseFromString(encoded)

        async def request_iterator():
            yield frame_with_unknown

        output = ExecutorRuntime(RuntimeModel(), model_name="test-model").Run(request_iterator(), None)
        failure = await anext(output)
        self.assertEqual(failure.sequence, 1)
        self.assertEqual(failure.run_failure.code, "invalid_request")

    async def test_rejects_invalid_trace_context(self) -> None:
        frame = self._start_frame()
        frame.start_run.trace_id = "secret-or-malformed"

        async def request_iterator():
            yield frame

        output = ExecutorRuntime(RuntimeModel(), model_name="test-model").Run(request_iterator(), None)
        failure = await anext(output)
        self.assertEqual(failure.sequence, 1)
        self.assertEqual(failure.run_failure.code, "invalid_request")

    async def test_accepts_governed_causal_parent_identity(self) -> None:
        frame = self._start_frame()
        frame.start_run.parent_run_id = "parent-run"

        async def request_iterator():
            yield frame

        output = ExecutorRuntime(RuntimeModel(), model_name="test-model").Run(request_iterator(), None)
        accepted = await anext(output)
        self.assertEqual(accepted.run_accepted.protocol_version, PROTOCOL_VERSION)
        await output.aclose()

    async def test_rejects_self_parent_and_malformed_parent_identity(self) -> None:
        for parent_run_id in ("run", "invalid parent"):
            with self.subTest(parent_run_id=parent_run_id):
                frame = self._start_frame()
                frame.start_run.parent_run_id = parent_run_id

                async def request_iterator():
                    yield frame

                output = ExecutorRuntime(RuntimeModel(), model_name="test-model").Run(request_iterator(), None)
                failure = await anext(output)
                self.assertEqual(failure.run_failure.code, "invalid_request")

    async def test_rejects_result_sequence_gap(self) -> None:
        requests: asyncio.Queue[executor_pb2.ExecutorFrame] = asyncio.Queue()

        async def request_iterator():
            while True:
                yield await requests.get()

        await requests.put(self._start_frame())
        output = ExecutorRuntime(RuntimeModel(), model_name="test-model").Run(request_iterator(), None)
        await anext(output)
        await anext(output)
        call = await anext(output)
        await requests.put(executor_pb2.ExecutorFrame(
            echo_id="echo",
            run_id="run",
            sequence=3,
            capability_result=executor_pb2.CapabilityResult(
                call_id=call.capability_call.call_id,
                capability_id=call.capability_call.capability_id,
                success=True,
                payload_json=b'{"routes":[]}',
            ),
        ))
        failure = await anext(output)
        self.assertEqual(failure.sequence, 4)
        self.assertEqual(failure.run_failure.code, "protocol_violation")

    @staticmethod
    def _start_frame():
        return executor_pb2.ExecutorFrame(
            echo_id="echo",
            run_id="run",
            sequence=1,
            start_run=executor_pb2.StartRun(
                app_id="campus-services",
                input_payload=executor_pb2.Payload(
                    content_type="text/plain; charset=utf-8", data="有哪些线路".encode()
                ),
                context_payload=executor_pb2.Payload(
                    content_type="application/ailuo.context+json",
                    data=b'{"schema_version":"ailuo.context.v1","blocks":[]}',
                ),
                max_steps=3,
                protocol_version=PROTOCOL_VERSION,
                max_capability_calls=4,
                max_execution_units=2000,
                max_output_bytes=4096,
                trace_id="11111111111111111111111111111111",
                parent_span_id="2222222222222222",
                capabilities=[executor_pb2.Capability(
                    id="campus.bus.routes.list",
                    version="1.0.0",
                    name="线路",
                    description="List routes",
                    input_schema_json='{"type":"object","properties":{"limit":{"type":"integer"}},"required":["limit"],"additionalProperties":false}',
                )],
            ),
        )


if __name__ == "__main__":
    unittest.main()
