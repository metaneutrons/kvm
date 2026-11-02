import { test, expect } from "@playwright/test";

import { ensureLocalAuthMode, getDeviceHost } from "./helpers";

// Set JETKVM_PASSWORD to the device's own password so the tests keep it.
const TEST_PASSWORD = process.env.JETKVM_PASSWORD ?? "TestPassword123";

function basicAuthHeader(password: string): string {
  return `Basic ${Buffer.from(`api:${password}`).toString("base64")}`;
}

// Width and height from the start-of-frame marker of a JPEG, or the header of a PNG.
function imageSize(data: Uint8Array): { width: number; height: number } {
  if (data[0] === 0x89 && data[1] === 0x50) {
    const view = new DataView(data.buffer, data.byteOffset);
    return { width: view.getUint32(16), height: view.getUint32(20) };
  }
  let i = 2;
  while (i + 9 < data.length) {
    if (data[i] !== 0xff) throw new Error("not a JPEG");
    const marker = data[i + 1];
    if (marker >= 0xc0 && marker <= 0xcf && ![0xc4, 0xc8, 0xcc].includes(marker)) {
      return { height: (data[i + 5] << 8) | data[i + 6], width: (data[i + 7] << 8) | data[i + 8] };
    }
    i += 2 + ((data[i + 2] << 8) | data[i + 3]);
  }
  throw new Error("no frame header in the JPEG");
}

const SCREENSHOT_PATHS = ["/screenshot.jpg", "/screenshot.png"] as const;

function expectedContentType(path: string): string {
  return path.endsWith(".png") ? "image/png" : "image/jpeg";
}

test.describe("Screenshot API", () => {
  test.setTimeout(60000);

  test.describe("password mode", () => {
    test.beforeEach(async ({ page }) => {
      // Leaves `page`'s browser context with a valid session cookie too, so
      // page.request below is already authenticated like a logged-in tab.
      await ensureLocalAuthMode(page, { mode: "password", password: TEST_PASSWORD });
    });

    for (const path of SCREENSHOT_PATHS) {
      test(`${path} rejects requests with no credentials`, async () => {
        const res = await fetch(`http://${getDeviceHost()}${path}`);
        expect(res.status).toBe(401);
      });

      test(`${path} rejects a bogus session cookie`, async () => {
        const res = await fetch(`http://${getDeviceHost()}${path}`, {
          headers: { Cookie: "authToken=not-a-real-token" },
        });
        expect(res.status).toBe(401);
      });

      test(`${path} succeeds with HTTP Basic Auth`, async () => {
        const res = await fetch(`http://${getDeviceHost()}${path}`, {
          headers: { Authorization: basicAuthHeader(TEST_PASSWORD) },
        });
        expect(res.status).toBe(200);
        expect(res.headers.get("content-type")).toBe(expectedContentType(path));

        const bytes = await res.arrayBuffer();
        expect(bytes.byteLength).toBeGreaterThan(1000);
      });

      test(`${path} succeeds with a logged-in browser session`, async ({ page }) => {
        const res = await page.request.get(path);
        expect(res.status()).toBe(200);
        expect(res.headers()["content-type"]).toBe(expectedContentType(path));

        const body = await res.body();
        expect(body.byteLength).toBeGreaterThan(1000);
      });
    }
  });

  test.describe("options", () => {
    const get = async (path: string) =>
      fetch(`http://${getDeviceHost()}${path}`, {
        headers: { Authorization: basicAuthHeader(TEST_PASSWORD) },
      });

    test.beforeEach(async ({ page }) => {
      await ensureLocalAuthMode(page, { mode: "password", password: TEST_PASSWORD });
    });

    for (const ext of ["jpg", "png"]) {
      test(`/screenshot.${ext} scales to a width and keeps the aspect ratio`, async () => {
        const full = await get(`/screenshot.${ext}`);
        expect(full.status).toBe(200);
        const whole = imageSize(new Uint8Array(await full.arrayBuffer()));

        const res = await get(`/screenshot.${ext}?width=320`);
        expect(res.status).toBe(200);
        const { width, height } = imageSize(new Uint8Array(await res.arrayBuffer()));
        expect(width).toBe(320);
        expect(width / height).toBeCloseTo(whole.width / whole.height, 1);
      });

      test(`/screenshot.${ext} crops a region`, async () => {
        const res = await get(`/screenshot.${ext}?x=0&y=0&w=320&h=192`);
        expect(res.status).toBe(200);
        const { width, height } = imageSize(new Uint8Array(await res.arrayBuffer()));
        expect(width / height).toBeCloseTo(320 / 192, 1);
        expect(width).toBeLessThanOrEqual(320);
      });
    }

    test("a higher quality makes a larger JPEG", async () => {
      const small = await get("/screenshot.jpg?quality=10");
      const large = await get("/screenshot.jpg?quality=95");
      expect((await large.arrayBuffer()).byteLength).toBeGreaterThan(
        (await small.arrayBuffer()).byteLength,
      );
    });

    test("rejects options it cannot use", async () => {
      for (const query of [
        "quality=0",
        "quality=100",
        "w=320",
        "h=320",
        "width=5",
        "x=abc",
        "x=9000&y=0&w=320&h=320",
      ]) {
        const res = await get(`/screenshot.jpg?${query}`);
        expect(res.status, query).toBe(400);
      }
    });
  });

  test("succeeds with no credentials in noPassword mode", async ({ page }) => {
    await ensureLocalAuthMode(page, { mode: "noPassword" });

    const res = await fetch(`http://${getDeviceHost()}/screenshot.jpg`);
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("image/jpeg");
  });
});
