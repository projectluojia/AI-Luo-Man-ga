from __future__ import annotations

import io
import contextlib
import json
import logging
import os
import unittest
from unittest.mock import patch

from agent.observe import configure, get_logger


class RedactionTest(unittest.TestCase):
    def test_free_text_credentials_and_opaque_values(self) -> None:
        values = [
            "请求失败 password=synthetic-private-value",
            "请求失败password=synthetic-private-value",
            '响应 {"api_key": "synthetic-private-value"}',
            "Authorization: Bearer synthetic-private-value",
            "Cookie: sid=synthetic-private-value",
            "https://user:synthetic-private-value@example.invalid/path",
            "-----BEGIN PRIVATE KEY-----\nsynthetic-private-value",
            "Bearer " + "x" * 100,
            "Basic " + "x" * 40,
            "ghp_" + "x" * 36,
            "github_pat_" + "x" * 32,
            "sk-proj-" + "x" * 32,
            "-----BEGIN RSA PRIVATE KEY-----\nsynthetic-private-value",
        ]
        for output_format in ("console", "json"):
            for value in values:
                with self.subTest(format=output_format, value=value):
                    output = io.StringIO()
                    with patch.dict(os.environ, {
                        "AILUO_LOG_FORMAT": output_format,
                        "AILUO_LOG_MAX_VALUE_LENGTH": "32",
                    }):
                        configure(output)
                        get_logger("test").info(
                            value, detail=value, nested={"note": [value]},
                            request_id="request-1", error_code="unavailable", input_tokens=12,
                        )
                    text = output.getvalue()
                    self.assertNotIn("synthetic-private", text)
                    self.assertNotIn("xxxx", text)
                    self.assertIn("request-1", text)
                    self.assertIn("unavailable", text)
                    if output_format == "json":
                        self.assertEqual(json.loads(text)["input_tokens"], 12)

    def test_exception_and_bytes_fields_do_not_serialize(self) -> None:
        for output_format in ("console", "json"):
            output = io.StringIO()
            with patch.dict(os.environ, {"AILUO_LOG_FORMAT": output_format}):
                configure(output)
                get_logger("test").info("调用失败", detail=ValueError("synthetic-private-content"),
                                        data=b"synthetic-private-content")
            self.assertNotIn("synthetic-private-content", output.getvalue())
            self.assertEqual(output.getvalue().count("[已脱敏]"), 2)

    def tearDown(self) -> None:
        logging.getLogger().handlers.clear()

    def test_format_failure_never_echoes_raw_record(self) -> None:
        output, errors = io.StringIO(), io.StringIO()
        configure(output)
        with contextlib.redirect_stderr(errors):
            logging.getLogger("test").error("synthetic-private-content %d", "invalid-number")
        self.assertNotIn("synthetic-private-content", errors.getvalue())
        self.assertNotIn("invalid-number", errors.getvalue())
        self.assertIn("记录已丢弃", errors.getvalue())

    def test_cycles_are_bounded_and_safe_fields_survive(self) -> None:
        output = io.StringIO()
        cycle: dict = {"password": "synthetic-private-content"}
        cycle["child"] = cycle
        with patch.dict(os.environ, {"AILUO_LOG_FORMAT": "json"}):
            configure(output)
            get_logger("test").info("完成", nested=cycle, error_code="cancelled", input_tokens=12)
        data = json.loads(output.getvalue())
        self.assertEqual(data["error_code"], "cancelled")
        self.assertEqual(data["input_tokens"], 12)
        self.assertNotIn("synthetic-private-content", output.getvalue())


if __name__ == "__main__":
    unittest.main()
