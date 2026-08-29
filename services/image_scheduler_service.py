from __future__ import annotations

import os
import threading
import time
import uuid
from typing import Any

from services.account_service import account_service
from services.proxy_service import proxy_settings


class ImageSchedulerService:
    """Private account/proxy lease manager used by the Go image gateway."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._reservations: dict[str, dict[str, Any]] = {}

    @staticmethod
    def _ttl_seconds() -> int:
        try:
            return max(60, min(int(os.getenv("IMAGE_SCHEDULER_RESERVATION_TTL_SECS", "900")), 7200))
        except ValueError:
            return 900

    def _public(self, item: dict[str, Any]) -> dict[str, Any]:
        # A reservation identifier is all the Go gateway needs.  Never expose
        # account tokens, proxy URLs, or proxy credentials through this API.
        hidden = {"account_token", "proxy_profile", "proxy_url", "egress_key"}
        return {key: value for key, value in item.items() if key not in hidden}

    def _release_item(self, item: dict[str, Any], *, failed: bool) -> None:
        account_service.mark_image_result(str(item.get("account_token") or ""), not failed, refresh_account=failed)
        profile = item.get("proxy_profile")
        if profile is not None:
            proxy_settings.release_image_egress(profile)

    def _reap_expired_locked(self) -> int:
        now = time.time()
        expired = [key for key, item in self._reservations.items() if float(item.get("expires_at") or 0) <= now]
        for key in expired:
            self._release_item(self._reservations.pop(key), failed=True)
        return len(expired)

    def reserve(self, *, model: str = "gpt-image-2") -> dict[str, Any]:
        del model
        with self._lock:
            self._reap_expired_locked()
            token = account_service._acquire_next_candidate_token()
            try:
                account = account_service.get_account(token) or {"access_token": token}
                profile = proxy_settings.get_profile(account=account, upstream=True, reserve_image_egress=True)
            except Exception:
                account_service.release_image_slot(token)
                raise
            now = time.time()
            reservation_id = uuid.uuid4().hex
            item = {
                "reservation_id": reservation_id, "account_token": token,
                "account_email": str(account.get("email") or ""),
                "proxy_url": profile.proxy_url, "proxy_source": profile.proxy_source,
                "proxy_group_id": profile.proxy_group_id, "proxy_node_id": profile.proxy_node_id,
                "egress_key": profile.egress_key, "proxy_profile": profile,
                "created_at": now, "expires_at": now + self._ttl_seconds(), "state": "reserved",
            }
            self._reservations[reservation_id] = item
            return self._public(item)

    def claim(self, reservation_id: str) -> dict[str, Any]:
        with self._lock:
            self._reap_expired_locked()
            item = self._reservations.get(str(reservation_id))
            if item is None:
                raise KeyError("reservation was not found or expired")
            if item.get("state") != "reserved":
                raise RuntimeError("reservation is already executing")
            item["state"] = "executing"
            return dict(item)

    def release(self, reservation_id: str, *, failed: bool = False) -> dict[str, Any]:
        with self._lock:
            self._reap_expired_locked()
            item = self._reservations.pop(str(reservation_id), None)
            if item is None:
                return {"released": False, "reservation_id": str(reservation_id)}
            self._release_item(item, failed=failed)
            return {"released": True, "failed": bool(failed), "reservation_id": str(reservation_id)}

    def status(self) -> dict[str, Any]:
        with self._lock:
            reaped = self._reap_expired_locked()
            return {"active_reservations": len(self._reservations), "expired_reaped": reaped,
                    "reservations": [self._public(item) for item in self._reservations.values()]}


image_scheduler_service = ImageSchedulerService()
