from __future__ import annotations

import argparse
import asyncio
import ipaddress
import json
import re
import secrets
import signal
import time
from datetime import datetime
from typing import AsyncIterator

import grpc

from agent.core import AgentKernel, BudgetExceeded, Capability, CapabilityRequested, FinalReply, ReplyDelta, UsageReported
from agent.generated import executor_pb2, executor_pb2_grpc
from agent.model import ModelProvider, ProviderFailure, ToolCall
from agent.observe import bind, configure, get_logger
from agent.openai_compatible import OpenAICompatibleProvider


logger = get_logger("agent_runtime")

PROTOCOL_VERSION = "3.0"
MAX_GRPC_MESSAGE_BYTES = 512 << 10
MAX_FRAME_BYTES = 300 << 10
MAX_INPUT_MESSAGE_BYTES = 16 << 10
MAX_SYSTEM_PROMPT_BYTES = 32 << 10
MAX_CAPABILITIES = 64
MAX_CAPABILITY_SCHEMA_BYTES = 64 << 10
MAX_CAPABILITY_PAYLOAD_BYTES = 64 << 10
MAX_RESULT_PAYLOAD_BYTES = 256 << 10
MAX_REPLY_DELTA_BYTES = 16 << 10
MAX_FINAL_MESSAGE_BYTES = 64 << 10
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
        safe_message = "Agent 执行失败"
    return _frame(
        echo_id,
        run_id,
        sequence,
        run_failure=executor_pb2.RunFailure(code=code, message=safe_message, retryable=retryable),
    )


class ProtocolViolation(Exception):
    pass


class ExecutorRuntime(executor_pb2_grpc.ExecutorRuntimeServicer):
    def __init__(self, provider: ModelProvider | None = None) -> None:
        self._provider = provider or OpenAICompatibleProvider.from_environment()
        self._provider_name = type(self._provider).__name__

    async def Health(self, request, context):
        """
        Check protocol compatibility and model-provider readiness for the requested model.
        
        Parameters:
            request: Health-check request containing accepted protocol versions and model identifier.
        
        Returns:
            A health response indicating readiness, provider name, supported protocol versions,
            and a status code when the check cannot succeed.
        """
        logger.debug("收到 Agent 健康检查", provider=self._provider_name)
        accepted = set(request.accepted_protocol_versions)
        compatible = not accepted or PROTOCOL_VERSION in accepted
        provider_ready = False
        status_code = ""
        if compatible and request.model:
            try:
                provider_ready = await asyncio.wait_for(self._provider.check_readiness(request.model), timeout=3.0)
            except Exception as exc:
                logger.warning("模型 Provider 就绪检查失败", error_type=type(exc).__name__)
                status_code = "provider_unavailable"
        if not compatible:
            status_code = "protocol_version_mismatch"
        elif not request.model:
            status_code = "invalid_model"
        elif not provider_ready and not status_code:
            status_code = "provider_unavailable"
        return executor_pb2.HealthResponse(
            ready=compatible and provider_ready,
            provider=self._provider_name,
            supported_protocol_versions=[PROTOCOL_VERSION],
            status_code=status_code,
        )

    async def Run(self, request_iterator, context) -> AsyncIterator[executor_pb2.ExecutorFrame]:
        """
        Execute an Agent run over a bidirectional gRPC stream.
        
        Parameters:
            request_iterator: Incoming frames containing the start request, capability
                results, or cancellation.
            context: gRPC request context.
        
        Yields:
            Outbound frames reporting run acceptance, capability calls, replies, usage,
            or failures.
        
        Raises:
            asyncio.CancelledError: If the run is cancelled while awaiting input or
                execution.
        """
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
            model=start.model,
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
                input_length=len(start.input_message),
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
                approved_confirmations = self._parse_pending_confirmations(
                    start.pending_confirmations,
                )
                expected_capabilities: dict[str, str] = {}
                expected_kernel_sequence = 2

                async def execute(call: ToolCall) -> str:
                    """
                    Waits for the result of a capability call and converts it to model-readable JSON.
                    
                    Returns:
                        str: The capability result encoded as JSON.
                    
                    Raises:
                        asyncio.CancelledError: If the run is cancelled while waiting.
                        ProtocolViolation: If the inbound frames or capability result are invalid.
                    """
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
                            confirmation_required=not result.success and result.error_code == "confirmation_required",
                        )
                        return encoded
                    raise ProtocolViolation("等待 Capability 结果时输入流提前结束")

                kernel = AgentKernel(self._provider)
                async for event in kernel.run(
                    model=start.model,
                    system_prompt=start.system_prompt,
                    input_message=start.input_message,
                    capabilities=capabilities,
                    execute=execute,
                    max_steps=start.max_steps or 8,
                    max_tool_calls=start.max_tool_calls,
                    max_input_tokens=start.max_input_tokens,
                    max_output_tokens=start.max_output_tokens,
                    max_total_tokens=start.max_total_tokens,
                    max_output_bytes=start.max_output_bytes,
                    max_cost_microusd=start.max_cost_microusd,
                    provider_timeout_seconds=start.provider_timeout_ms / 1000,
                ):
                    if isinstance(event, CapabilityRequested):
                        if event.call.id in expected_capabilities:
                            raise ProtocolViolation("模型返回了重复的 ToolCall ID")
                        self._validate_model_call(event.call.id, event.capability_id, event.call.arguments)
                        expected_capabilities[event.call.id] = event.capability_id
                        # 公共确认往返：确认判定与去重由 Go 内核权威执行——本进程
                        # 恒发调用帧（已批准确认随帧携带），waiting 投影不用于本地
                        # 决策，调用失败时由内核返回 confirmation_required + 投影。
                        outbound_sequence += 1
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
                                confirmation_id=approved_confirmations.get(event.capability_id, ""),
                            ),
                        )
                    elif isinstance(event, ReplyDelta):
                        self._validate_text(event.text, 1, MAX_REPLY_DELTA_BYTES, "回复片段")
                        outbound_sequence += 1
                        logger.debug(
                            "向 Go 内核发送模型回复片段",
                            sequence=outbound_sequence,
                            delta_length=len(event.text),
                        )
                        yield _frame(
                            first_frame.echo_id,
                            first_frame.run_id,
                            outbound_sequence,
                            reply_delta=executor_pb2.ReplyDelta(text=event.text),
                        )
                    elif isinstance(event, FinalReply):
                        self._validate_text(event.text, 1, MAX_FINAL_MESSAGE_BYTES, "最终回复")
                        outbound_sequence += 1
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
                            final_message=executor_pb2.FinalMessage(text=event.text),
                        )
                    elif isinstance(event, UsageReported):
                        outbound_sequence += 1
                        yield _frame(
                            first_frame.echo_id,
                            first_frame.run_id,
                            outbound_sequence,
                            run_usage=executor_pb2.RunUsage(
                                input_tokens=event.input_tokens,
                                output_tokens=event.output_tokens,
                                total_tokens=event.total_tokens,
                                cost_microusd=event.cost_microusd,
                                provider_retries=event.provider_retries,
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
                    code = exc.code
                    retryable = exc.retryable
                elif isinstance(exc, BudgetExceeded):
                    code = "budget_exceeded"
                else:
                    code = "agent_run_failed"
                yield _failure(
                    first_frame.echo_id,
                    first_frame.run_id,
                    outbound_sequence,
                    code,
                    f"Agent 执行失败：{type(exc).__name__}",
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
        ExecutorRuntime._validate_text(start.input_message, 1, MAX_INPUT_MESSAGE_BYTES, "输入消息")
        ExecutorRuntime._validate_text(start.timezone, 1, MAX_IDENTIFIER_BYTES, "时区")
        ExecutorRuntime._validate_text(start.model, 1, MAX_IDENTIFIER_BYTES, "模型")
        ExecutorRuntime._validate_text(start.system_prompt, 1, MAX_SYSTEM_PROMPT_BYTES, "系统提示")
        if (
            start.max_steps < 1
            or start.max_steps > MAX_PROTOCOL_STEPS
            or start.max_tool_calls < 1
            or start.max_tool_calls > MAX_TOOL_CALLS
            or start.max_input_tokens < 1
            or start.max_input_tokens > 1_000_000_000
            or start.max_output_tokens < 1
            or start.max_output_tokens > 1_000_000_000
            or start.max_total_tokens < 1
            or start.max_total_tokens > 1_000_000_000
            or start.max_output_bytes < 1
            or start.max_output_bytes > MAX_FINAL_MESSAGE_BYTES
            or start.max_cost_microusd > 1_000_000_000_000_000
            or start.provider_timeout_ms < 100
            or start.provider_timeout_ms > 120_000
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
        """
        Validate an inbound frame's identity, sequence, body, size, and cancellation reason.
        
        Parameters:
            frame: The inbound protocol frame to validate.
            first_frame: The initial frame whose run and echo identifiers must match.
            expected_sequence (int): The sequence number required for the frame.
        
        Raises:
            ProtocolViolation: If the frame violates protocol constraints.
        """
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
    def _parse_pending_confirmations(specifications) -> dict[str, str]:
        """
        Validate pending confirmation projections and collect approved confirmations by capability.
        
        Parameters:
        	specifications: Confirmation projections associated with a run.
        
        Returns:
        	dict[str, str]: A mapping from capability IDs to approved confirmation IDs.
        
        Raises:
        	ProtocolViolation: If a projection exceeds protocol limits or contains invalid
        	identifiers, types, status, expiration, or duplicate confirmation IDs.
        """
        if len(specifications) > MAX_CAPABILITIES:
            raise ProtocolViolation("确认投影数量超过协议限制")
        approved: dict[str, str] = {}
        seen: set[str] = set()
        for confirmation in specifications:
            ExecutorRuntime._valid_token(confirmation.confirmation_id)
            if confirmation.confirmation_id in seen:
                raise ProtocolViolation("确认投影标识重复")
            seen.add(confirmation.confirmation_id)
            ExecutorRuntime._valid_token(confirmation.capability_id)
            if confirmation.target_type not in ("capability", "tool"):
                raise ProtocolViolation("确认投影目标类型无效")
            ExecutorRuntime._validate_text(confirmation.target_id, 1, MAX_IDENTIFIER_BYTES, "确认投影目标")
            if confirmation.side_effect not in ("write", "external"):
                raise ProtocolViolation("确认投影副作用类型无效")
            if confirmation.status not in ("waiting", "approved"):
                raise ProtocolViolation("确认投影状态无效")
            try:
                expires_at = datetime.fromisoformat(confirmation.expires_at.replace("Z", "+00:00"))
            except ValueError as exc:
                raise ProtocolViolation("确认投影有效期不是有效的 RFC3339 时间") from exc
            if expires_at.tzinfo is None:
                raise ProtocolViolation("确认投影有效期必须携带时区")
            if confirmation.status == "approved":
                approved[confirmation.capability_id] = confirmation.confirmation_id
        return approved

    @staticmethod
    def _validate_capability_result(result) -> None:
        """
        Validate a capability result and its optional confirmation projection.
        
        Parameters:
        	result: The capability result to validate.
        
        Raises:
        	ProtocolViolation: If the result fields, payload, error details, or confirmation projection are invalid.
        """
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
        # confirmation_required 携带投影时必须格式合法（内核常态）；无投影同样
        # 容忍——确认治理端口未装配或建确认失败时，这仍是可恢复的受治理结果，
        # 交由模型向用户说明。其余失败结果携带投影即协议违例，避免执行者把
        # 无关确认与调用错误绑定。
        has_confirmation = result.HasField("confirmation")
        if result.error_code == "confirmation_required":
            if has_confirmation:
                confirmation = result.confirmation
                ExecutorRuntime._valid_token(confirmation.confirmation_id)
                # 投影字段规则与 StartRun 解析一致：能力标识/目标必填、状态取闭式
                # 值，畸形投影不转发给模型（本协议版本下确认按 capability_id
                # 索引，缺标识的投影不可用）。
                ExecutorRuntime._valid_token(confirmation.capability_id)
                ExecutorRuntime._validate_text(confirmation.target_id, 1, MAX_IDENTIFIER_BYTES, "确认投影目标")
                if confirmation.status not in ("waiting", "approved"):
                    raise ProtocolViolation("确认投影状态无效")
                if confirmation.target_type not in ("capability", "tool") or confirmation.side_effect not in ("write", "external"):
                    raise ProtocolViolation("确认投影目标或副作用类型无效")
                try:
                    expires_at = datetime.fromisoformat(confirmation.expires_at.replace("Z", "+00:00"))
                except ValueError as exc:
                    raise ProtocolViolation("确认投影有效期不是有效的 RFC3339 时间") from exc
                if expires_at.tzinfo is None:
                    raise ProtocolViolation("确认投影有效期必须携带时区")
        elif has_confirmation:
            raise ProtocolViolation("非 confirmation_required 结果携带确认投影")

    @staticmethod
    def _validate_model_call(call_id: str, capability_id: str, arguments: str) -> None:
        """
        Validate a model capability call's identifiers and argument payload.
        
        Parameters:
        	call_id (str): Identifier of the model call.
        	capability_id (str): Identifier of the requested capability.
        	arguments (str): JSON argument payload for the capability.
        """
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
    def _model_result(result) -> str:
        """
        Convert a capability result into model-readable JSON.
        
        Parameters:
        	result: A capability result containing either a JSON payload or structured error details.
        
        Returns:
        	str: The decoded payload JSON for a successful result, or a JSON object containing the error and optional confirmation details.
        """
        if result.success:
            return result.payload_json.decode("utf-8")
        error = {
            "code": result.error_code,
            "message": result.error_message,
        }
        if result.HasField("confirmation"):
            confirmation = result.confirmation
            error["confirmation"] = {
                "confirmation_id": confirmation.confirmation_id,
                "capability_id": confirmation.capability_id,
                "target_type": confirmation.target_type,
                "target_id": confirmation.target_id,
                "side_effect": confirmation.side_effect,
                "status": confirmation.status,
                "expires_at": confirmation.expires_at,
            }
        return json.dumps({"error": error}, ensure_ascii=False, separators=(",", ":"))


async def serve(address: str) -> None:
    """
    Start the Agent gRPC server, wait for termination, and perform graceful shutdown.
    
    Parameters:
        address (str): Loopback or Unix-socket address on which to listen.
    
    Raises:
        RuntimeError: If the address is not loopback or the server cannot bind to it.
    """
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
