from __future__ import annotations

import argparse
import asyncio
import ipaddress
import json
import os
import re
import secrets
import signal
import time
from typing import AsyncIterator

import grpc

from agent.core import AgentKernel, BudgetExceeded, Capability, CapabilityRequested, FinalReply, ReplyDelta, UsageReported
from agent.generated import executor_pb2, executor_pb2_grpc
from agent.model import ModelProvider, ProviderFailure, ToolCall
from agent.observe import bind, configure, get_logger
from agent.openai_compatible import OpenAICompatibleProvider


logger = get_logger("agent_runtime")

PROTOCOL_VERSION = "4.0"
MAX_GRPC_MESSAGE_BYTES = 512 << 10
MAX_FRAME_BYTES = 300 << 10
MAX_INPUT_PAYLOAD_BYTES = 16 << 10
MAX_CONTEXT_PAYLOAD_BYTES = 64 << 10
MAX_EXECUTOR_CONFIG_BYTES = 64 << 10
MAX_CAPABILITIES = 64
MAX_CAPABILITY_SCHEMA_BYTES = 64 << 10
MAX_CAPABILITY_PAYLOAD_BYTES = 64 << 10
MAX_RESULT_PAYLOAD_BYTES = 256 << 10
MAX_OUTPUT_DELTA_BYTES = 16 << 10
MAX_OUTPUT_PAYLOAD_BYTES = 256 << 10
MAX_FAILURE_MESSAGE_BYTES = 1024
MAX_IDENTIFIER_BYTES = 128
MAX_DESCRIPTION_BYTES = 4096
MAX_NAME_BYTES = 256
MAX_PROTOCOL_STEPS = 64
MAX_TOOL_CALLS = 256
TOKEN_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]*$")
CODE_PATTERN = re.compile(r"^[a-z][a-z0-9_]*$")
TRACE_PATTERN = re.compile(r"^[0-9a-f]{32}$")
SPAN_PATTERN = re.compile(r"^[0-9a-f]{16}$")


def _frame(echo_id: str, run_id: str, sequence: int, **body) -> executor_pb2.ExecutorFrame:
    return executor_pb2.ExecutorFrame(echo_id=echo_id, run_id=run_id, sequence=sequence, **body)


def _failure(echo_id: str, run_id: str, sequence: int, code: str, message: str, *, retryable: bool = False):
    safe_message = message
    if len(safe_message.encode("utf-8")) > MAX_FAILURE_MESSAGE_BYTES:
        safe_message = "执行者执行失败"
    return _frame(
        echo_id,
        run_id,
        sequence,
        run_failure=executor_pb2.RunFailure(code=code, message=safe_message, retryable=retryable),
    )


def _execution_failure_code(provider_code: str) -> str:
    return {
        "provider_timeout": "execution_timeout",
        "rate_limited": "execution_rate_limited",
        "provider_unavailable": "execution_unavailable",
        "provider_rejected": "execution_rejected",
        "provider_failure": "execution_failed",
        "provider_protocol_error": "execution_protocol_error",
    }.get(provider_code, "execution_failed")


class ProtocolViolation(Exception):
    pass


class ExecutorRuntime(executor_pb2_grpc.ExecutorRuntimeServicer):
    def __init__(self, provider: ModelProvider | None = None, model_name: str | None = None) -> None:
        self._provider = provider or OpenAICompatibleProvider.from_environment()
        self._provider_name = type(self._provider).__name__
        self._model_name = model_name or os.getenv("AILUO_MODEL_NAME", "").strip()

    async def Health(self, request, context):
        logger.debug("收到执行者健康检查", provider=self._provider_name)
        accepted = set(request.accepted_protocol_versions)
        compatible = PROTOCOL_VERSION in accepted
        provider_ready = False
        status_code = ""
        if compatible and self._model_name:
            try:
                provider_ready = await asyncio.wait_for(self._provider.check_readiness(self._model_name), timeout=3.0)
            except Exception as exc:
                logger.warning("模型 Provider 就绪检查失败", error_type=type(exc).__name__)
                status_code = "provider_unavailable"
        if not compatible:
            status_code = "protocol_version_mismatch"
        elif not self._model_name:
            status_code = "executor_configuration_unavailable"
        elif not provider_ready and not status_code:
            status_code = "provider_unavailable"
        return executor_pb2.HealthResponse(
            ready=compatible and provider_ready,
            supported_protocol_versions=[PROTOCOL_VERSION],
            status_code=status_code,
        )

    async def Run(self, request_iterator, context) -> AsyncIterator[executor_pb2.ExecutorFrame]:
        started = time.monotonic()
        try:
            first_frame = await anext(request_iterator)
        except StopAsyncIteration:
            logger.warning("Agent 双向流未提供启动帧")
            yield _failure("", "", 1, "invalid_request", "第一个 Agent 帧必须是 start_run")
            return

        try:
            self._validate_start_frame(first_frame)
        except ProtocolViolation as exc:
            logger.warning(
                "Agent 双向流启动帧无效",
                frame_type=first_frame.WhichOneof("body"),
                error_type=type(exc).__name__,
            )
            code = "protocol_version_mismatch" if first_frame.start_run.protocol_version and first_frame.start_run.protocol_version != PROTOCOL_VERSION else "invalid_request"
            yield _failure(
                first_frame.echo_id,
                first_frame.run_id,
                1,
                code,
                "Agent 协议启动帧无效",
            )
            return

        start = first_frame.start_run
        with bind(
            app_id=start.app_id,
            echo_id=first_frame.echo_id,
            run_id=first_frame.run_id,
            trace_id=start.trace_id,
            parent_span_id=start.parent_span_id,
            span_id=secrets.token_hex(8),
        ):
            span_status = "ok"
            logger.debug("追踪 Span 已开始", span_name="agent.run")
            logger.info(
                "开始执行 Agent Run",
                capability_count=len(start.capabilities),
                max_steps=start.max_steps,
                input_length=len(start.input_payload.data),
            )
            outbound_sequence = 1
            yield _frame(
                first_frame.echo_id,
                first_frame.run_id,
                outbound_sequence,
                run_accepted=executor_pb2.RunAccepted(protocol_version=PROTOCOL_VERSION),
            )
            try:
                capabilities = self._parse_capabilities(start.capabilities)
                expected_capabilities: dict[str, str] = {}
                expected_kernel_sequence = 2

                async def execute(call: ToolCall) -> str:
                    nonlocal expected_kernel_sequence
                    expected_capability_id = expected_capabilities.pop(call.id, "")
                    if not expected_capability_id:
                        raise ProtocolViolation("Capability 调用没有对应的已投影标识")
                    async for frame in request_iterator:
                        self._validate_inbound_frame(frame, first_frame, expected_kernel_sequence)
                        expected_kernel_sequence += 1
                        frame_type = frame.WhichOneof("body")
                        if frame_type == "cancel_run":
                            logger.warning(
                                "收到 Agent Run 取消请求",
                                reason_length=len(frame.cancel_run.reason),
                            )
                            raise asyncio.CancelledError(frame.cancel_run.reason)
                        if frame_type != "capability_result":
                            raise ProtocolViolation(f"等待 Capability 结果时收到无效帧：{frame_type}")
                        result = frame.capability_result
                        self._validate_capability_result(result)
                        if result.call_id != call.id:
                            raise ProtocolViolation("Capability 结果的 call_id 与当前调用不一致")
                        if result.capability_id != expected_capability_id:
                            raise ProtocolViolation("Capability 结果的 capability_id 与当前调用不一致")
                        encoded = self._model_result(result)
                        logger.info(
                            "收到 Capability 执行结果",
                            call_id=result.call_id,
                            capability_id=result.capability_id,
                            success=result.success,
                            result_bytes=len(result.payload_json),
                        )
                        return encoded
                    raise ProtocolViolation("等待 Capability 结果时输入流提前结束")

                kernel = AgentKernel(self._provider)
                if not self._model_name:
                    raise ProtocolViolation("执行者未配置自身运行所需的模型")
                agent_config = self._parse_executor_config(start.executor_config)
                input_message = _decode_text_payload(start.input_payload, "执行输入")
                context_text = _render_agent_context(start.context_payload, agent_config)
                unit_budget = start.max_execution_units
                async for event in kernel.run(
                    model=self._model_name,
                    system_prompt=context_text,
                    input_message=input_message,
                    capabilities=capabilities,
                    execute=execute,
                    max_steps=start.max_steps,
                    max_tool_calls=start.max_capability_calls,
                    max_input_tokens=unit_budget,
                    max_output_tokens=unit_budget,
                    max_total_tokens=unit_budget,
                    max_output_bytes=start.max_output_bytes,
                    max_cost_microusd=start.max_cost_microusd,
                ):
                    outbound_sequence += 1
                    if isinstance(event, CapabilityRequested):
                        if event.call.id in expected_capabilities:
                            raise ProtocolViolation("模型返回了重复的 ToolCall ID")
                        self._validate_capability_call(event.call.id, event.capability_id, event.call.arguments)
                        expected_capabilities[event.call.id] = event.capability_id
                        logger.info(
                            "向 Go 内核请求执行 Capability",
                            sequence=outbound_sequence,
                            call_id=event.call.id,
                            capability_id=event.capability_id,
                            argument_bytes=len(event.call.arguments.encode("utf-8")),
                        )
                        yield _frame(
                            first_frame.echo_id,
                            first_frame.run_id,
                            outbound_sequence,
                            capability_call=executor_pb2.CapabilityCall(
                                call_id=event.call.id,
                                capability_id=event.capability_id,
                                payload_json=event.call.arguments.encode("utf-8"),
                            ),
                        )
                    elif isinstance(event, ReplyDelta):
                        self._validate_text(event.text, 1, MAX_OUTPUT_DELTA_BYTES, "输出片段")
                        logger.debug(
                            "向 Go 内核发送模型回复片段",
                            sequence=outbound_sequence,
                            delta_length=len(event.text),
                        )
                        yield _frame(
                            first_frame.echo_id,
                            first_frame.run_id,
                            outbound_sequence,
                            output_delta=executor_pb2.OutputDelta(
                                payload=executor_pb2.Payload(
                                    content_type="text/plain; charset=utf-8",
                                    data=event.text.encode("utf-8"),
                                ),
                            ),
                        )
                    elif isinstance(event, FinalReply):
                        self._validate_text(event.text, 1, MAX_OUTPUT_PAYLOAD_BYTES, "最终结果")
                        logger.info(
                            "Agent Run 已生成最终回复",
                            sequence=outbound_sequence,
                            reply_length=len(event.text),
                            duration_ms=round((time.monotonic() - started) * 1000, 3),
                        )
                        yield _frame(
                            first_frame.echo_id,
                            first_frame.run_id,
                            outbound_sequence,
                            final_result=executor_pb2.FinalResult(
                                payload=executor_pb2.Payload(
                                    content_type="text/plain; charset=utf-8",
                                    data=event.text.encode("utf-8"),
                                ),
                            ),
                        )
                    elif isinstance(event, UsageReported):
                        yield _frame(
                            first_frame.echo_id,
                            first_frame.run_id,
                            outbound_sequence,
                            resource_usage=executor_pb2.ResourceUsage(
                                execution_units=event.total_tokens,
                                cost_microusd=event.cost_microusd,
                                retries=event.provider_retries,
                            ),
                        )
            except asyncio.CancelledError:
                span_status = "cancelled"
                logger.warning(
                    "Agent Run 已取消",
                    duration_ms=round((time.monotonic() - started) * 1000, 3),
                )
                raise
            except Exception as exc:
                span_status = "error"
                logger.exception(
                    "Agent Run 执行失败",
                    error_type=type(exc).__name__,
                    duration_ms=round((time.monotonic() - started) * 1000, 3),
                )
                outbound_sequence += 1
                retryable = False
                if isinstance(exc, ProtocolViolation):
                    code = "protocol_violation"
                elif isinstance(exc, ProviderFailure):
                    code = _execution_failure_code(exc.code)
                    retryable = exc.retryable
                elif isinstance(exc, BudgetExceeded):
                    code = "budget_exceeded"
                else:
                    code = "execution_failed"
                yield _failure(
                    first_frame.echo_id,
                    first_frame.run_id,
                    outbound_sequence,
                    code,
                    "执行者执行失败",
                    retryable=retryable,
                )
            finally:
                logger.debug(
                    "追踪 Span 已结束",
                    span_name="agent.run",
                    span_status=span_status,
                    duration_ms=round((time.monotonic() - started) * 1000, 3),
                )

    @staticmethod
    def _parse_capabilities(specifications) -> list[Capability]:
        if len(specifications) > MAX_CAPABILITIES:
            raise ProtocolViolation("Capability 数量超过协议限制")
        capabilities: list[Capability] = []
        seen: set[str] = set()
        for specification in specifications:
            if specification.id in seen:
                raise ProtocolViolation("Capability ID 重复")
            ExecutorRuntime._valid_token(specification.id)
            ExecutorRuntime._valid_token(specification.version)
            ExecutorRuntime._validate_text(specification.name, 1, MAX_NAME_BYTES, "Capability 名称")
            ExecutorRuntime._validate_text(specification.description, 1, MAX_DESCRIPTION_BYTES, "Capability 描述")
            ExecutorRuntime._validate_text(
                specification.input_schema_json,
                2,
                MAX_CAPABILITY_SCHEMA_BYTES,
                "Capability Schema",
            )
            try:
                schema = json.loads(specification.input_schema_json)
            except json.JSONDecodeError as exc:
                raise ProtocolViolation(f"Capability 输入模式不是有效 JSON：{specification.id}") from exc
            if not isinstance(schema, dict):
                raise ProtocolViolation(f"Capability 输入模式必须是 JSON 对象：{specification.id}")
            seen.add(specification.id)
            capabilities.append(Capability(
                id=specification.id,
                name=specification.name,
                description=specification.description,
                input_schema=schema,
            ))
        return capabilities

    @staticmethod
    def _parse_executor_config(payload) -> dict:
        if not payload or not payload.data:
            return {}
        if payload.content_type != "application/json":
            raise ProtocolViolation("执行者配置必须使用 application/json")
        try:
            value = json.loads(payload.data)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ProtocolViolation("执行者配置不是有效 JSON") from exc
        if not isinstance(value, dict):
            raise ProtocolViolation("执行者配置必须是 JSON 对象")
        return value

    @staticmethod
    def _validate_start_frame(frame) -> None:
        if (
            frame.ByteSize() > MAX_FRAME_BYTES
            or ExecutorRuntime._has_unknown_fields(frame)
            or frame.WhichOneof("body") != "start_run"
            or frame.sequence != 1
        ):
            raise ProtocolViolation("第一个 Agent 帧必须是有效的 sequence=1 start_run")
        ExecutorRuntime._valid_token(frame.echo_id)
        ExecutorRuntime._valid_token(frame.run_id)
        start = frame.start_run
        if start.protocol_version != PROTOCOL_VERSION:
            raise ProtocolViolation("Agent 协议版本不兼容")
        ExecutorRuntime._valid_token(start.app_id)
        ExecutorRuntime._validate_payload(start.input_payload, True, MAX_INPUT_PAYLOAD_BYTES, "执行输入")
        ExecutorRuntime._validate_payload(start.context_payload, False, MAX_CONTEXT_PAYLOAD_BYTES, "执行上下文")
        ExecutorRuntime._validate_payload(start.executor_config, False, MAX_EXECUTOR_CONFIG_BYTES, "执行者配置")
        if (
            start.max_steps < 1
            or start.max_steps > MAX_PROTOCOL_STEPS
            or start.max_capability_calls < 1
            or start.max_capability_calls > MAX_TOOL_CALLS
            or start.max_execution_units < 1
            or start.max_execution_units > 1_000_000_000
            or start.max_output_bytes < 1
            or start.max_output_bytes > MAX_OUTPUT_PAYLOAD_BYTES
            or start.max_cost_microusd > 1_000_000_000_000_000
            or (start.parent_run_id and start.parent_run_id == frame.run_id)
            or TRACE_PATTERN.fullmatch(start.trace_id) is None
            or SPAN_PATTERN.fullmatch(start.parent_span_id) is None
            or start.trace_id == "0" * 32
            or start.parent_span_id == "0" * 16
        ):
            raise ProtocolViolation("start_run 字段无效")
        if start.parent_run_id:
            ExecutorRuntime._valid_token(start.parent_run_id)

    @staticmethod
    def _validate_inbound_frame(frame, first_frame, expected_sequence: int) -> None:
        if (
            frame.ByteSize() > MAX_FRAME_BYTES
            or ExecutorRuntime._has_unknown_fields(frame)
            or frame.echo_id != first_frame.echo_id
            or frame.run_id != first_frame.run_id
            or frame.sequence != expected_sequence
            or frame.WhichOneof("body") is None
        ):
            raise ProtocolViolation("Agent 输入帧的身份、序号、类型或大小无效")
        if frame.WhichOneof("body") == "cancel_run":
            ExecutorRuntime._validate_text(frame.cancel_run.reason, 1, MAX_FAILURE_MESSAGE_BYTES, "取消原因")

    @staticmethod
    def _validate_capability_result(result) -> None:
        ExecutorRuntime._valid_token(result.call_id)
        ExecutorRuntime._valid_token(result.capability_id)
        if result.success:
            if (
                not result.payload_json
                or len(result.payload_json) > MAX_RESULT_PAYLOAD_BYTES
                or result.error_code
                or result.error_message
            ):
                raise ProtocolViolation("成功 CapabilityResult 字段无效")
            try:
                json.loads(result.payload_json)
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                raise ProtocolViolation("成功 CapabilityResult 不是有效 JSON") from exc
            return
        if result.payload_json:
            raise ProtocolViolation("失败 CapabilityResult 字段无效")
        ExecutorRuntime._valid_code(result.error_code)
        ExecutorRuntime._validate_text(result.error_message, 1, MAX_FAILURE_MESSAGE_BYTES, "Capability 错误消息")

    @staticmethod
    def _validate_capability_call(call_id: str, capability_id: str, arguments: str) -> None:
        ExecutorRuntime._valid_token(call_id)
        ExecutorRuntime._valid_token(capability_id)
        ExecutorRuntime._validate_text(arguments, 2, MAX_CAPABILITY_PAYLOAD_BYTES, "Capability 参数")

    @staticmethod
    def _valid_token(value: str) -> None:
        ExecutorRuntime._validate_text(value, 1, MAX_IDENTIFIER_BYTES, "标识")
        if TOKEN_PATTERN.fullmatch(value) is None:
            raise ProtocolViolation("标识不符合协议字符集")

    @staticmethod
    def _has_unknown_fields(message) -> bool:
        encoded = message.SerializeToString(deterministic=True)
        cleaned = type(message)()
        cleaned.CopyFrom(message)
        cleaned.DiscardUnknownFields()
        return encoded != cleaned.SerializeToString(deterministic=True)

    @staticmethod
    def _valid_code(value: str) -> None:
        ExecutorRuntime._validate_text(value, 1, 64, "错误码")
        if CODE_PATTERN.fullmatch(value) is None:
            raise ProtocolViolation("错误码不符合协议字符集")

    @staticmethod
    def _validate_text(value: str, minimum: int, maximum: int, field: str) -> None:
        size = len(value.encode("utf-8"))
        if size < minimum or size > maximum:
            raise ProtocolViolation(f"{field}长度超出协议限制")

    @staticmethod
    def _validate_payload(value, required: bool, maximum: int, field: str) -> None:
        if value is None:
            if required:
                raise ProtocolViolation(f"{field}不能为空")
            return
        if not value.data:
            if required or value.content_type:
                raise ProtocolViolation(f"{field}不能为空或 content type 无效")
            return
        if not value.content_type or len(value.content_type.encode("utf-8")) > 128:
            raise ProtocolViolation(f"{field} content type 无效")
        size = len(value.data)
        if size > maximum:
            raise ProtocolViolation(f"{field}长度超出协议限制")

    @staticmethod
    def _model_result(result) -> str:
        if result.success:
            return result.payload_json.decode("utf-8")
        return json.dumps({
            "error": {
                "code": result.error_code,
                "message": result.error_message,
            },
        }, ensure_ascii=False, separators=(",", ":"))


def _decode_text_payload(payload, field: str) -> str:
    if payload is None or not payload.data or not payload.content_type.startswith("text/plain"):
        raise ProtocolViolation(f"{field} 必须是非空 text/plain payload")
    try:
        return payload.data.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ProtocolViolation(f"{field} 不是有效 UTF-8") from exc


def _render_agent_context(payload, config: dict) -> str:
    parts: list[str] = []
    system_prompt = config.get("system_prompt", "")
    if system_prompt:
        if not isinstance(system_prompt, str):
            raise ProtocolViolation("执行者配置中的 system_prompt 必须是字符串")
        parts.append(system_prompt)

    if payload is None or not payload.data:
        return "\n\n".join(parts)
    if payload.content_type != "application/ailuo.context+json":
        raise ProtocolViolation("执行上下文必须使用 application/ailuo.context+json")
    try:
        envelope = json.loads(payload.data)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProtocolViolation("执行上下文不是有效 JSON") from exc
    if not isinstance(envelope, dict) or envelope.get("schema_version") != "ailuo.context.v1":
        raise ProtocolViolation("执行上下文版本不受支持")
    blocks = envelope.get("blocks")
    if not isinstance(blocks, list):
        raise ProtocolViolation("执行上下文 blocks 无效")
    channel = ""
    history: list[dict] = []
    for block in blocks:
        if not isinstance(block, dict) or not isinstance(block.get("type"), str):
            raise ProtocolViolation("执行上下文 block 无效")
        block_type = block["type"]
        value = block.get("payload")
        if block_type == "execution.metadata":
            if not isinstance(value, dict) or not isinstance(value.get("channel", ""), str):
                raise ProtocolViolation("执行上下文 metadata 无效")
            channel = value.get("channel", "")
        elif block_type == "conversation.history":
            if not isinstance(value, dict) or not isinstance(value.get("entries"), list):
                raise ProtocolViolation("执行上下文 history 无效")
            history = value["entries"]
        else:
            raise ProtocolViolation(f"执行上下文包含 Agent 不支持的 block：{block_type}")

    channel_prompts = config.get("channel_prompts", {})
    if channel_prompts:
        if not isinstance(channel_prompts, dict) or any(not isinstance(item, str) for item in channel_prompts.values()):
            raise ProtocolViolation("执行者配置中的 channel_prompts 无效")
        if channel_prompts.get(channel):
            parts.append(channel_prompts[channel])
    if history:
        lines = ["【历史对话】"]
        for entry in history:
            if not isinstance(entry, dict) or not isinstance(entry.get("sender"), str):
                raise ProtocolViolation("执行上下文历史条目无效")
            timestamp = entry.get("created_at", "")
            content = entry.get("content", "")
            if not isinstance(timestamp, str) or not isinstance(content, str):
                raise ProtocolViolation("执行上下文历史正文无效")
            line = f"{entry['sender']}（{timestamp}）"
            if content:
                line += "：" + content
            lines.append(line)
        parts.append("\n".join(lines))
    return "\n\n".join(parts)


async def serve(address: str) -> None:
    if not _is_loopback_address(address):
        raise RuntimeError("Agent gRPC 非回环监听必须配置认证传输")
    server = grpc.aio.server(options=[
        ("grpc.max_receive_message_length", MAX_GRPC_MESSAGE_BYTES),
        ("grpc.max_send_message_length", MAX_GRPC_MESSAGE_BYTES),
    ])
    runtime = ExecutorRuntime()
    executor_pb2_grpc.add_ExecutorRuntimeServicer_to_server(runtime, server)
    if server.add_insecure_port(address) == 0:
        raise RuntimeError(f"Agent gRPC 监听地址不可用：{address}")
    await server.start()
    logger.info("Python AI Agent gRPC 服务已经启动", address=address)
    loop = asyncio.get_running_loop()
    stop_requested = asyncio.Event()
    installed_signals = []
    for signal_number in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(signal_number, stop_requested.set)
            installed_signals.append(signal_number)
        except NotImplementedError:
            pass
    termination = asyncio.create_task(server.wait_for_termination())
    stopping = asyncio.create_task(stop_requested.wait())
    try:
        await asyncio.wait({termination, stopping}, return_when=asyncio.FIRST_COMPLETED)
    finally:
        logger.info("Python AI Agent gRPC 服务正在停止", address=address)
        await server.stop(grace=5)
        for task in (termination, stopping):
            task.cancel()
        await asyncio.gather(termination, stopping, return_exceptions=True)
        for signal_number in installed_signals:
            loop.remove_signal_handler(signal_number)


def _is_loopback_address(address: str) -> bool:
    if address.startswith("unix:"):
        return True
    try:
        host, port = address.rsplit(":", 1)
        if not port.isdigit():
            return False
        host = host.strip("[]")
        return host.lower() == "localhost" or ipaddress.ip_address(host).is_loopback
    except (ValueError, TypeError):
        return False


def main() -> None:
    parser = argparse.ArgumentParser(description="运行 AI珞（爱珞） Python AI Agent")
    parser.add_argument("--listen", default="127.0.0.1:50051", help="gRPC 监听地址")
    arguments = parser.parse_args()
    configure()
    asyncio.run(serve(arguments.listen))


if __name__ == "__main__":
    main()
