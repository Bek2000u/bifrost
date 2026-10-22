#!/usr/bin/env python3
"""Extract the requested management surfaces from Bifrost's bundled OpenAPI."""

import copy
import json
from pathlib import Path


HERE = Path(__file__).resolve().parent
SOURCE = HERE.parents[2] / "docs/openapi/openapi.json"
DESTINATION = HERE / "management.openapi.json"
METHODS = {"get", "post", "put", "patch", "delete", "head", "options"}


def selected_path(path):
    return (
        path == "/api/config"
        or path.startswith("/api/config/")
        or path == "/api/proxy-config"
        or path == "/api/providers"
        or path.startswith("/api/providers/")
        or path == "/api/logs"
        or path.startswith("/api/logs/")
        or path in {"/api/mcp/clients", "/api/mcp/client", "/api/mcp/virtual-mcps"}
        or path.startswith("/api/mcp/client/")
        or path.startswith("/api/mcp/virtual-mcps/")
    )


def collect_components(value, source, components):
    if isinstance(value, dict):
        reference = value.get("$ref")
        if reference is not None:
            parts = reference.split("/")
            if len(parts) < 4 or parts[:2] != ["#", "components"]:
                raise ValueError(f"Expected an internal component reference: {reference}")
            group, name = (part.replace("~1", "/").replace("~0", "~") for part in parts[2:4])
            bucket = components.setdefault(group, {})
            if name not in bucket:
                definition = copy.deepcopy(source["components"][group][name])
                bucket[name] = definition
                collect_components(definition, source, components)
        for item in value.values():
            collect_components(item, source, components)
    elif isinstance(value, list):
        for item in value:
            collect_components(item, source, components)


def main():
    source = json.loads(SOURCE.read_text(encoding="utf-8"))
    paths = {path: copy.deepcopy(item) for path, item in source["paths"].items() if selected_path(path)}
    operations = [operation for item in paths.values() for method, operation in item.items() if method in METHODS]
    if not operations:
        raise ValueError("The source has no matching management operations")
    security = copy.deepcopy(source.get("security", []))
    scheme_names = {
        name
        for requirements in [security, *(operation.get("security", []) for operation in operations)]
        for requirement in requirements
        for name in requirement
    }
    components = {
        "securitySchemes": {
            name: copy.deepcopy(source["components"]["securitySchemes"][name])
            for name in sorted(scheme_names)
        }
    }
    collect_components(paths, source, components)
    spec = {
        "openapi": source["openapi"],
        "info": {
            "title": "Bifrost management MCP",
            "version": source["info"]["version"],
            "description": "Manage Bifrost providers, settings, logs, MCP connections and Virtual MCP endpoints through the management API.",
        },
        "servers": [{"url": "https://gateway.norbek.uz"}],
        "paths": paths,
        "components": components,
    }
    if security:
        spec["security"] = security
    data = (json.dumps(spec, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
    if len(data) > 5 << 20:
        raise ValueError("The selected spec exceeds Bifrost's 5 MiB import limit")
    DESTINATION.write_bytes(data)
    print(f"{len(paths)} paths, {len(operations)} operations, {len(data)} bytes -> {DESTINATION}")


if __name__ == "__main__":
    main()
