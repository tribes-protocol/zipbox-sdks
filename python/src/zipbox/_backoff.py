"""Exponential backoff with a cap and jitter. Never a ceiling on attempts."""

from __future__ import annotations

import random
from collections.abc import Callable


class Backoff:
    """``next()`` returns the delay before the next attempt; ``reset()`` on success."""

    def __init__(
        self,
        base_s: float,
        cap_s: float,
        *,
        rng: Callable[[], float] = random.random,
    ) -> None:
        self.base_s = base_s
        self.cap_s = cap_s
        self.attempt = 0
        self._rng = rng

    def peek(self) -> float:
        """The un-jittered delay the next ``next()`` is based on."""
        return float(min(self.cap_s, self.base_s * (2.0**self.attempt)))

    def next(self) -> float:
        """Advance the ladder; jitter keeps a fleet of clients from retrying in step."""
        delay = self.peek() * (0.5 + self._rng() / 2)
        self.attempt = min(self.attempt + 1, 32)
        return delay

    def reset(self) -> None:
        """Back to the first rung. Call only on observed success."""
        self.attempt = 0
