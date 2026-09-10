"""临时邮箱提供商抽象基类。"""
from __future__ import annotations


class MailProvider:
    """临时邮箱服务统一接口（子类实现）。"""

    name: str = ""
    display_name: str = ""

    def create(self) -> str:
        """创建临时邮箱，返回邮箱地址。"""
        raise NotImplementedError

    def wait_otp(self, timeout: float = 150) -> str:
        """轮询收件箱，返回 6 位验证码。超时抛 TimeoutError。"""
        raise NotImplementedError

    def close(self) -> None:
        """释放资源（会话等）。"""
