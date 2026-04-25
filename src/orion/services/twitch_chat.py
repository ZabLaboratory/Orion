"""Twitch IRC chat connector.

Minimal TMI-compatible IRC client that connects to ``irc.chat.twitch.tv:6697`` over
TLS, authenticates with an OAuth access token, joins one or more channels, and yields
parsed PRIVMSG events as async iterables.

This is prep work — the intended consumer is the chat-component engine that reads
these events and triggers blueprint-backed UI components. Integration with the
blueprint runtime lives outside this service (see ``chat_service.py`` for the event
bus and ``routes/chat.py`` for the WS surface).
"""

from __future__ import annotations

import asyncio
import logging
from collections.abc import AsyncIterator
from dataclasses import dataclass, field
from typing import Any

logger = logging.getLogger(__name__)

_IRC_HOST = "irc.chat.twitch.tv"
_IRC_TLS_PORT = 6697


@dataclass
class ChatMessage:
    """A parsed PRIVMSG from Twitch IRC."""

    channel: str
    author_login: str
    author_display: str | None
    author_id: str | None
    content: str
    badges: dict[str, Any] = field(default_factory=dict)
    emotes: dict[str, Any] = field(default_factory=dict)
    tags: dict[str, str] = field(default_factory=dict)
    sent_at_ts: int | None = None


def _parse_tags(raw: str) -> dict[str, str]:
    tags: dict[str, str] = {}
    for pair in raw.split(";"):
        if "=" in pair:
            key, value = pair.split("=", 1)
            tags[key] = value
    return tags


def _parse_badges(raw: str | None) -> dict[str, str]:
    if not raw:
        return {}
    out: dict[str, str] = {}
    for item in raw.split(","):
        if "/" in item:
            k, v = item.split("/", 1)
            out[k] = v
    return out


def _parse_privmsg(line: str) -> ChatMessage | None:
    """Parse a single IRC line. Returns None for non-PRIVMSG lines."""
    if not line.startswith("@"):
        return None
    try:
        raw_tags, rest = line[1:].split(" ", 1)
    except ValueError:
        return None
    tags = _parse_tags(raw_tags)
    # :nick!user@host PRIVMSG #channel :content
    if " PRIVMSG " not in rest:
        return None
    prefix_and_cmd, content = rest.split(" :", 1)
    parts = prefix_and_cmd.split(" ")
    if len(parts) < 3:
        return None
    prefix = parts[0]  # :nick!user@host
    channel = parts[2]
    login = prefix.lstrip(":").split("!", 1)[0]
    sent_at_ts: int | None = None
    if "tmi-sent-ts" in tags:
        try:
            sent_at_ts = int(tags["tmi-sent-ts"])
        except ValueError:
            sent_at_ts = None
    return ChatMessage(
        channel=channel.lstrip("#"),
        author_login=login,
        author_display=tags.get("display-name") or None,
        author_id=tags.get("user-id") or None,
        content=content.rstrip("\r\n"),
        badges=_parse_badges(tags.get("badges")),
        emotes={"raw": tags.get("emotes")} if tags.get("emotes") else {},
        tags=tags,
        sent_at_ts=sent_at_ts,
    )


class TwitchChatClient:
    """Async IRC client for a single OAuth identity. One client per authenticated user.

    Usage::

        client = TwitchChatClient(access_token, nick)
        await client.connect()
        await client.join("channel_login")
        async for msg in client.messages():
            ...
        await client.close()
    """

    def __init__(self, access_token: str, nick: str) -> None:
        self._token = access_token
        self._nick = nick.lower()
        self._reader: asyncio.StreamReader | None = None
        self._writer: asyncio.StreamWriter | None = None
        self._joined: set[str] = set()
        self._closed = False

    async def connect(self) -> None:
        self._reader, self._writer = await asyncio.open_connection(_IRC_HOST, _IRC_TLS_PORT, ssl=True)
        # Request tags + commands + membership capabilities.
        self._send("CAP REQ :twitch.tv/tags twitch.tv/commands twitch.tv/membership")
        self._send(f"PASS oauth:{self._token}")
        self._send(f"NICK {self._nick}")
        await self._drain()

    def _send(self, line: str) -> None:
        if self._writer is None:
            raise RuntimeError("TwitchChatClient not connected")
        self._writer.write((line + "\r\n").encode("utf-8"))

    async def _drain(self) -> None:
        if self._writer is None:
            raise RuntimeError("TwitchChatClient not connected")
        await self._writer.drain()

    async def join(self, channel: str) -> None:
        channel = channel.lower().lstrip("#")
        if channel in self._joined:
            return
        self._send(f"JOIN #{channel}")
        await self._drain()
        self._joined.add(channel)

    async def part(self, channel: str) -> None:
        channel = channel.lower().lstrip("#")
        if channel not in self._joined:
            return
        self._send(f"PART #{channel}")
        await self._drain()
        self._joined.discard(channel)

    async def send(self, channel: str, content: str) -> None:
        channel = channel.lower().lstrip("#")
        self._send(f"PRIVMSG #{channel} :{content}")
        await self._drain()

    async def messages(self) -> AsyncIterator[ChatMessage]:
        if self._reader is None:
            raise RuntimeError("TwitchChatClient not connected")
        while not self._closed:
            raw = await self._reader.readline()
            if not raw:
                logger.info("Twitch IRC closed the connection")
                return
            line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
            if line.startswith("PING"):
                self._send(line.replace("PING", "PONG", 1))
                await self._drain()
                continue
            parsed = _parse_privmsg(line)
            if parsed is not None:
                yield parsed

    async def close(self) -> None:
        self._closed = True
        if self._writer is not None:
            self._writer.close()
            try:
                await self._writer.wait_closed()
            except Exception:  # noqa: BLE001 — best-effort cleanup
                logger.debug("Error closing Twitch IRC writer", exc_info=True)
