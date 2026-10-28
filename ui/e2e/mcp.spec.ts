import { expect, test } from "@playwright/test";

import {
  API_TEST_PASSWORD,
  TEST_TOKEN_PREFIX,
  apiRpc,
  basicAuth,
  bearer,
  createApiToken,
  deleteTestTokens,
  deviceUrl,
  ensureLocalAuthMode,
  jpegSize,
  listApiTokens,
  mcpClient,
  setMcpEnabled,
  toolText,
} from "./helpers";
import type { Headers } from "./helpers";

const VIEW_TOOLS = ["get_state", "list_keyboard_layouts", "list_media", "screenshot", "wait"];
const INPUT_TOOLS = [
  "click",
  "drag",
  "move_mouse",
  "press_keys",
  "scroll",
  "set_jiggler",
  "set_keyboard_layout",
  "type_text",
];
const MEDIA_TOOLS = ["mount_media", "unmount_media"];
const POWER_TOOLS = ["control_power", "wake_host"];

test.describe("MCP server", () => {
  test.setTimeout(120_000);
  test.describe.configure({ mode: "serial" });

  test.beforeEach(async ({ page }) => {
    await ensureLocalAuthMode(page, { mode: "password", password: API_TEST_PASSWORD });
    await setMcpEnabled(page, true);
  });

  test.afterEach(async ({ page }) => {
    await setMcpEnabled(page, true);
    await deleteTestTokens(page);
  });

  async function clientWith(page: import("@playwright/test").Page, permissions: string[]) {
    const { token } = await createApiToken(page, {
      name: `${TEST_TOKEN_PREFIX}mcp ${permissions.join("+") || "view"}`,
      permissions,
    });
    const client = mcpClient(bearer(token));
    const init = await client.initialize();
    expect(init.status, JSON.stringify(init)).toBe(200);
    return { client, auth: bearer(token) };
  }

  test("lists the tools a token's permissions cover", async ({ page }) => {
    const cases: [string[], string[]][] = [
      [[], VIEW_TOOLS],
      [["input"], [...VIEW_TOOLS, ...INPUT_TOOLS]],
      [["media"], [...VIEW_TOOLS, ...MEDIA_TOOLS]],
      [["power"], [...VIEW_TOOLS, ...POWER_TOOLS]],
      [
        ["input", "media", "power"],
        [...VIEW_TOOLS, ...INPUT_TOOLS, ...MEDIA_TOOLS, ...POWER_TOOLS],
      ],
    ];
    for (const [permissions, expected] of cases) {
      const { client } = await clientWith(page, permissions);
      const names = (await client.tools()).map(t => t.name).sort();
      expect(names, permissions.join("+") || "view").toEqual([...expected].sort());
    }
  });

  test("describes every tool the same way", async ({ page }) => {
    const { client } = await clientWith(page, ["input", "media", "power"]);
    for (const tool of await client.tools()) {
      expect(tool.title, `${tool.name} has a title`).toBeTruthy();
      expect(tool.description, `${tool.name} has a description`).toBeTruthy();
      expect(tool.annotations, `${tool.name} has annotations`).toBeTruthy();
      if (tool.name.startsWith("get_") || tool.name.startsWith("list_")) {
        expect(tool.annotations?.readOnlyHint, `${tool.name} only reads`).toBe(true);
      }
      if (tool.name.startsWith("set_")) {
        expect(tool.annotations?.idempotentHint, `${tool.name} is idempotent`).toBe(true);
        expect(tool.annotations?.destructiveHint, `${tool.name} is not destructive`).toBe(false);
      }
    }
    const byName = Object.fromEntries((await client.tools()).map(t => [t.name, t]));
    expect(byName["click"].inputSchema?.properties?.button.enum).toEqual([
      "left",
      "right",
      "middle",
    ]);
    expect(byName["wait"].inputSchema?.properties?.until.enum).toEqual(["change", "stable"]);
    expect(byName["set_jiggler"].inputSchema?.required).toContain("enabled");
    expect(byName["set_keyboard_layout"].inputSchema?.required).toContain("layout");
  });

  test("takes a screenshot", async ({ page }) => {
    const { client } = await clientWith(page, []);
    const result = await client.call("screenshot", { quality: 60 });
    test.skip(
      result.isError === true && /no video signal/.test(toolText(result)),
      `the host sends no video: ${toolText(result)}`,
    );
    expect(result.isError, toolText(result)).toBeFalsy();

    const image = result.content.find(c => c.type === "image");
    expect(image?.mimeType).toBe("image/jpeg");
    const { width, height } = jpegSize(Buffer.from(image?.data ?? "", "base64"));
    expect(toolText(result)).toContain(`${width}x${height}`);

    const zoom = await client.call("screenshot", {
      region: { x: 0, y: 0, width: 200, height: 120 },
    });
    expect(zoom.isError, toolText(zoom)).toBeFalsy();
    expect(zoom.content.some(c => c.type === "image")).toBe(true);
  });

  test("reports the device state", async ({ page }) => {
    const { client } = await clientWith(page, []);
    const result = await client.call("get_state");
    expect(result.isError, toolText(result)).toBeFalsy();
    const state = result.structuredContent as Record<string, unknown>;
    for (const key of ["video", "usb", "keyboard_layout", "jiggler"]) {
      expect(state, `get_state has ${key}`).toHaveProperty(key);
    }
    expect(state).not.toHaveProperty("available_layouts");
    expect(typeof (state.jiggler as { enabled: boolean }).enabled).toBe("boolean");
  });

  test("lists keyboard layouts and media", async ({ page }) => {
    const { client, auth } = await clientWith(page, []);

    const layouts = (await client.call("list_keyboard_layouts")).structuredContent as {
      current: string;
      layouts: { code: string; name: string }[];
    };
    const codes = layouts.layouts.map(l => l.code);
    for (const code of ["en-US", "de-DE", "de-DE-macos"]) expect(codes).toContain(code);
    expect(layouts.layouts.every(l => l.name)).toBe(true);
    expect(layouts.current).toBe((await apiRpc(auth, "getKeyboardLayout")).result);

    const media = (await client.call("list_media")).structuredContent as {
      files: { name: string; size: number }[];
      bytes_free: number;
    };
    expect(Array.isArray(media.files)).toBe(true);
    expect(media.bytes_free).toBeGreaterThan(0);
  });

  test("sets the keyboard layout and puts it back", async ({ page }) => {
    const { client, auth } = await clientWith(page, ["input"]);
    const original = (await apiRpc(auth, "getKeyboardLayout")).result as string;
    const other = original === "de-DE-macos" ? "en-US" : "de-DE-macos";
    try {
      const result = await client.call("set_keyboard_layout", { layout: other });
      expect(result.isError, toolText(result)).toBeFalsy();
      expect((await apiRpc(auth, "getKeyboardLayout")).result).toBe(other);

      // Setting it again changes nothing.
      expect((await client.call("set_keyboard_layout", { layout: other })).isError).toBeFalsy();

      const unknown = await client.call("set_keyboard_layout", { layout: "xx-XX" });
      expect(unknown.isError).toBe(true);
      expect(toolText(unknown)).toContain("unknown layout");
      expect((await apiRpc(auth, "getKeyboardLayout")).result).toBe(other);
    } finally {
      await client.call("set_keyboard_layout", { layout: original });
    }
    expect((await apiRpc(auth, "getKeyboardLayout")).result).toBe(original);
  });

  test("turns the jiggler on and off and puts it back", async ({ page }) => {
    const { client } = await clientWith(page, ["input"]);
    const enabled = async () =>
      ((await client.call("get_state")).structuredContent as { jiggler: { enabled: boolean } })
        .jiggler.enabled;
    const original = await enabled();
    try {
      expect((await client.call("set_jiggler", { enabled: !original })).isError).toBeFalsy();
      expect(await enabled()).toBe(!original);
    } finally {
      await client.call("set_jiggler", { enabled: original });
    }
    expect(await enabled()).toBe(original);
  });

  test("returns invalid arguments as errors that name the choices", async ({ page }) => {
    const { client } = await clientWith(page, ["input", "media", "power"]);
    const cases: [string, Record<string, unknown>, RegExp][] = [
      ["click", { x: 1, y: 1, button: "fourth" }, /left.*right.*middle/],
      ["wait", { seconds: 1, until: "never" }, /change.*stable/],
      ["mount_media", { url: "http://example.invalid/a.iso", mode: "floppy" }, /CDROM.*Disk/],
      ["control_power", { action: "explode" }, /power-short/],
      ["set_jiggler", {}, /enabled/],
    ];
    for (const [tool, args, expected] of cases) {
      const result = await client.call(tool, args);
      expect(result.isError, `${tool} ${JSON.stringify(args)}`).toBe(true);
      expect(toolText(result)).toMatch(expected);
    }
  });

  test("refuses a tool the token does not cover", async ({ page }) => {
    const { client } = await clientWith(page, []);
    for (const name of ["click", "type_text", "set_jiggler", "control_power", "mount_media"]) {
      const res = await client.request("tools/call", { name, arguments: {} });
      const refused = res.error !== undefined || (res.result as { isError?: boolean }).isError;
      expect(refused, `${name} with a view-only token`).toBeTruthy();
    }
  });

  test("answers 401 without credentials and takes the password", async () => {
    const none = await mcpClient({}).request("tools/list");
    expect(none.status).toBe(401);

    const withPassword = mcpClient(basicAuth(API_TEST_PASSWORD));
    expect((await withPassword.tools()).length).toBeGreaterThan(0);
  });

  test("can be turned off for everyone and on again", async ({ page }) => {
    const { client, auth } = await clientWith(page, []);
    expect((await client.tools()).length).toBeGreaterThan(0);

    await setMcpEnabled(page, false);
    expect((await listApiTokens(page)).mcp_enabled).toBe(false);
    try {
      expect((await client.request("tools/list")).status).toBe(404);
      expect((await mcpClient(basicAuth(API_TEST_PASSWORD)).request("tools/list")).status).toBe(
        404,
      );
      // Without credentials the answer does not reveal the setting.
      expect((await mcpClient({}).request("tools/list")).status).toBe(401);
      // The API for scripts keeps working.
      expect((await apiRpc(auth, "ping")).status).toBe(200);
      const snapshotUrl = deviceUrl("/api/video/snapshot?width=64");
      expect((await fetch(snapshotUrl, { headers: auth as Headers })).status).not.toBe(404);
    } finally {
      await setMcpEnabled(page, true);
    }
    expect((await client.tools()).length).toBeGreaterThan(0);
  });
});
