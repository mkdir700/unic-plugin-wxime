# UniClipboard WeType plugin

`unic.wxime` connects UniClipboard to a WeType clipboard group through the
[WeType Go SDK](https://github.com/mkdir700/wx-ime-sdk). Install it explicitly.
Each backend instance uses its own WeType device identity and private state.
Text is enabled by default. Image sync is opt-in. The provider receipt means
the service accepted the content.

## Install and pair

Use a `unic` and `unicd` build from the UniClipboard
[external channel pairing branch](https://github.com/UniClipboard/UniClipboard/tree/feat/external-channel-pairing)
or a later compatible release. Use an initialized UniClipboard profile.
Source installation needs Go 1.27.3 or later on `PATH`, CGO, a C compiler,
`pkg-config`, and OpenSSL development files with secp128r1 support.
Use `GOTOOLCHAIN=local go version` to check the toolchain used by the installer.

On macOS, install `openssl@3` and `pkg-config` with Homebrew and export the
OpenSSL metadata path before starting the service or invoking the installer.
On Debian or Ubuntu, install `build-essential`, `pkg-config`, and `libssl-dev`.

```sh
export PKG_CONFIG_PATH="$(brew --prefix openssl@3)/lib/pkgconfig"
unic plugin install https://github.com/mkdir700/unic-plugin-wxime --build
unic plugin enable unic.wxime
```

Restart the service after enabling the plugin. In WeType, generate a pairing
code for an authorized group. Run the following command. Enter the code at the
prompt and confirm the pairing in WeType when prompted.

```sh
unic channel pair unic.wxime
unic status
```

Pairing enables clipboard sync with the WeType group. Keep `unicd` running,
either directly or through Desktop, to receive updates. Exercise the pairing
only on accounts and devices you are authorized to use.

The plugin supports the latest clipboard value from WeType. It receives UTF-8
text and, when enabled, PNG and JPEG images. It sends UTF-8 text up to 1 MiB
and PNG images up to 16 MiB, subject to the Engine and provider limits.
File transfer is outside the external-channel profile.

Enable `images` in the plugin settings and restart the service to opt in.
Engine invalidates the old local channel after a plugin settings change.
Run `unic channel pair unic.wxime --resume` to authorize the existing WeType
group again. The same command also recovers a pairing that completed at the
provider before the local channel was saved.

Use the existing external-channel controls to pause the channel or disable
clipboard auto-apply. Removing the local channel removes its UniClipboard
authorization. To remove the WeType device from the group, use WeType.
Use a new UniClipboard backend instance to pair with a different group.

## Verify

```sh
mkdir -p artifacts
WXIME_E2E_RECEIPT="$PWD/artifacts/process.json" go test -race ./... -count=1
go build -mod=readonly ./cmd/unic-plugin-wxime
```

The process test drives the public Engine API and the real streamed plugin RPC
through a child process with a local provider fixture. The pinned SDK has a
separate end-to-end suite for its WeType HTTP and file transports.
