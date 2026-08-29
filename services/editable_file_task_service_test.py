from __future__ import annotations

import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from urllib.parse import parse_qs, urlsplit

os.environ.setdefault("CHATGPT2API_AUTH_KEY", "test-auth-key")

from services import editable_file_task_service as editable


class EditableFileTaskSecurityTest(unittest.TestCase):
    def test_storage_components_do_not_preserve_path_input(self) -> None:
        component = editable._storage_component("../../../outside")
        self.assertNotIn("/", component)
        self.assertNotIn("..", component)

    def test_download_requires_matching_signature(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "files"
            path = root / "ppt" / "owner" / "task" / "result.pptx"
            path.parent.mkdir(parents=True)
            path.write_bytes(b"result")
            with patch.object(editable, "EDITABLE_FILE_ROOT", root):
                url = editable._file_url(path, "")
                parsed = urlsplit(url)
                relative_path = parsed.path.removeprefix("/files/")
                signature = parse_qs(parsed.query)["signature"][0]
                self.assertEqual(
                    editable.editable_file_task_service.public_file_path(relative_path, signature),
                    path,
                )
                with self.assertRaises(PermissionError):
                    editable.editable_file_task_service.public_file_path(relative_path, "invalid")


if __name__ == "__main__":
    unittest.main()
