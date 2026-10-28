import { expect, test } from "@playwright/test";

import {
  API_TEST_PASSWORD,
  TEST_TOKEN_PREFIX,
  apiRpc,
  basicAuth,
  bearer,
  createApiToken,
  deleteApiToken,
  deleteTestTokens,
  deviceUrl,
  ensureLocalAuthMode,
  jpegSize,
  listApiTokens,
  takeSnapshot,
} from "./helpers";

// Failed credentials count against the login rate limit of the test machine's
// address, and a successful login clears it. Each test therefore sends only a
// few wrong credentials, and every test starts with a login.
const PING = { jsonrpc: "2.0", method: "ping", id: 1 };
const METHOD_NOT_FOUND = -32601;
const NO_PERMISSION = -32001;

test.describe("API access", () => {
  test.setTimeout(120_000);
  test.describe.configure({ mode: "serial" });

  test.beforeEach(async ({ page }) => {
    await ensureLocalAuthMode(page, { mode: "password", password: API_TEST_PASSWORD });
  });

  test.afterEach(async ({ page }) => {
    await deleteTestTokens(page);
  });

  test("refuses requests without valid credentials", async () => {
    const none = await fetch(deviceUrl("/api/rpc"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(PING),
    });
    expect(none.status).toBe(401);

    expect((await apiRpc({ Authorization: "Bearer jkvm_not-a-real-token" }, "ping")).status).toBe(
      401,
    );
    expect((await apiRpc(basicAuth("definitely wrong"), "ping")).status).toBe(401);
  });

  test("a token cannot manage tokens or reach the UI", async ({ page }) => {
    const { token } = await createApiToken(page, {
      name: `${TEST_TOKEN_PREFIX}full`,
      permissions: ["input", "media", "power"],
    });
    for (const [method, path] of [
      ["GET", "/auth/api-tokens"],
      ["POST", "/auth/api-tokens"],
      ["PUT", "/auth/mcp"],
      ["GET", "/device"],
    ]) {
      const res = await fetch(deviceUrl(path), { method, headers: bearer(token) });
      expect(res.status, `${method} ${path} with a token`).toBe(401);
    }
  });

  test("each permission opens only its own methods", async ({ page }) => {
    // Only requests that must be refused are sent with a limited token: an
    // allowed one would act on the host.
    const cases: { permissions: string[]; refused: string[] }[] = [
      { permissions: [], refused: ["keypressReport", "mountWithHTTP", "setATXPowerAction"] },
      { permissions: ["input"], refused: ["mountWithHTTP", "unmountImage", "wakeHost"] },
      { permissions: ["media"], refused: ["keypressReport", "setJigglerState", "setDCPowerState"] },
      { permissions: ["power"], refused: ["keyboardReport", "wheelReport", "mountWithStorage"] },
    ];
    for (const { permissions, refused } of cases) {
      const { token } = await createApiToken(page, {
        name: `${TEST_TOKEN_PREFIX}${permissions.join("+") || "view"}`,
        permissions,
      });
      const auth = bearer(token);

      const ping = await apiRpc(auth, "ping");
      expect(ping.status).toBe(200);
      expect(ping.result).toBe("pong");

      for (const method of refused) {
        const res = await apiRpc(auth, method);
        expect(res.error?.code, `${permissions.join("+") || "view"} calling ${method}`).toBe(
          NO_PERMISSION,
        );
      }
      // Methods outside the API stay out of reach for every token.
      for (const method of ["factoryReset", "setDevModeState", "noSuchMethod"]) {
        const res = await apiRpc(auth, method);
        expect(res.error?.code, method).toBe(METHOD_NOT_FOUND);
      }
    }
  });

  test("an upload needs the media permission", async ({ page }) => {
    const view = await createApiToken(page, { name: `${TEST_TOKEN_PREFIX}view` });
    const media = await createApiToken(page, {
      name: `${TEST_TOKEN_PREFIX}media`,
      permissions: ["media"],
    });
    const upload = (token: string) =>
      fetch(deviceUrl("/api/storage/upload?uploadId=none"), {
        method: "POST",
        headers: bearer(token),
      });
    expect((await upload(view.token)).status).toBe(403);
    // The upload is unknown, which is all that is needed to see the request passed.
    expect((await upload(media.token)).status).toBe(404);
  });

  test("creates, lists, uses and deletes a token", async ({ page }) => {
    const name = `${TEST_TOKEN_PREFIX}lifecycle`;
    const { token, info } = await createApiToken(page, {
      name,
      permissions: ["input"],
      expires_in_days: 7,
    });

    expect(token).toMatch(/^jkvm_[A-Za-z0-9_-]{40,}$/);
    expect(info.name).toBe(name);
    expect(info.permissions).toEqual(["view", "input"]);
    expect(Date.parse(info.created_at)).toBeGreaterThan(Date.now() - 60_000);
    const days = (Date.parse(info.expires_at ?? "") - Date.now()) / 86_400_000;
    // The device and this machine may disagree by a few hours.
    expect(days).toBeGreaterThan(6.5);
    expect(days).toBeLessThan(7.5);

    // The list never contains the token or its hash.
    const raw = await (await page.request.get("/auth/api-tokens")).text();
    expect(raw).not.toContain(token);
    expect(raw).not.toContain('"hash"');
    const listed = (await listApiTokens(page)).tokens.find(t => t.id === info.id);
    expect(listed?.last_used_at).toBeUndefined();

    expect((await apiRpc(bearer(token), "ping")).status).toBe(200);
    const used = (await listApiTokens(page)).tokens.find(t => t.id === info.id);
    expect(used?.last_used_at, "the list shows when the token was last used").toBeTruthy();
    expect(used?.last_used_ip).toBeTruthy();

    expect(await deleteApiToken(page, info.id)).toBe(200);
    expect((await apiRpc(bearer(token), "ping")).status).toBe(401);
    expect(await deleteApiToken(page, info.id)).toBe(404);
  });

  test("rejects invalid token requests", async ({ page }) => {
    const bad: Record<string, unknown>[] = [
      { name: "" },
      { name: "   " },
      { name: "x".repeat(41) },
      { name: `${TEST_TOKEN_PREFIX}a`, permissions: ["root"] },
      { name: `${TEST_TOKEN_PREFIX}a`, expires_in_days: -1 },
      { name: `${TEST_TOKEN_PREFIX}a`, expires_in_days: 3651 },
    ];
    for (const data of bad) {
      const res = await page.request.post("/auth/api-tokens", { data });
      expect(res.status(), JSON.stringify(data)).toBe(400);
    }
    const notJson = await page.request.post("/auth/api-tokens", {
      headers: { "Content-Type": "application/json" },
      data: "{not json",
    });
    expect(notJson.status()).toBe(400);
  });

  test("keeps at most the listed number of tokens", async ({ page }) => {
    const { tokens, limit } = await listApiTokens(page);
    for (let i = tokens.length; i < limit; i++) {
      await createApiToken(page, { name: `${TEST_TOKEN_PREFIX}${i}` });
    }
    const over = await page.request.post("/auth/api-tokens", {
      data: { name: `${TEST_TOKEN_PREFIX}over` },
    });
    expect(over.status()).toBe(400);
    expect((await listApiTokens(page)).tokens).toHaveLength(limit);
  });

  test("accepts the device password with any user name", async () => {
    for (const user of ["api", "", "someone"]) {
      const res = await apiRpc(basicAuth(API_TEST_PASSWORD, user), "ping");
      expect(res.status, `user name "${user}"`).toBe(200);
      expect(res.result).toBe("pong");
    }
    expect((await apiRpc(basicAuth(`${API_TEST_PASSWORD}-wrong`), "ping")).status).toBe(401);
  });

  test("serves a snapshot to a token, the password and a logged-in browser", async ({ page }) => {
    const { token } = await createApiToken(page, { name: `${TEST_TOKEN_PREFIX}view` });

    const unauthorized = await fetch(deviceUrl("/api/video/snapshot"));
    expect(unauthorized.status).toBe(401);

    for (const [who, headers] of [
      ["a token", bearer(token)],
      ["the password", basicAuth(API_TEST_PASSWORD)],
    ] as const) {
      const res = await takeSnapshot(headers);
      expect(res.status, who).toBe(200);
      expect(res.headers.get("content-type")).toBe("image/jpeg");
      const data = new Uint8Array(await res.arrayBuffer());
      expect(data.byteLength, who).toBeGreaterThan(1000);
      const { width, height } = jpegSize(data);
      expect(width).toBeGreaterThan(100);
      expect(height).toBeGreaterThan(100);
    }

    const viaBrowser = await page.request.get("/api/video/snapshot");
    expect([200, 503]).toContain(viaBrowser.status());
  });

  test("scales and crops a snapshot", async ({ page }) => {
    const { token } = await createApiToken(page, { name: `${TEST_TOKEN_PREFIX}view` });
    const auth = bearer(token);

    const scaled = await takeSnapshot(auth, "?width=320");
    expect(scaled.status).toBe(200);
    expect(jpegSize(new Uint8Array(await scaled.arrayBuffer())).width).toBe(320);

    const cropped = await takeSnapshot(auth, "?x=0&y=0&w=320&h=192");
    expect(cropped.status).toBe(200);
    const { width, height } = jpegSize(new Uint8Array(await cropped.arrayBuffer()));
    expect(width / height).toBeCloseTo(320 / 192, 1);
    expect(width).toBeLessThanOrEqual(320);

    const small = await takeSnapshot(auth, "?quality=10");
    const large = await takeSnapshot(auth, "?quality=95");
    expect((await large.arrayBuffer()).byteLength).toBeGreaterThan(
      (await small.arrayBuffer()).byteLength,
    );
  });

  test("rejects invalid snapshot parameters", async ({ page }) => {
    const { token } = await createApiToken(page, { name: `${TEST_TOKEN_PREFIX}view` });
    for (const query of ["?quality=0", "?quality=100", "?w=320", "?h=320", "?width=5", "?x=abc"]) {
      const res = await fetch(deviceUrl(`/api/video/snapshot${query}`), { headers: bearer(token) });
      expect(res.status, query).toBe(400);
    }
  });
});

test.describe("API access without a password", () => {
  test.setTimeout(120_000);

  // The password is set again for whatever runs next.
  test.afterAll(async ({ browser }) => {
    const page = await (await browser.newContext({ baseURL: process.env.JETKVM_URL })).newPage();
    await ensureLocalAuthMode(page, { mode: "password", password: API_TEST_PASSWORD });
    await page.close();
  });

  test("is open, like the web UI", async ({ page }) => {
    await ensureLocalAuthMode(page, { mode: "noPassword" });

    const rpc = await apiRpc({}, "ping");
    expect(rpc.status).toBe(200);
    expect(rpc.result).toBe("pong");

    const res = await takeSnapshot({});
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("image/jpeg");
  });
});
