import { readFile, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

// Read the native contract from a local checkout; no server or credentials are needed.
if (!process.argv[2]) {
  throw new Error("Usage: node prepare.mjs <open-connector-checkout> [--check]");
}
const source = resolve(process.argv[2], "src/server/api/openapi.ts");
const { createOpenApiDocument } = await import(pathToFileURL(source).href);
const native = createOpenApiDocument([]);
const paths = Object.fromEntries(
  Object.entries(native.paths).filter(([path]) =>
    (path.startsWith("/api/") && !path.startsWith("/api/auth/")) ||
    path === "/v1/providers/{service}/setup" ||
    path.startsWith("/v1/connections") ||
    path.startsWith("/v1/connection-requests/"),
  ),
);
const spec = {
  ...native,
  info: {
    ...native.info,
    title: "OpenConnector management",
    description: "Manage connections, OAuth clients, runtime access, policies, subscriptions, files and execution history through the native API. Provider actions use OpenConnector's five discovery tools. Writes take effect immediately.",
  },
  servers: [{ url: "https://connector.norbek.uz" }],
  paths,
  security: [{ AdminBearer: [] }],
  components: {
    ...native.components,
    securitySchemes: { AdminBearer: { type: "http", scheme: "bearer" } },
  },
};
const specText = `${JSON.stringify(spec, null, 2)}\n`;
if (Buffer.byteLength(specText) > 5 << 20) throw new Error("OpenAPI exceeds Bifrost's 5 MiB import limit");
const client = {
  name: "openconnector_admin",
  endpoint_slug: "openconnector-admin",
  connection_type: "openapi",
  auth_type: "none",
  tools_to_execute: ["*"],
  openapi_config: {
    spec: specText,
    base_url: "https://connector.norbek.uz",
    security_credentials: { AdminBearer: { value: "env.OPENCONNECTOR_ADMIN_TOKEN" } },
  },
};
for (const [name, value] of [["openconnector.openapi.json", specText], ["client.json", `${JSON.stringify(client, null, 2)}\n`]]) {
  const destination = new URL(name, import.meta.url);
  if (process.argv.includes("--check")) {
    if (await readFile(destination, "utf8") !== value) throw new Error(`${name} is stale; rerun without --check`);
  } else {
    await writeFile(destination, value);
  }
}
console.log(`${Object.keys(paths).length} management paths; native contract ${process.argv.includes("--check") ? "verified" : "exported"}; no API calls`);
