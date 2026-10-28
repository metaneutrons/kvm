# API

The JetKVM offers an HTTP API under `/api` for scripts. It controls the
attached computer the way a person at the keyboard would, so treat access to it
like access to the JetKVM itself.

## Enabling access

1. Set a password for the JetKVM (Settings → Access → Local). API tokens are
   only available in password mode.
2. Under Settings → Access → API Tokens, enter a name for the client, tick the
   permissions it needs, optionally set an expiry and select **Create Token**.
   Copy the token right away; it is shown once and stored on the device only as
   a hash.
3. Send it with every request:

   ```
   Authorization: Bearer jkvm_…
   ```

Give every client its own token. Deleting one token cuts off that client and
no other, and the audit log names the token behind each action. You can keep up
to 16 tokens. The list shows when each was created, when it was last used and
from which address (the last use is kept in memory and starts empty after a
reboot), and when it expires. An expired token is refused with 401 until you
delete it.

A token works for `/api` only, not for the web UI, password changes or
settings. Failed attempts count towards the same rate limit as login
attempts.

### Permissions

Every token can view; the others are added one by one, so a client only gets
what it needs:

| Permission | Allows |
|---|---|
| View | Reading the device state. Always included. |
| Input | Keyboard, mouse and the mouse jiggler. |
| Virtual media | Mounting and unmounting images, uploading files. |
| Power | ATX and DC power, Wake-on-LAN and the USB wake-up. |

A call without the permission is refused. Input is close to full control of the
host: anyone who can type on it can usually also do what the other permissions
allow, so give Input only to clients you trust with that.

### Using the password

The device password also works, as HTTP Basic Auth with any user name, for
example `curl -u api:<password> …`. It has all permissions, like the login, and
costs the device more work than a token, so use a token for anything that runs
unattended; the password is meant for a quick call by hand. Wrong passwords
count towards the same rate limit as tokens and logins, and while HTTPS is on,
the password is refused over plain HTTP just like a token. The API sends no
`WWW-Authenticate` header, so a browser does not ask for a password.

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

## HTTP endpoints

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
with `mountWithStorage`.

## Audit log

Every action taken through `/api/rpc` is logged by the `api` component of the
device log at info level, whatever the default log level, with the name of the
token used. High-frequency input, such as single mouse moves, is logged at
debug level. While an agent or script sends input, the web UI shows a notice at
the bottom of the video, until 10 seconds after the last action.
