#!/usr/bin/env python3
"""
Anuma AI 账号自动注册脚本
==========================
逆向流程（2026-08-03 实测 chat.anuma.ai）:

  1. 创建临时邮箱 (cf-temp-mail: YOUR_MAIL_DOMAIN)
  2. 打开 https://chat.anuma.ai/ (camoufox 反检测浏览器, Xvfb 虚拟显示)
  3. 填入邮箱 -> 提交表单 -> 页面内 hCaptcha 隐形组件自动执行
     * 若 hCaptcha 弹出可见 challenge: 用 hcaptcha-challenger AgentV 解决
       (视觉模型 point/glm-5v-turbo, OpenAI 兼容后端 YOUR_LLM_BACKEND_URL)
     * 若隐形通过: 无需人工干预
  4. POST https://auth.privy.io/api/v1/passwordless/init
       {"email": ..., "token": "<hCaptcha token P1_...>"}
       -> 邮箱收到 6 位验证码
  5. 轮询临时邮箱取 OTP -> 填入页面 6 个 OTP 框 (键盘输入, 触发 React)
  6. 页面内 Privy SDK 自动完成:
       POST /passwordless/authenticate {email, code, mode}
       POST /sessions {refresh_token} -> 拿到 identity_token
     localStorage 出现 privy:token / privy:pat / privy:id_token / privy:refresh_token
  7. 显式创建嵌入式钱包 (Privy MPC, 老 API 仍可用):
       POST https://auth.privy.io/api/v1/wallets {"chain_type":"ethereum"}
  8. 刷新 session -> 查询余额 GET https://portal.anuma.ai/api/v1/credits/balance
     新账号默认 100 credits (basic tier)

关键坑:
  - passwordless/init 现在【必须】带 hCaptcha token:
      无 token  -> 401 invalid_credentials
      假 token  -> 401 invalid_captcha
     (参考仓库 AnumaAI 的纯 requests 流程已失效, 必须浏览器拿 token)
  - 页面 Continue 按钮有时 locator.click 会超时(被遮挡), 用 JS click 兜底
  - Cookie 弹窗 (Cookiebot) 会挡住 OTP 步骤, 进入前先 Accept/Reject
  - OTP 输入是 6 个 maxlength=1 的框, 必须用真实键盘事件 (React 组件)

输出: accounts.json (email / wallet / tokens / balance / expires_at)
用法: python register.py [--count N] [--headed] [--debug]
"""
from __future__ import annotations

import argparse
import asyncio
import base64
import json
import os
import re
import secrets
import shutil
import sys
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Optional

# ---------------------------------------------------------------------------
# 环境准备
# ---------------------------------------------------------------------------
ROOT = Path(__file__).resolve().parent
CAMOUFOX_SP = os.environ.get("CAMOUFOX_SITE_PACKAGES", "YOUR_CAMOUFOX_SITE_PACKAGES")
HC_SRC = os.environ.get("HC_CHALLENGER_SRC", "YOUR_HC_CHALLENGER_SRC")
HC_VENV_SP = os.environ.get("HC_CHALLENGER_VENV_SP", "YOUR_HC_CHALLENGER_VENV_SP")

for p in (CAMOUFOX_SP, HC_SRC, HC_VENV_SP):
    if os.path.isdir(p) and p not in sys.path:
        sys.path.insert(0, p)

# 载入 hcaptcha-challenger 的 .env (OPENAI_BASE_URL / OPENAI_MODEL / OPENAI_API_KEY)
_HC_ENV = Path(os.environ.get("HC_CHALLENGER_ENV", "YOUR_HC_CHALLENGER_ENV"))
if _HC_ENV.exists():
    for line in _HC_ENV.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            os.environ.setdefault(k.strip(), v.strip())

# 载入外部环境里的 CF_TEMP_MAIL_* (例如 cf-temp-mail skill 的 ~/.hermes/.env)
HERMES_ENV = Path(os.environ.get("HERMES_ENV_FILE", "YOUR_HERMES_ENV_FILE"))
for line in HERMES_ENV.read_text().splitlines() if HERMES_ENV.exists() else []:
    line = line.strip()
    if line and not line.startswith("#") and "=" in line:
        k, v = line.split("=", 1)
        os.environ.setdefault(k.strip(), v.strip().strip('"').strip("'"))

# 载入 ROOT/.env (ANUMA_GATEWAY_URL / ANUMA_GATEWAY_TOKEN 等网关配置, 覆盖 GATEWAY_* 默认值)
_ROOT_ENV = ROOT / ".env"
if _ROOT_ENV.exists():
    for line in _ROOT_ENV.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            os.environ.setdefault(k.strip(), v.strip().strip('"').strip("'"))

MAIL_API = os.environ.get("CF_TEMP_MAIL_API", "https://YOUR_MAIL_DOMAIN").rstrip("/")
MAIL_KEY = os.environ.get("CF_TEMP_MAIL_KEY", "")
MAIL_DOM = os.environ.get("CF_TEMP_MAIL_DOMAIN", "YOUR_MAIL_DOMAIN")

# 临时邮箱提供商选择: cftemp (默认, cf-temp-mail) | yydsmail
MAIL_PROVIDER = os.environ.get("MAIL_PROVIDER", "cftemp").strip().lower()
# YYDS Mail 配置 (MAIL_PROVIDER=yydsmail 时生效)
YYDS_MAIL_API_KEY = os.environ.get("YYDS_MAIL_API_KEY", "")
YYDS_MAIL_BASE_URL = os.environ.get("YYDS_MAIL_BASE_URL", "")
YYDS_MAIL_DOMAIN = os.environ.get("YYDS_MAIL_DOMAIN", "")
YYDS_MAIL_SUBDOMAIN = os.environ.get("YYDS_MAIL_SUBDOMAIN", "")
YYDS_MAIL_WILDCARD = os.environ.get("YYDS_MAIL_WILDCARD", "").lower() in ("1", "true", "yes", "on")
OPENAI_MODEL = os.environ.get("OPENAI_MODEL", "point/glm-5v-turbo")

PRIVY_APP_ID = "YOUR_PRIVY_APP_ID"
PRIVY_CLIENT = "react-auth:3.14.1"
PRIVY_AUTH = "https://auth.privy.io/api/v1"
PORTAL = "https://portal.anuma.ai/api/v1"
SIGNUP_URL = "https://chat.anuma.ai/"
UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")

# 网关上传配置 (SPEC-upload §3): 注册成功后秒级推送, 不再依赖 CSV 轮询
GATEWAY_URL = os.environ.get("ANUMA_GATEWAY_URL", "http://127.0.0.1:7895").rstrip("/")
GATEWAY_TOKEN = os.environ.get("ANUMA_GATEWAY_TOKEN", "YOUR_ADMIN_PASSWORD")

os.environ.setdefault("DISPLAY", ":99")


def log(msg: str):
    print(f"[{datetime.now().strftime('%H:%M:%S')}] {msg}", flush=True)


def _mask_email(email: str) -> str:
    """日志脱敏: 只保留 local-part, 隐藏域名 (foo@...)."""
    if not email:
        return ""
    return email.split("@")[0] + "@..."


def _mask_code(code: str) -> str:
    """日志脱敏: OTP 默认只显示后 2 位 (****ab), 不打印完整验证码."""
    if not code:
        return ""
    return "****" + code[-2:]


# ---------------------------------------------------------------------------
# 临时邮箱 (cf-temp-mail skill)
# ---------------------------------------------------------------------------
class TempMail:
    def __init__(self):
        if not MAIL_KEY:
            raise RuntimeError("缺少 CF_TEMP_MAIL_KEY (请配置 CF_TEMP_MAIL_KEY 环境变量)")
        self.jwt: Optional[str] = None
        self.address: str = ""

    def create(self) -> str:
        import requests
        local = "anuma" + secrets.token_hex(4)
        r = requests.post(f"{MAIL_API}/api/new_address",
                          headers={"x-custom-auth": MAIL_KEY, "Content-Type": "application/json"},
                          json={"name": local, "domain": MAIL_DOM}, timeout=30)
        r.raise_for_status()
        d = r.json()
        self.address = d["address"]
        self.jwt = d.get("jwt") or d.get("token") or ""
        if not self.jwt:
            raise RuntimeError(f"建箱失败: {d}")
        return self.address

    def _list(self):
        import requests
        r = requests.get(f"{MAIL_API}/api/mails", headers={"Authorization": f"Bearer {self.jwt}"},
                         params={"limit": 10, "offset": 0}, timeout=30)
        r.raise_for_status()
        return (r.json() or {}).get("results", [])

    def _detail(self, mid: str):
        import requests
        r = requests.get(f"{MAIL_API}/api/mail/{mid}", headers={"Authorization": f"Bearer {self.jwt}"}, timeout=30)
        r.raise_for_status()
        return r.json()

    def wait_otp(self, timeout: float = 150) -> str:
        """轮询收件箱, 从 HTML 正文里抽 6 位验证码."""
        deadline = time.time() + timeout
        seen = set()
        while time.time() < deadline:
            try:
                mails = self._list()
            except Exception:
                mails = []
            for m in mails:
                mid = str(m.get("id") or "")
                if not mid or mid in seen:
                    continue
                seen.add(mid)
                try:
                    d = self._detail(mid)
                except Exception:
                    continue
                raw = str(d.get("raw") or "")
                hm = re.search(r"<html.*?</html>", raw, re.S | re.I)
                blob = hm.group(0) if hm else raw
                codes = re.findall(r"\b(\d{6})\b", blob)
                if codes:
                    return codes[0]
            time.sleep(4)
        raise TimeoutError(f"{int(timeout)}s 内未收到验证码")


# ---------------------------------------------------------------------------
# 临时邮箱提供商工厂 (cftemp | yydsmail)
# ---------------------------------------------------------------------------
def make_mail_provider() -> TempMail:
    """按 MAIL_PROVIDER 环境变量返回临时邮箱实例（统一 create/wait_otp 接口）。"""
    if MAIL_PROVIDER == "yydsmail":
        try:
            from mail_providers.yydsmail import YydsMailProvider
        except ImportError as e:
            raise RuntimeError(f"导入 yydsmail 提供商失败: {e} (需要 pip install curl_cffi)") from e
        if not YYDS_MAIL_API_KEY:
            raise RuntimeError("MAIL_PROVIDER=yydsmail 但缺少 YYDS_MAIL_API_KEY (请配置环境变量)")
        log(f"临时邮箱提供商: YYDS Mail")
        return YydsMailProvider(
            api_key=YYDS_MAIL_API_KEY,
            base_url=YYDS_MAIL_BASE_URL,
            domain=YYDS_MAIL_DOMAIN,
            subdomain=YYDS_MAIL_SUBDOMAIN,
            wildcard=YYDS_MAIL_WILDCARD,
        )
    log("临时邮箱提供商: cf-temp-mail")
    return TempMail()


# ---------------------------------------------------------------------------
# Privy API (requests, 带浏览器里拿到的 token)
# ---------------------------------------------------------------------------
def _privy_headers(auth: Optional[str] = None, *, caid: Optional[str] = None) -> dict:
    h = {
        "accept": "application/json",
        "content-type": "application/json",
        "origin": "https://chat.anuma.ai",
        "referer": "https://chat.anuma.ai/",
        "user-agent": UA,
        "privy-app-id": PRIVY_APP_ID,
        "privy-client": PRIVY_CLIENT,
        "privy-ca-id": caid or str(uuid.uuid4()),
    }
    if auth:
        h["authorization"] = f"Bearer {auth}"
    return h


def privy_create_wallet(access_token: str, caid: Optional[str] = None) -> dict:
    import requests
    r = requests.post(f"{PRIVY_AUTH}/wallets", headers=_privy_headers(access_token, caid=caid),
                      json={"chain_type": "ethereum"}, timeout=30)
    if r.status_code != 200:
        raise RuntimeError(f"创建钱包失败 HTTP {r.status_code}: {r.text[:300]}")
    return r.json()


def privy_refresh_session(access_token: str, refresh_token: str, caid: Optional[str] = None) -> dict:
    import requests
    r = requests.post(f"{PRIVY_AUTH}/sessions", headers=_privy_headers(access_token, caid=caid),
                      json={"refresh_token": refresh_token}, timeout=30)
    if r.status_code != 200:
        raise RuntimeError(f"刷新 session 失败 HTTP {r.status_code}: {r.text[:300]}")
    return r.json()


def get_balance(identity_token: str, caid: Optional[str] = None) -> dict:
    import requests
    h = _privy_headers(identity_token, caid=caid)
    h.pop("content-type")
    r = requests.get(f"{PORTAL}/credits/balance", headers=h, timeout=30)
    if r.status_code != 200:
        raise RuntimeError(f"查询余额失败 HTTP {r.status_code}: {r.text[:300]}")
    return r.json()


def jwt_exp(token: str) -> Optional[int]:
    try:
        b = token.split(".")[1]
        b += "=" * (4 - len(b) % 4)
        return json.loads(base64.b64decode(b)).get("exp")
    except Exception:
        return None


# ---------------------------------------------------------------------------
# 浏览器流程 (camoufox + hcaptcha-challenger AgentV)
# ---------------------------------------------------------------------------
async def _dismiss_cookies(page) -> None:
    for name in ("Accept all", "Reject all"):
        try:
            btn = page.get_by_role("button", name=name, exact=True).first
            if await btn.count() and await btn.is_visible(timeout=1500):
                await btn.click(timeout=3000)
                log(f"cookie 弹窗已关闭: {name}")
                await page.wait_for_timeout(600)
                return
        except Exception:
            pass


async def _has_visible_challenge(page) -> bool:
    return await page.evaluate("""
      () => {
        for (const f of document.querySelectorAll('iframe')) {
          if (f.src.includes('frame=challenge')) {
            const r = f.getBoundingClientRect();
            if (r.width > 50 && r.height > 50) return true;
          }
        }
        return false;
      }
    """)


async def _wait_otp_ui(page, agent, timeout: float = 90) -> bool:
    """等 OTP UI 出现; 期间若 hCaptcha 弹 challenge 则用 AgentV 解."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        body = await page.evaluate("document.body ? document.body.innerText.slice(0, 1200) : ''")
        if "verification code" in body.lower() or "Enter verification" in body:
            return True
        if await _has_visible_challenge(page):
            log("检测到 hCaptcha challenge, AgentV 开始解决...")
            from hcaptcha_challenger.agent.challenger import ChallengeSignal
            sig = await agent.wait_for_challenge()
            log(f"challenge 结果: {sig}")
            if sig == ChallengeSignal.FAILURE:
                log("challenge 失败, 重新提交表单")
                try:
                    await page.evaluate("document.querySelector('button[type=submit]').click()")
                except Exception:
                    pass
        await page.wait_for_timeout(2000)
    return False


async def _extract_tokens(page) -> dict:
    ls = await page.evaluate("() => { const o={}; for (let i=0;i<localStorage.length;i++){const k=localStorage.key(i); o[k]=localStorage.getItem(k);} return o; }")
    return {
        "token": (ls.get("privy:token") or "").strip('"'),
        "pat": (ls.get("privy:pat") or "").strip('"'),
        "id_token": (ls.get("privy:id_token") or "").strip('"'),
        "refresh_token": (ls.get("privy:refresh_token") or "").strip('"'),
    }


async def register_one(mail: TempMail, debug: bool = False) -> dict:
    email = mail.create()
    log(f"临时邮箱: {_mask_email(email)}")

    from camoufox.async_api import AsyncCamoufox
    from hcaptcha_challenger import AgentV, AgentConfig
    from hcaptcha_challenger.agent.challenger import ChallengeSignal

    launch_kwargs = {
        "headless": "virtual",
        "humanize": True,
        "window": (1366, 850),
        "os": "windows",
        "i_know_what_im_doing": True,
    }
    async with AsyncCamoufox(**launch_kwargs) as browser:
        page = await browser.new_page()
        agent = AgentV(page=page, agent_config=AgentConfig(DISABLE_BEZIER_TRAJECTORY=True))
        log(f"AgentV 就绪, hCaptcha 模型: {OPENAI_MODEL}")

        await page.goto(SIGNUP_URL, wait_until="domcontentloaded", timeout=90000)
        await page.wait_for_timeout(6000)
        await _dismiss_cookies(page)

        inp = page.locator("input[placeholder*='email']").first
        await inp.fill(email)
        await page.wait_for_timeout(600)
        try:
            await page.get_by_role("button", name="Continue", exact=True).first.click(timeout=5000)
        except Exception:
            await page.evaluate("document.querySelector('button[type=submit]').click()")
        log("邮箱已提交, 等待 OTP UI / hCaptcha")

        if not await _wait_otp_ui(page, agent):
            raise RuntimeError("OTP UI 未出现")
        await _dismiss_cookies(page)

        code = mail.wait_otp(timeout=150)
        log(f"收到验证码: {code if debug else _mask_code(code)}")
        await _dismiss_cookies(page)

        boxes = page.locator("input[maxlength='1']")
        n = await boxes.count()
        if n >= 6:
            try:
                await boxes.first.click(timeout=5000)
            except Exception:
                await page.evaluate("document.querySelector('input[maxlength=\"1\"]').focus()")
            await page.wait_for_timeout(400)
            await page.keyboard.type(code)
            await page.wait_for_timeout(500)
            await page.keyboard.press("Enter")
        else:
            s = page.locator("input[maxlength='6']").first
            if await s.count():
                try:
                    await s.click(timeout=5000)
                except Exception:
                    await s.focus()
                await s.fill(code)
                await s.press("Enter")
        log("OTP 已输入, 等待 Privy 认证完成...")

        tokens: dict = {}
        for i in range(25):
            await page.wait_for_timeout(3000)
            tokens = await _extract_tokens(page)
            if tokens["id_token"] and tokens["pat"]:
                log(f"Privy 认证完成 ({(i + 1) * 3}s)")
                break
        if not tokens.get("id_token"):
            raise RuntimeError("Privy 认证未完成: localStorage 无 privy:id_token")

        if debug:
            await page.screenshot(path=str(ROOT / "debug_final.png"))

    # ---- 浏览器外: 创建嵌入式钱包 + 刷新 session + 查余额 ----
    caid = uuid.uuid4()
    wallet = privy_create_wallet(tokens["pat"], caid=str(caid))
    wallet_addr = wallet.get("address") or ""
    log(f"嵌入式钱包创建成功: {wallet_addr} (id={wallet.get('id')})")

    sess = privy_refresh_session(tokens["pat"], tokens["refresh_token"], caid=str(caid))
    id_token = sess.get("identity_token") or tokens["id_token"]
    pat = sess.get("privy_access_token") or tokens["pat"]
    refresh = sess.get("refresh_token") or tokens["refresh_token"]

    bal = get_balance(id_token, caid=str(caid))
    log(f"余额: {bal.get('available_credits')} credits (tier={bal.get('subscription_tier')})")

    # 权威钱包地址: portal 认的 wallet_index=0 (可能与 POST /wallets 的不同,
    # 因为浏览器 SDK createOnLogin=all-users 可能已自动建过一个)
    portal_wallet = bal.get("wallet_address") or ""
    if portal_wallet:
        wallet_addr = portal_wallet
    # 兜底: linked_accounts 里的第一个 wallet
    if not wallet_addr:
        for la in (sess.get("user") or {}).get("linked_accounts", []):
            if la.get("type") == "wallet" and la.get("address"):
                wallet_addr = la["address"]
                break
    log(f"最终钱包地址: {wallet_addr}")

    return {
        "email": email,
        "wallet_address": wallet_addr,
        "wallet_id": wallet.get("id", ""),
        "user_id": (sess.get("user") or {}).get("id", ""),
        "available_credits": bal.get("available_credits"),
        "subscription_tier": bal.get("subscription_tier"),
        "identity_token": id_token,
        "access_token": pat,
        "refresh_token": refresh,
        "expires_at": jwt_exp(id_token),
        "created_at": datetime.now(timezone.utc).isoformat(),
    }


# ---------------------------------------------------------------------------
# 网关上传 (SPEC-upload §3)
# ---------------------------------------------------------------------------
def _upload_payload(acc: dict) -> dict:
    """把账号记录(accounts.json 或 CSV 行)映射为网关 AccountInput JSON."""
    return {
        "email": acc.get("email", ""),
        "wallet_address": acc.get("wallet_address", ""),
        "wallet_id": acc.get("wallet_id", ""),
        "user_id": acc.get("user_id", ""),
        "tier": acc.get("tier") or acc.get("subscription_tier", ""),
        "identity_token": acc.get("identity_token", ""),
        "access_token": acc.get("access_token", ""),
        "refresh_token": acc.get("refresh_token", ""),
        "available_credits": int(acc.get("available_credits") or 0),
        "expires_at": int(acc.get("expires_at") or 0),
    }


def upload_to_gateway(account: dict, max_retries: int = 2, retry_interval: float = 2.0) -> bool:
    """POST 单个账号到网关 upsert。失败重试 max_retries 次(间隔 retry_interval)，
    仍失败只记日志返回 False —— 不阻塞注册(CSV 已备份, 可稍后 --import-csv 补传)。"""
    import requests
    url = f"{GATEWAY_URL}/api/accounts"
    headers = {"Authorization": f"Bearer {GATEWAY_TOKEN}", "Content-Type": "application/json"}
    payload = _upload_payload(account)
    last_err = ""
    for attempt in range(max_retries + 1):
        try:
            r = requests.post(url, headers=headers, json=payload, timeout=15)
            if r.status_code == 200:
                log(f"[+] 已上传网关: {_mask_email(account.get('email'))}")
                return True
            last_err = f"HTTP {r.status_code}: {r.text[:200]}"
        except Exception as e:
            last_err = str(e)
        if attempt < max_retries:
            time.sleep(retry_interval)
    log(f"[!] 网关上传失败 ({last_err}), 已重试 {max_retries} 次 — CSV 已有备份, 可稍后 --import-csv 补传")
    return False


def import_csv(args) -> int:
    """--import-csv: 读 accounts.csv 批量上传回网关(恢复池)。--limit N 限数量,
    --dry-run 只打印预览不上传。"""
    import requests
    import csv as csv_mod
    csv_path = Path(args.csv) if args.csv else ROOT / "accounts.csv"
    if not csv_path.exists():
        log(f"[!] CSV 不存在: {csv_path}")
        return 1
    with open(csv_path, newline="", encoding="utf-8") as f:
        rows = [r for r in csv_mod.DictReader(f) if r.get("email") and r.get("identity_token")]
    total = len(rows)
    if args.limit and args.limit > 0:
        rows = rows[: args.limit]
    url = f"{GATEWAY_URL}/api/accounts"
    headers = {"Authorization": f"Bearer {GATEWAY_TOKEN}", "Content-Type": "application/json"}
    if args.dry_run:
        log(f"[dry-run] 将上传 {len(rows)}/{total} 个账号到 {url}")
        for r in rows:
            log(f"  - {_mask_email(r['email'])} | credits={r.get('available_credits')} | tier={r.get('subscription_tier')}")
        return 0
    try:
        r = requests.post(url, headers=headers, json=[_upload_payload(x) for x in rows], timeout=60)
        if r.status_code == 200:
            d = r.json().get("data", {})
            log(f"[+] 批量上传成功: added={d.get('added')} updated={d.get('updated')} (limit={args.limit or 'all'})")
            return 0
        log(f"[!] 批量上传失败 HTTP {r.status_code}: {r.text[:300]}")
        return 1
    except Exception as e:
        log(f"[!] 批量上传异常: {e}")
        return 1


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------
def main():
    ap = argparse.ArgumentParser(description="Anuma AI 账号自动注册 (Privy + hCaptcha + 嵌入式钱包) + 网关上传")
    ap.add_argument("--count", type=int, default=1, help="注册账号数 (默认 1)")
    ap.add_argument("--debug", action="store_true", help="保留截图等调试产物")
    ap.add_argument("--out", default=str(ROOT / "accounts.json"), help="输出文件 (默认 accounts.json)")
    ap.add_argument("--import-csv", action="store_true", help="从 CSV 批量上传账号到网关 (恢复池)")
    ap.add_argument("--csv", default=None, help="--import-csv 的 CSV 路径 (默认 accounts.csv)")
    ap.add_argument("--limit", type=int, default=0, help="--import-csv 最多上传 N 个 (默认全部)")
    ap.add_argument("--dry-run", action="store_true", help="--import-csv 只打印预览, 不上传")
    ap.add_argument("--max-consecutive-failures", type=int, default=5,
                    help="连续注册失败达到该次数即熔断退出 (默认 5)")
    ap.add_argument("--mail-provider", default=None,
                    help="临时邮箱提供商: cftemp (默认) | yydsmail (覆盖环境变量 MAIL_PROVIDER)")
    args = ap.parse_args()

    global MAIL_PROVIDER
    if args.mail_provider:
        MAIL_PROVIDER = args.mail_provider.strip().lower()

    if args.import_csv:
        sys.exit(import_csv(args))

    out_path = Path(args.out)
    csv_path = out_path.with_suffix(".csv")
    accounts = []
    if out_path.exists():
        try:
            accounts = json.loads(out_path.read_text())
        except Exception:
            bak_path = out_path.with_suffix(".json.bak")
            shutil.copy2(out_path, bak_path)
            log(f"[!] {out_path} 损坏, 已备份到 {bak_path}, 从空列表继续")
            accounts = []
    start_len = len(accounts)

    csv_fields = ["email", "wallet_address", "wallet_id", "user_id", "available_credits",
                  "subscription_tier", "expires_at", "created_at",
                  "identity_token", "access_token", "refresh_token"]

    def write_csv():
        import csv
        tmp_path = csv_path.with_name(csv_path.name + ".tmp")
        with open(tmp_path, "w", newline="", encoding="utf-8") as f:
            w = csv.DictWriter(f, fieldnames=csv_fields, extrasaction="ignore")
            w.writeheader()
            for a in accounts:
                w.writerow(a)
        os.replace(tmp_path, csv_path)
        log(f"CSV 已写入: {csv_path} ({len(accounts)} 账号)")

    consecutive_failures = 0
    for i in range(args.count):
        log(f"===== 注册账号 {i + 1}/{args.count} =====")
        try:
            acc = asyncio.run(register_one(make_mail_provider(), debug=args.debug))
            accounts.append(acc)
            tmp_out = out_path.with_suffix(".json.tmp")
            tmp_out.write_text(json.dumps(accounts, ensure_ascii=False, indent=2))
            os.replace(tmp_out, out_path)
            write_csv()
            log(f"[+] {_mask_email(acc['email'])} -> {acc['wallet_address']} | {acc['available_credits']} credits")
            # 注册成功 → 秒级推送网关 (失败不阻塞, CSV 已备份)
            upload_to_gateway(acc)
            consecutive_failures = 0
        except Exception as e:
            log(f"[!] 账号 {i + 1} 注册失败: {e}")
            if args.debug:
                import traceback
                traceback.print_exc()
            consecutive_failures += 1
            if consecutive_failures >= args.max_consecutive_failures:
                log(f"[!] 连续失败 {consecutive_failures} 次, 达到熔断阈值 {args.max_consecutive_failures}, 退出")
                sys.exit(1)
            time.sleep(3)

    ok = len(accounts) - start_len
    log(f"完成: 本次成功 {ok} / 尝试 {args.count} | 累计 {len(accounts)} 账号 | CSV: {csv_path}")


if __name__ == "__main__":
    main()
