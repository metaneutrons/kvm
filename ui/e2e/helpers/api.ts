import { expect, test } from "@playwright/test";
import type { Page } from "@playwright/test";

/**
 * The device password the API tests log in with. Set JETKVM_PASSWORD to the
 * one the device has, so the tests keep it; without it the tests use their
 * own and may have to change the device's.
 */
export const API_TEST_PASSWORD = process.env.JETKVM_PASSWORD ?? "TestPassword123";

/** Tokens the tests create start with this, so they can be cleaned up. */
export const TEST_TOKEN_PREFIX = "e2e ";

export type Headers = Record<string, string>;

export function deviceUrl(path: string): string {
  const base = process.env.JETKVM_URL;
  if (!base) throw new Error("JETKVM_URL environment variable is not set");
  return new URL(path, base).toString();
}

export function bearer(token: string): Headers {
  return { Authorization: `Bearer ${token}` };
}

export function basicAuth(password: string, user = "api"): Headers {
  return { Authorization: `Basic ${Buffer.from(`${user}:${password}`).toString("base64")}` };
}

export interface ApiTokenInfo {
  id: string;
  name: string;
  permissions: string[];
  created_at: string;
  expires_at?: string;
  expired?: boolean;
  last_used_at?: string;
  last_used_ip?: string;
}

export interface ApiTokenList {
  tokens: ApiTokenInfo[];
  limit: number;
  mcp_enabled: boolean;
}

export interface NewApiToken {
  name: string;
  permissions?: string[];
  expires_in_days?: number;
}

/** The token list, read through the logged-in page. */
export async function listApiTokens(page: Page): Promise<ApiTokenList> {
  const res = await page.request.get("/auth/api-tokens");
  expect(res.status(), await res.text()).toBe(200);
  return (await res.json()) as ApiTokenList;
}

/** Creates a token through the logged-in page. The token is only shown here. */
export async function createApiToken(
  page: Page,
  request: NewApiToken,
): Promise<{ token: string; info: ApiTokenInfo }> {
  const res = await page.request.post("/auth/api-tokens", { data: request });
  expect(res.status(), await res.text()).toBe(200);
  return (await res.json()) as { token: string; info: ApiTokenInfo };
}

export async function deleteApiToken(page: Page, id: string): Promise<number> {
  const res = await page.request.delete(`/auth/api-tokens/${id}`);
  return res.status();
}

/** Deletes every token the tests made, and leaves the others alone. */
export async function deleteTestTokens(page: Page): Promise<void> {
  const { tokens } = await listApiTokens(page);
  for (const token of tokens.filter(t => t.name.startsWith(TEST_TOKEN_PREFIX))) {
    await deleteApiToken(page, token.id);
  }
}

export async function setMcpEnabled(page: Page, enabled: boolean): Promise<void> {
  const res = await page.request.put("/auth/mcp", { data: { enabled } });
  expect(res.status(), await res.text()).toBe(200);
}

export interface RpcError {
  code: number;
  message: string;
}

export interface RpcResponse {
  status: number;
  result?: unknown;
  error?: RpcError;
}

/** One JSON-RPC call to /api/rpc with the given credentials. */
export async function apiRpc(
  headers: Headers,
  method: string,
  params: Record<string, unknown> = {},
): Promise<RpcResponse> {
  const res = await fetch(deviceUrl("/api/rpc"), {
    method: "POST",
    headers: { "Content-Type": "application/json", ...headers },
    body: JSON.stringify({ jsonrpc: "2.0", method, params, id: 1 }),
  });
  const text = await res.text();
  let body: { result?: unknown; error?: RpcError } = {};
  try {
    body = JSON.parse(text) as typeof body;
  } catch {
    // Not JSON, for example the answer to a refused request.
  }
  return { status: res.status, result: body.result, error: body.error };
}

export interface McpContent {
  type: string;
  text?: string;
  data?: string;
  mimeType?: string;
}

export interface McpToolResult {
  isError?: boolean;
  content: McpContent[];
  structuredContent?: Record<string, unknown>;
}

export interface McpTool {
  name: string;
  title?: string;
  description?: string;
  inputSchema?: { properties?: Record<string, { enum?: string[] }>; required?: string[] };
  annotations?: { readOnlyHint?: boolean; idempotentHint?: boolean; destructiveHint?: boolean };
}

interface McpEnvelope {
  result?: unknown;
  error?: RpcError;
}

/** A client for /api/mcp that speaks plain JSON over HTTP, as the server answers. */
export function mcpClient(headers: Headers, path = "/api/mcp") {
  let nextId = 1;

  async function request(method: string, params: Record<string, unknown> = {}) {
    const res = await fetch(deviceUrl(path), {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        ...headers,
      },
      body: JSON.stringify({ jsonrpc: "2.0", id: nextId++, method, params }),
    });
    const text = await res.text();
    let envelope: McpEnvelope = {};
    try {
      envelope = JSON.parse(text) as McpEnvelope;
    } catch {
      // Not JSON, for example the answer to a refused request.
    }
    return { status: res.status, ...envelope };
  }

  return {
    request,
    async initialize() {
      return request("initialize", {
        protocolVersion: "2025-06-18",
        capabilities: {},
        clientInfo: { name: "e2e", version: "1" },
      });
    },
    async tools(): Promise<McpTool[]> {
      const res = await request("tools/list");
      expect(res.status, JSON.stringify(res)).toBe(200);
      return (res.result as { tools: McpTool[] }).tools;
    },
    /** Calls a tool and returns its result; a protocol error fails the test. */
    async call(name: string, args: Record<string, unknown> = {}): Promise<McpToolResult> {
      const res = await request("tools/call", { name, arguments: args });
      expect(res.error, `${name}: ${JSON.stringify(res.error)}`).toBeUndefined();
      return res.result as McpToolResult;
    },
  };
}

export function toolText(result: McpToolResult): string {
  return result.content
    .filter(c => c.type === "text")
    .map(c => c.text ?? "")
    .join("\n");
}

/** Width and height of a JPEG, read from its start-of-frame marker. */
export function jpegSize(data: Uint8Array): { width: number; height: number } {
  expect(data[0], "JPEG start marker").toBe(0xff);
  expect(data[1], "JPEG start marker").toBe(0xd8);
  let i = 2;
  while (i + 9 < data.length) {
    if (data[i] !== 0xff) {
      i++;
      continue;
    }
    const marker = data[i + 1];
    // SOF0 to SOF15, except the table markers DHT, JPG and DAC.
    if (marker >= 0xc0 && marker <= 0xcf && ![0xc4, 0xc8, 0xcc].includes(marker)) {
      return { height: (data[i + 5] << 8) | data[i + 6], width: (data[i + 7] << 8) | data[i + 8] };
    }
    i += 2 + ((data[i + 2] << 8) | data[i + 3]);
  }
  throw new Error("no start-of-frame marker in the JPEG");
}

/**
 * GET /api/video/snapshot. A host that sends no video, for example one whose
 * display sleeps, answers 503; that is not what these tests check, so they skip.
 */
export async function takeSnapshot(headers: Headers, query = ""): Promise<Response> {
  const res = await fetch(deviceUrl(`/api/video/snapshot${query}`), { headers });
  if (res.status === 503) {
    test.skip(true, `the host sends no video: ${await res.text()}`);
  }
  return res;
}
