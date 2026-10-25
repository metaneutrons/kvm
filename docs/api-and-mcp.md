# API and MCP server

The JetKVM offers an HTTP API under `/api` for scripts, and a [Model Context
Protocol](https://modelcontextprotocol.io) (MCP) server at `/api/mcp` that lets
an AI agent see the host's screen and use its keyboard and mouse.

Both control the attached computer the way a person at the keyboard would. Treat
access to them like access to the JetKVM itself.

## Enabling access

1. Set a password for the JetKVM (Settings → Access → Local). The API token is
   only available in password mode.
2. Under Settings → Access → API Token, choose the permissions and select
   **Create Token**. Copy the token right away; it is shown once and stored on
   the device only as a hash.
3. Send it with every request:

   ```
   Authorization: Bearer jkvm_…
   ```

**Replace Token** issues a new token and invalidates the old one, **Delete
Token** switches token access off. The token works for `/api` only, not for the
web UI, password changes or settings. Failed attempts count towards the same
rate limit as login attempts.

A token has one of two permissions:

- **Full control**: screenshots, state, keyboard and mouse input, virtual
  media and power.
- **View only**: screenshots and state. It sends no input, mounts nothing and
  calls no power action. Over MCP it offers only `screenshot`, `get_state` and
  `wait`.

Requests from a browser that is logged in to the web UI also pass, as long as
they come from the JetKVM's own pages. Requests that other websites make the
browser send are rejected.

If the JetKVM runs without a password, `/api` is open to everyone on the
network, like the web UI.

## Use HTTPS

Over plain HTTP the token travels unencrypted, and anyone on the network path
can read and reuse it. Enable HTTPS under Settings → Access → HTTPS Mode
(**Self-signed** or **Custom** with your own certificate) and use `https://`
URLs in every client.

HTTP on port 80 stays available after HTTPS is enabled. While HTTPS is enabled,
the device refuses the token over plain HTTP with 403, so a client configured
with an `http://` URL fails at once. The token has then crossed the network
unencrypted once, so replace it if that network is not trusted.

With a self-signed certificate, clients have to trust it. For Claude Code and
other Node.js clients, export the certificate from the browser and point
`NODE_EXTRA_CA_CERTS` at the file, or use a certificate from a CA the client
already trusts.

## MCP server

`/api/mcp` speaks MCP over Streamable HTTP in stateless mode: every request is
answered on its own, no session is kept between calls.

### Connecting Claude Code

```bash
claude mcp add --transport http jetkvm https://<your-jetkvm>/api/mcp --header "Authorization: Bearer jkvm_…"
```

Replace `<your-jetkvm>` with the device's address, for example its IP
address. Other MCP clients connect the same way: the endpoint URL plus the
`Authorization` header.

### Screen size and coordinates

The agent works with one screen size per connection. By default screenshots are
scaled to at most 1280 pixels wide (1280×720 for a 1080p host), which keeps them
small and below the sizes at which clients start to scale images themselves. All
mouse tools take coordinates in that size, so what the model sees and where it
clicks agree for any video resolution. Fractions such as `412.5` are allowed.

A client that handles large images can choose another size in the URL:

| URL | Screen the agent works with |
|---|---|
| `/api/mcp` | at most 1280 pixels wide |
| `/api/mcp?screen_width=1600` | at most 1600 pixels wide (320–7680) |
| `/api/mcp?screen_width=native` | the video resolution |

To read small text, the agent takes a screenshot of a region. It returns that
part of the screen at up to the real resolution, and states how a point in the
zoomed image maps back to screen coordinates.

### Tools

| Tool | What it does |
|---|---|
| `screenshot` | The screen as JPEG, or a zoomed region of it. Starts video capture if no browser session runs it, and keeps it running for 30 seconds. |
| `move_mouse`, `click`, `drag`, `scroll` | Mouse input at screen coordinates. Needs the absolute mouse, which is on by default. |
| `press_keys` | Presses keys together, e.g. `["ControlLeft", "KeyC"]`, and holds them for up to 30 seconds. Keys are named after the physical US position, as in `KeyboardEvent.code`; `Ctrl`, `Shift`, `Alt`, `Cmd`, `Win` and `Esc` work as short forms. |
| `type_text` | Types up to 4096 characters through a keyboard layout. |
| `get_state` | Video signal, USB, keyboard layout and LEDs, virtual media, power. |
| `wait` | Waits a number of seconds, or until the screen changes or settles. |
| `wake_host` | Sends a USB remote wakeup to a sleeping host. |
| `power` | Power button, reset or power supply, with the ATX or DC extension. |
| `mount_media`, `unmount_media` | Attaches an image from a URL or the JetKVM's storage as a USB CD-ROM or disk. |

Every action answers with a short confirmation, and errors are returned to the
model as text so it can correct itself.

Points to know:

- **Keyboard layout.** `type_text` maps characters to keys through the layout set
  in the JetKVM (Settings → Keyboard), or the `layout` given in the call. It has
  to match the layout of the host. For a Mac use a macOS layout such as
  "Deutsch (macOS)" (`de-DE-macos`): macOS places many symbols differently from
  Windows and Linux. Characters the layout cannot type are reported, and nothing
  is typed.
- **Typing speed.** Each character takes about 45 milliseconds, so 1000
  characters take about 45 seconds. Clients with a short tool timeout need
  shorter texts.
- **Scroll direction.** Positive `delta_y` turns the wheel down. macOS reverses
  the direction with natural scrolling, its default.
- **Waking the host.** `wake_host` wakes a host from sleep if it allows USB
  wakeup. A display that only went dark wakes from a key press instead, for
  example `press_keys` with `["ShiftLeft"]`.
- **Shared input.** People using the web UI and the agent type into the same
  keyboard and mouse. Keyboard calls from the agent run one at a time. While an
  agent or script sends input, the web UI shows a notice at the bottom of the
  video, until 10 seconds after the last action.
- **Seeing the result.** The input tools take `screenshot: true` and then return
  a screenshot once the screen has settled, which saves a separate call. The
  `wait` tool waits a number of seconds, or until the screen changes or stops
  changing, for example while an app starts.
- **Holding keys.** `press_keys` holds keys for up to 30 seconds (`hold_ms`),
  for example Option through a Mac's startup to reach the boot picker.

## HTTP endpoints

### `GET /api/video/snapshot`

Returns the current frame as JPEG and starts video capture if needed. Optional
query parameters:

| Parameter | Meaning |
|---|---|
| `quality` | JPEG quality, 1–99, default 80 |
| `x`, `y`, `w`, `h` | Region in video pixels; `w` and `h` together |
| `width` | Scale down to this width, keeping the aspect ratio |

Answers 503 with a `reason` when the host sends no video, for example while its
display sleeps.

### `POST /api/rpc`

Calls a JSON-RPC 2.0 method, for example:

```bash
curl -H "Authorization: Bearer jkvm_…" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"getVideoState"}' https://<your-jetkvm>/api/rpc
```

Allowed are state queries, keyboard and mouse input, virtual media, Wake-on-LAN
and power. Device administration (network, updates, SSH keys, developer mode,
factory reset) is left to the web UI, so a leaked token cannot take over the
JetKVM itself.

### `POST /api/storage/upload`

Uploads an image in two steps: `startStorageFileUpload` (`filename`, `size`)
over `/api/rpc` returns an `uploadId` as `dataChannel`; then send the file as the
request body to `/api/storage/upload?uploadId=…`. Afterwards it can be mounted
with `mountWithStorage` or the `mount_media` tool.

## Audit log

Every action taken through `/api/rpc` or the MCP server, and every screenshot an
agent takes, is logged by the `api` component of the device log at info level,
whatever the default log level. Typed text is not logged, only its length.
High-frequency input over `/api/rpc`, such as single mouse moves, is logged at
debug level.
