"""Field and facet builders — ``f.text("title", "Blue Shoes")`` reads better
than dict literals and pins the right ``type`` string."""

from __future__ import annotations

from datetime import date, datetime, timezone
from typing import Any, Dict, Optional, Union


def _mk(name: str, type_: str, value: Any, language: Optional[str] = None) -> Dict[str, Any]:
    field: Dict[str, Any] = {"name": name, "type": type_, "value": value}
    if language is not None:
        field["language"] = language
    return field


class f:
    """Field builders for search documents."""

    @staticmethod
    def text(name: str, value: str, language: Optional[str] = None) -> Dict[str, Any]:
        return _mk(name, "text", value, language)

    @staticmethod
    def html(name: str, value: str, language: Optional[str] = None) -> Dict[str, Any]:
        return _mk(name, "html", value, language)

    @staticmethod
    def atom(name: str, value: str) -> Dict[str, Any]:
        return _mk(name, "atom", value)

    @staticmethod
    def number(name: str, value: float) -> Dict[str, Any]:
        return _mk(name, "number", value)

    @staticmethod
    def date(name: str, value: Union[datetime, date, str, int]) -> Dict[str, Any]:
        """Accepts a datetime/date, ISO string, or epoch milliseconds."""
        if isinstance(value, datetime):
            if value.tzinfo is None:
                value = value.replace(tzinfo=timezone.utc)
            value = value.isoformat()
        elif isinstance(value, date):
            value = value.isoformat()
        return _mk(name, "date", value)

    @staticmethod
    def geo(name: str, lat: float, lng: float) -> Dict[str, Any]:
        return _mk(name, "geo", {"lat": lat, "lng": lng})

    @staticmethod
    def tokenprefix(name: str, value: str) -> Dict[str, Any]:
        return _mk(name, "tokenprefix", value)

    @staticmethod
    def untokenprefix(name: str, value: str) -> Dict[str, Any]:
        return _mk(name, "untokenprefix", value)


class facet:
    """Facet builders for document facets."""

    @staticmethod
    def atom(name: str, value: str) -> Dict[str, Any]:
        return {"name": name, "type": "atom", "value": value}

    @staticmethod
    def number(name: str, value: float) -> Dict[str, Any]:
        return {"name": name, "type": "number", "value": value}
