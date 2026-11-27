"""Helpers for applying the selected proxy timezone to isolated Node runtimes."""

from __future__ import annotations

import os
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError


def node_environment_for_timezone(timezone_id: str | None) -> dict[str, str] | None:
    if not timezone_id:
        return None

    try:
        ZoneInfo(timezone_id)
    except (ValueError, ZoneInfoNotFoundError):
        raise ValueError("Invalid IANA timezone for Node runtime") from None

    environment = os.environ.copy()
    environment["TZ"] = timezone_id
    return environment
