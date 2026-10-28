import { expect } from "@playwright/test";
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
