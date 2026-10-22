"""Check the public management MCP surface without contacting a server."""

import importlib.util
import json
from pathlib import Path
import unittest

HERE = Path(__file__).resolve().parent
MODULE = importlib.util.spec_from_file_location("management_prepare", HERE / "prepare.py")
prepare = importlib.util.module_from_spec(MODULE)
MODULE.loader.exec_module(prepare)


class ManagementSurfaceTests(unittest.TestCase):
    def test_includes_mcp_connections_and_virtual_mcp_management(self):
        paths = [
            "/api/mcp/clients",
            "/api/mcp/client",
            "/api/mcp/client/{id}",
            "/api/mcp/client/{id}/refresh-tools",
            "/api/mcp/client/{id}/reauthorize",
            "/api/mcp/virtual-mcps",
            "/api/mcp/virtual-mcps/{id}",
            "/api/mcp/virtual-mcps/{id}/virtual-keys/{vkId}",
        ]
        for path in paths:
            with self.subTest(path=path):
                self.assertTrue(prepare.selected_path(path))

    def test_does_not_expand_to_unrequested_management_surfaces(self):
        for path in ["/api/mcp/library", "/api/governance/virtual-keys", "/api/oauth/config", "/v1/chat/completions"]:
            with self.subTest(path=path):
                self.assertFalse(prepare.selected_path(path))

    def test_snapshot_is_self_contained_and_contains_the_selected_operations(self):
        source = json.loads(prepare.SOURCE.read_text(encoding="utf-8"))
        snapshot = json.loads(prepare.DESTINATION.read_text(encoding="utf-8"))
        expected = {path: item for path, item in source["paths"].items() if prepare.selected_path(path)}
        self.assertEqual(snapshot["paths"], expected)
        components = {}
        prepare.collect_components(snapshot["paths"], snapshot, components)
        self.assertLess(prepare.DESTINATION.stat().st_size, 5 << 20)

    def test_mcp_management_operations_support_the_configured_admin_auth(self):
        snapshot = json.loads(prepare.DESTINATION.read_text(encoding="utf-8"))
        for path, item in snapshot["paths"].items():
            if not path.startswith("/api/mcp/"):
                continue
            for method, operation in item.items():
                if method in prepare.METHODS:
                    with self.subTest(path=path, method=method):
                        self.assertTrue(any("BasicAuth" in auth for auth in operation.get("security", snapshot["security"])))


if __name__ == "__main__":
    unittest.main()
