from __future__ import annotations

import asyncio
import os
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from agent.model import ModelUsage, ProviderFailure, TextDelta, TurnCompleted
from agent.openai_compatible import OpenAICompatibleProvider, _model_api_key


class FakeStream:
    def __init__(self, chunks):
        self._chunks = chunks

    def __aiter__(self):
        self._iterator = iter(self._chunks)
        return self

    async def __anext__(self):
        try:
            value = next(self._iterator)
        except StopIteration as exc:
            raise StopAsyncIteration from exc
        if isinstance(value, BaseException):
            raise value
        return value


class FakeCompletions:
    def __init__(self, chunks):
        self._chunks = chunks
        self.request = None

    async def create(self, **kwargs):
        self.request = kwargs
        return FakeStream(self._chunks)


class ScriptedCompletions:
    def __init__(self, outcomes):
        self._outcomes = iter(outcomes)
        self.calls = 0

    async def create(self, **kwargs):
        self.calls += 1
        outcome = next(self._outcomes)
        if isinstance(outcome, BaseException):
            raise outcome
        return outcome


class FakeModels:
    def __init__(self, outcome=None):
        self.outcome = outcome
        self.models = []

    async def retrieve(self, model):
        self.models.append(model)
        if isinstance(self.outcome, BaseException):
            raise self.outcome
        return self.outcome or SimpleNamespace(id=model)


def chunk(*, content=None, tool_calls=None, finish_reason=None, usage=None):
    delta = SimpleNamespace(content=content, tool_calls=tool_calls or [])
    choices = [SimpleNamespace(delta=delta, finish_reason=finish_reason)] if content is not None or tool_calls or finish_reason is not None else []
    return SimpleNamespace(choices=choices, usage=usage)


def partial(index, *, call_id=None, name=None, arguments=None):
    function = SimpleNamespace(name=name, arguments=arguments)
    return SimpleNamespace(index=index, id=call_id, function=function)


class OpenAICompatibleProviderTest(unittest.IsolatedAsyncioTestCase):
    def test_production_secret_file_is_restricted_and_raw_secret_is_rejected(self):
        with patch.dict(os.environ, {
            "AILUO_ENVIRONMENT": "production",
            "AILUO_MODEL_API_KEY": "raw-secret",
            "OPENAI_API_KEY": "",
            "AILUO_MODEL_API_KEY_FILE": "",
        }):
            with self.assertRaises(ValueError):
                _model_api_key()

        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "model-key")
            with open(path, "w", encoding="utf-8") as secret:
                secret.write("file-secret\n")
            os.chmod(path, 0o600)
            with patch.dict(os.environ, {
                "AILUO_ENVIRONMENT": "production",
                "AILUO_MODEL_API_KEY": "",
                "OPENAI_API_KEY": "",
                "AILUO_MODEL_API_KEY_FILE": path,
            }):
                self.assertEqual(_model_api_key(), "file-secret")
                os.chmod(path, 0o644)
                with self.assertRaises(ValueError):
                    _model_api_key()
            os.chmod(path, 0o600)
            link = os.path.join(directory, "model-key-link")
            os.symlink(path, link)
            with patch.dict(os.environ, {
                "AILUO_ENVIRONMENT": "production",
                "AILUO_MODEL_API_KEY": "",
                "OPENAI_API_KEY": "",
                "AILUO_MODEL_API_KEY_FILE": link,
            }):
                with self.assertRaises(ValueError):
                    _model_api_key()

    @staticmethod
    async def collect(provider):
        return [
            event
            async for event in provider.stream_turn(
                model="test-model",
                messages=[{"role": "user", "content": "线路"}],
                tools=[],
            )
        ]

    async def test_streams_text_and_assembles_native_tool_calls(self):
        completions = FakeCompletions([
            chunk(content="我来查询。"),
            chunk(tool_calls=[partial(0, call_id="call_", name="cap_campus_", arguments='{"limit":')]),
            chunk(tool_calls=[partial(0, call_id="1", name="bus_routes_list", arguments="10}")], finish_reason="tool_calls"),
            chunk(usage=SimpleNamespace(prompt_tokens=10, completion_tokens=2, total_tokens=12)),
        ])
        client = SimpleNamespace(chat=SimpleNamespace(completions=completions))
        provider = OpenAICompatibleProvider(client=client)
        events = []
        async for event in provider.stream_turn(
            model="test-model",
            messages=[{"role": "user", "content": "线路"}],
            tools=[{"type": "function", "function": {"name": "cap_campus_bus_routes_list"}}],
        ):
            events.append(event)

        self.assertEqual([event.text for event in events if isinstance(event, TextDelta)], ["我来查询。"])
        completed = [event for event in events if isinstance(event, TurnCompleted)][0]
        self.assertEqual(completed.tool_calls[0].id, "call_1")
        self.assertEqual(completed.tool_calls[0].name, "cap_campus_bus_routes_list")
        self.assertEqual(completed.tool_calls[0].arguments, '{"limit":10}')
        self.assertEqual(completed.usage, ModelUsage(input_tokens=10, output_tokens=2, total_tokens=12))
        self.assertTrue(completions.request["stream"])
        self.assertEqual(completions.request["tool_choice"], "auto")
        self.assertEqual(completions.request["stream_options"], {"include_usage": True})

    async def test_retries_retryable_failure_before_first_stream_chunk(self):
        delays = []

        async def sleep(delay):
            delays.append(delay)

        completions = ScriptedCompletions([
            TimeoutError(),
            FakeStream([
                chunk(content="完成"),
                chunk(finish_reason="stop"),
                chunk(usage=SimpleNamespace(prompt_tokens=2, completion_tokens=1, total_tokens=3)),
            ]),
        ])
        provider = OpenAICompatibleProvider(
            client=SimpleNamespace(chat=SimpleNamespace(completions=completions)),
            max_retries=1,
            retry_base_seconds=0.2,
            retry_max_seconds=1,
            sleep=sleep,
            random_value=lambda: 0,
        )

        events = await self.collect(provider)

        self.assertEqual(completions.calls, 2)
        self.assertEqual(delays, [0.1])
        self.assertEqual(events[-1].usage.total_tokens, 3)
        self.assertEqual(events[-1].usage.provider_retries, 1)

    async def test_does_not_retry_after_partial_stream_data(self):
        completions = ScriptedCompletions([
            FakeStream([chunk(content="部分"), TimeoutError()]),
        ])
        provider = OpenAICompatibleProvider(
            client=SimpleNamespace(chat=SimpleNamespace(completions=completions)),
            max_retries=3,
        )

        with self.assertRaises(ProviderFailure) as captured:
            await self.collect(provider)

        self.assertEqual(captured.exception.code, "provider_timeout")
        self.assertTrue(captured.exception.retryable)
        self.assertEqual(completions.calls, 1)

    async def test_rejects_malformed_or_usage_free_stream(self):
        malformed = OpenAICompatibleProvider(
            client=SimpleNamespace(chat=SimpleNamespace(completions=FakeCompletions([
                chunk(tool_calls=[partial(0, call_id="call", name="capability", arguments=None)]),
                chunk(finish_reason="tool_calls"),
                chunk(usage=SimpleNamespace(prompt_tokens=1, completion_tokens=1, total_tokens=2)),
            ]))),
        )
        with self.assertRaises(ProviderFailure) as captured:
            await self.collect(malformed)
        self.assertEqual(captured.exception.code, "provider_protocol_error")
        self.assertFalse(captured.exception.retryable)

        missing_usage = OpenAICompatibleProvider(
            client=SimpleNamespace(chat=SimpleNamespace(completions=FakeCompletions([
                chunk(content="完成"),
                chunk(finish_reason="stop"),
            ]))),
        )
        with self.assertRaises(ProviderFailure) as captured:
            await self.collect(missing_usage)
        self.assertEqual(captured.exception.code, "provider_protocol_error")

    async def test_unknown_provider_failure_is_stable_and_non_retryable(self):
        completions = ScriptedCompletions([RuntimeError("raw provider body")])
        provider = OpenAICompatibleProvider(
            client=SimpleNamespace(chat=SimpleNamespace(completions=completions)),
            max_retries=3,
        )
        with self.assertRaises(ProviderFailure) as captured:
            await self.collect(provider)
        self.assertEqual(captured.exception.code, "provider_failure")
        self.assertFalse(captured.exception.retryable)
        self.assertEqual(completions.calls, 1)

    async def test_cancellation_propagates_without_retry(self):
        started = asyncio.Event()

        class BlockingCompletions:
            def __init__(self):
                self.calls = 0

            async def create(self, **kwargs):
                self.calls += 1
                started.set()
                await asyncio.Event().wait()

        completions = BlockingCompletions()
        provider = OpenAICompatibleProvider(
            client=SimpleNamespace(chat=SimpleNamespace(completions=completions)),
            max_retries=3,
        )
        task = asyncio.create_task(self.collect(provider))
        await started.wait()
        task.cancel()

        with self.assertRaises(asyncio.CancelledError):
            await task
        self.assertEqual(completions.calls, 1)

    async def test_readiness_checks_the_configured_model(self):
        models = FakeModels()
        provider = OpenAICompatibleProvider(
            client=SimpleNamespace(models=models, chat=SimpleNamespace(completions=FakeCompletions([]))),
        )
        self.assertTrue(await provider.check_readiness("test-model"))
        self.assertEqual(models.models, ["test-model"])

        unavailable = OpenAICompatibleProvider(
            client=SimpleNamespace(
                models=FakeModels(TimeoutError()),
                chat=SimpleNamespace(completions=FakeCompletions([])),
            ),
        )
        self.assertFalse(await unavailable.check_readiness("test-model"))

    async def test_rate_limit_waits_for_the_oldest_slot(self):
        clock = [0.0]
        delays = []

        async def sleep(delay):
            delays.append(delay)
            clock[0] += delay

        completions = FakeCompletions([
            chunk(content="完成"),
            chunk(finish_reason="stop"),
            chunk(usage=SimpleNamespace(prompt_tokens=2, completion_tokens=1, total_tokens=3)),
        ])
        provider = OpenAICompatibleProvider(
            client=SimpleNamespace(chat=SimpleNamespace(completions=completions)),
            requests_per_minute=1,
            sleep=sleep,
            monotonic=lambda: clock[0],
        )

        await self.collect(provider)
        await self.collect(provider)

        self.assertEqual(delays, [60.0])

    async def test_concurrency_limit_covers_stream_consumption(self):
        stream_started = asyncio.Event()
        release_stream = asyncio.Event()

        class BlockingStream(FakeStream):
            def __init__(self, chunks):
                super().__init__(chunks)
                self.blocked = False

            async def __anext__(self):
                if not self.blocked:
                    self.blocked = True
                    stream_started.set()
                    await release_stream.wait()
                return await super().__anext__()

        completions = ScriptedCompletions([
            BlockingStream([
                chunk(content="一"),
                chunk(finish_reason="stop"),
                chunk(usage=SimpleNamespace(prompt_tokens=1, completion_tokens=1, total_tokens=2)),
            ]),
            FakeStream([
                chunk(content="二"),
                chunk(finish_reason="stop"),
                chunk(usage=SimpleNamespace(prompt_tokens=1, completion_tokens=1, total_tokens=2)),
            ]),
        ])
        provider = OpenAICompatibleProvider(
            client=SimpleNamespace(chat=SimpleNamespace(completions=completions)),
            max_concurrency=1,
        )
        first = asyncio.create_task(self.collect(provider))
        await stream_started.wait()
        second = asyncio.create_task(self.collect(provider))
        await asyncio.sleep(0)
        self.assertEqual(completions.calls, 1)
        release_stream.set()
        await asyncio.gather(first, second)
        self.assertEqual(completions.calls, 2)


if __name__ == "__main__":
    unittest.main()
