from __future__ import annotations

import base64
import hashlib
import re
import threading
from datetime import datetime, timezone
from typing import Any
from urllib.parse import urlsplit
from urllib.request import Request, urlopen

from services.config import config
from utils.log import logger


SUPPORTED_PROXY_SCHEMES = {"http", "https", "socks4", "socks5"}
MAX_SUBSCRIPTION_BYTES = 5 * 1024 * 1024
DEFAULT_REFRESH_MINUTES = 30.0


def _clean(value: object) -> str:
    return str(value or "").strip()


def _group_id(value: object) -> str:
    raw = _clean(value).lower()
    if raw.startswith("group:"):
        raw = raw.split(":", 1)[1]
    return re.sub(r"[^a-z0-9_-]+", "-", raw).strip("-_")


def _refresh_minutes(value: object) -> float:
    try:
        return max(5.0, min(1440.0, float(value)))
    except (TypeError, ValueError, OverflowError):
        return DEFAULT_REFRESH_MINUTES


def _normalize_proxy_url(value: object) -> str:
    raw = _clean(value).strip("\ufeff\"'")
    if not raw or raw.startswith(("#", ";", "//")):
        return ""
    raw = raw.split("#", 1)[0].strip()
    lower = raw.lower()
    if lower.startswith("socks5h://"):
        raw = "socks5://" + raw[len("socks5h://"):]
    if "://" not in raw:
        parts = raw.split(":")
        if len(parts) == 4 and parts[1].isdigit():
            host, port, username, password = parts
            raw = f"http://{username}:{password}@{host}:{port}"
        else:
            raw = f"http://{raw}"
    try:
        parsed = urlsplit(raw)
        scheme = parsed.scheme.lower()
        port = parsed.port
    except (TypeError, ValueError):
        return ""
    if scheme not in SUPPORTED_PROXY_SCHEMES or not parsed.hostname or not port:
        return ""
    if port < 1 or port > 65535:
        return ""
    return raw


def parse_proxy_subscription(text: str) -> list[str]:
    candidates = re.split(r"[\r\n,;\t ]+", text.strip())
    proxies = [proxy for item in candidates if (proxy := _normalize_proxy_url(item))]
    if not proxies:
        compact = re.sub(r"\s+", "", text)
        try:
            padding = "=" * (-len(compact) % 4)
            decoded = base64.b64decode(compact + padding).decode("utf-8-sig")
        except Exception:
            decoded = ""
        if decoded and decoded != text:
            candidates = re.split(r"[\r\n,;\t ]+", decoded.strip())
            proxies = [proxy for item in candidates if (proxy := _normalize_proxy_url(item))]
    return list(dict.fromkeys(proxies))


def _fetch_subscription(url: str, timeout: int = 20) -> str:
    parsed = urlsplit(url)
    if parsed.scheme.lower() not in {"http", "https"} or not parsed.hostname:
        raise ValueError("subscription URL must use http or https")
    request = Request(url, headers={"User-Agent": "GPTGrok2API-ProxySubscription/1.0"})
    with urlopen(request, timeout=max(3, min(timeout, 60))) as response:
        data = response.read(MAX_SUBSCRIPTION_BYTES + 1)
        if len(data) > MAX_SUBSCRIPTION_BYTES:
            raise ValueError("subscription response exceeds 5 MiB")
        charset = response.headers.get_content_charset() or "utf-8"
    return data.decode(charset, errors="replace")


def _subscription_node(proxy_url: str, index: int, concurrency: int) -> dict[str, Any]:
    parsed = urlsplit(proxy_url)
    digest = hashlib.sha256(proxy_url.encode("utf-8")).hexdigest()[:16]
    return {
        "id": f"sub-{digest}",
        "name": f"订阅 {index} · {parsed.scheme}",
        "url": proxy_url,
        "enabled": True,
        "image_concurrency_limit": concurrency,
        "notes": "订阅自动管理",
        "source": "subscription",
        "subscription_managed": True,
    }


def _groups() -> list[dict[str, Any]]:
    raw = config.get().get("proxy_groups")
    return [dict(item) for item in raw if isinstance(item, dict)] if isinstance(raw, list) else []


def refresh_proxy_group_subscription(group_id: str) -> dict[str, Any]:
    normalized = _group_id(group_id)
    groups = _groups()
    group = next((item for item in groups if _group_id(item.get("id")) == normalized), None)
    if group is None:
        raise ValueError("proxy group not found")
    subscription_url = _clean(group.get("subscription_url"))
    if not subscription_url:
        raise ValueError("proxy subscription URL is required")

    now = datetime.now(timezone.utc).isoformat()
    try:
        proxies = parse_proxy_subscription(_fetch_subscription(subscription_url))
        if not proxies:
            raise ValueError("subscription returned no supported proxies")
        latest_groups = _groups()
        latest_group = next((item for item in latest_groups if _group_id(item.get("id")) == normalized), None)
        if latest_group is None:
            raise ValueError("proxy group was deleted during subscription refresh")
        group = latest_group
        try:
            concurrency = max(0, min(10000, int(group.get("subscription_node_image_concurrency_limit", 30))))
        except (TypeError, ValueError):
            concurrency = 30
        manual_nodes = [
            dict(node) for node in group.get("nodes", [])
            if isinstance(node, dict)
            and not bool(node.get("subscription_managed"))
            and _clean(node.get("source")).lower() != "subscription"
        ]
        subscription_nodes = [
            _subscription_node(proxy, index, concurrency)
            for index, proxy in enumerate(proxies, start=1)
        ]
        group.update({
            "nodes": manual_nodes + subscription_nodes,
            "subscription_last_updated_at": now,
            "subscription_last_error": "",
            "subscription_node_count": len(subscription_nodes),
        })
        next_groups = [group if _group_id(item.get("id")) == normalized else item for item in latest_groups]
        updated = config.update({"proxy_groups": next_groups})
        logger.info({"event": "proxy_subscription_refreshed", "group_id": normalized, "node_count": len(subscription_nodes)})
        return {"group": group, "groups": updated.get("proxy_groups", []), "node_count": len(subscription_nodes)}
    except Exception as exc:
        safe_error = str(exc).replace(subscription_url, "[subscription-url]")[:300]
        latest_groups = _groups()
        latest_group = next((item for item in latest_groups if _group_id(item.get("id")) == normalized), None)
        if latest_group is not None:
            latest_group.update({"subscription_last_attempt_at": now, "subscription_last_error": safe_error})
            next_groups = [latest_group if _group_id(item.get("id")) == normalized else item for item in latest_groups]
            config.update({"proxy_groups": next_groups})
        logger.warning({"event": "proxy_subscription_refresh_failed", "group_id": normalized, "error": safe_error})
        raise


def _is_due(group: dict[str, Any], now: datetime) -> bool:
    if not bool(group.get("subscription_enabled")) or not _clean(group.get("subscription_url")):
        return False
    last = _clean(group.get("subscription_last_updated_at") or group.get("subscription_last_attempt_at"))
    if not last:
        return True
    try:
        previous = datetime.fromisoformat(last.replace("Z", "+00:00"))
        if previous.tzinfo is None:
            previous = previous.replace(tzinfo=timezone.utc)
    except ValueError:
        return True
    return (now - previous).total_seconds() >= _refresh_minutes(group.get("subscription_interval_minutes")) * 60


def start_proxy_subscription_scheduler(stop_event: threading.Event) -> threading.Thread:
    def worker() -> None:
        if stop_event.wait(10):
            return
        while not stop_event.is_set():
            now = datetime.now(timezone.utc)
            for group in _groups():
                if stop_event.is_set():
                    break
                if not _is_due(group, now):
                    continue
                try:
                    refresh_proxy_group_subscription(_clean(group.get("id")))
                except Exception:
                    pass
            stop_event.wait(60)

    thread = threading.Thread(target=worker, name="proxy-subscription-updater", daemon=True)
    thread.start()
    return thread
