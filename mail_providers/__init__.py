"""临时邮箱提供商（可扩展）。

选择:
  - cf-temp-mail (内联在 register.py 的 TempMail, 默认)
  - yydsmail (本包)
"""
from __future__ import annotations

from .base import MailProvider
from .yydsmail import YydsMailProvider

__all__ = ["MailProvider", "YydsMailProvider"]