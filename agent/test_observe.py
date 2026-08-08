from __future__ import annotations

import io
import logging
import os
import unittest
from unittest.mock import patch

from agent.observe import bind, configure, get_logger


class ObserveTest(unittest.TestCase):
    def test_chinese_output_redacts_private_fields(self) -> None:
        output = io.StringIO()
        with patch.dict(os.environ, {
            "AILUO_LOG_FORMAT": "console",
            "AILUO_LOG_LEVEL": "INFO",
            "AILUO_LOG_MAX_VALUE_LENGTH": "16",
        }):
            configure(output)
            with bind(app_id="campus-services", echo_id="echo-1"):
                get_logger("test").info(
                    "校巴查询完成",
                    api_key="不能出现",
                    tool_arguments={"query": "不能出现"},
                    error="provider-secret-body",
                    reason="/private/database/path",
                    input_tokens=12,
                    output_tokens="不能出现的令牌",
                    detail="长" * 20,
                )

        text = output.getvalue()
        self.assertIn("信息", text)
        self.assertIn("校巴查询完成", text)
        self.assertIn("app_id=\"campus-services\"", text)
        self.assertIn("[已脱敏]", text)
        self.assertIn("[已截断]", text)
        self.assertIn("input_tokens=12", text)
        self.assertNotIn("不能出现", text)
        self.assertNotIn("provider-secret-body", text)
        self.assertNotIn("/private/database/path", text)

    def test_exception_log_omits_exception_message(self) -> None:
        output = io.StringIO()
        configure(output)
        try:
            raise RuntimeError("上游响应正文不能进入日志")
        except RuntimeError:
            get_logger("test").exception("模型请求失败")

        text = output.getvalue()
        self.assertIn("exception_type=\"RuntimeError\"", text)
        self.assertIn("stack_frames=", text)
        self.assertNotIn("上游响应正文不能进入日志", text)

    def test_third_party_plaintext_logs_are_suppressed(self) -> None:
        output = io.StringIO()
        configure(output)
        logging.getLogger("httpx").critical("https://provider.example/private?token=secret")
        self.assertEqual(output.getvalue(), "")

    def tearDown(self) -> None:
        logging.getLogger().handlers.clear()


if __name__ == "__main__":
    unittest.main()
