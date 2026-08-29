from __future__ import annotations

import os
import unittest

os.environ.setdefault("CHATGPT2API_AUTH_KEY", "test-auth-key")

from fastapi import HTTPException

from api import image_inputs


class FakeResponse:
    def __init__(self, chunks: list[bytes], headers: dict[str, str] | None = None) -> None:
        self._chunks = chunks
        self.headers = headers or {}

    def iter_content(self, chunk_size: int):
        del chunk_size
        yield from self._chunks


class ImageInputSecurityTest(unittest.TestCase):
    def test_private_addresses_are_rejected(self) -> None:
        with self.assertRaises(HTTPException):
            image_inputs._validate_remote_image_url("http://127.0.0.1/image.png")
        with self.assertRaises(HTTPException):
            image_inputs._validate_remote_image_url("http://169.254.169.254/latest/meta-data")

    def test_public_literal_address_is_allowed(self) -> None:
        image_inputs._validate_remote_image_url("https://8.8.8.8/image.png")

    def test_streaming_limit_stops_oversized_response(self) -> None:
        response = FakeResponse([
            b"a" * image_inputs.MAX_IMAGE_REFERENCE_BYTES,
            b"b",
        ])
        with self.assertRaises(HTTPException):
            image_inputs._read_limited_image_response(response)


if __name__ == "__main__":
    unittest.main()
