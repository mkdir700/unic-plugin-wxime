# UniClipboard WeType plugin

`unic.wxime` connects UniClipboard to a WeType clipboard group through the
[WeType Go SDK](https://github.com/mkdir700/wx-ime-sdk). Install it explicitly.
Each backend instance uses its own WeType device identity and private state.
The UniClipboard backend sync policy controls text and image sync in both
directions. The provider receipt means the service accepted the content.

## Install and pair

Use an initialized UniClipboard v2 profile. Version 0.1.3 requires a host
that supports the `image_files_as_images` manifest field. This field is
available starting with Desktop 2.0.0-build.6 and CLI 2.0.0-build.5.

Install the prebuilt plugin for macOS (13+), Linux, or Windows (`amd64` / `arm64`).
The prebuilt binaries statically link OpenSSL `libcrypto`. The installer verifies
the SHA-256 digest.

```sh
unic plugin install https://github.com/mkdir700/unic-plugin-wxime --ref v0.1.3
unic plugin enable unic.wxime
```

To build from source (`--build`), use Go 1.27.3 or later on `PATH`, CGO, a C
compiler, `pkg-config`, and OpenSSL development files with secp128r1 support.
On macOS, install `openssl@3` and `pkg-config` with Homebrew and export
`PKG_CONFIG_PATH="$(brew --prefix openssl@3)/lib/pkgconfig"`. On Debian or
Ubuntu, install `build-essential`, `pkg-config`, and `libssl-dev`.

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
When the host can read a PNG representation of a single copied image file,
the plugin sends that representation as an image. General file transfer is
outside the external-channel profile.

Use **Sync images** under the backend's **Sync policy** in UniClipboard.
Policy changes apply while the service is running. The policy also applies to
existing installations that previously saved a separate plugin image setting.

After upgrading from a version with the separate plugin setting, run
`unic channel pair unic.wxime --resume` once to restore the local authorization
for the existing WeType group. The same command recovers a pairing that
completed at the provider before the local channel was saved.

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
