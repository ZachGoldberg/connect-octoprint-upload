# OctoPrint upload for Prusa Connect

A Prusa Connect extension that lets **PrusaSlicer 2.x** upload and print through a self-hosted
[Prusa Connect](https://github.com/prusa3d/prusa-connect) server, using PrusaSlicer's
**OctoPrint** host type.

PrusaSlicer 2.x's own "Prusa Connect" host type only talks to Prusa's cloud service, so it cannot
reach a Connect server you run yourself
([prusa3d/prusa-connect#7](https://github.com/prusa3d/prusa-connect/issues/7),
[#24](https://github.com/prusa3d/prusa-connect/issues/24)). Prusa has said native support for
local Connect is coming in a PrusaSlicer 3 alpha. Until then, this extension speaks enough of
OctoPrint's API for PrusaSlicer's **Send to printer** and **Upload and Print** buttons to work,
and hands each upload to Connect.

```
PrusaSlicer ──OctoPrint API──▶ octoprint-upload (:17481) ──Connect HTTP API──▶ Prusa Connect ──▶ printer
```

Tested with Prusa Connect 0.1.0-beta.1 and PrusaSlicer 2.9 on a Core One: Upload and Print sent a
3DBenchy and the printer printed it.

This project is not affiliated with or endorsed by Prusa Research.

| | |
| --- | --- |
| Module path | `github.com/zachgoldberg/connect-octoprint-upload` |
| Name | `octoprint-upload` |
| Prusa Connect | 0.1.0-beta.1 or newer |
| PrusaSlicer | 2.x, host type OctoPrint (tested on 2.9) |
| Platforms | linux amd64, arm64, arm; macOS amd64, arm64; Windows amd64 |

## Install

### From the marketplace

Once a release is published to the [Prusa Plugin Marketplace](https://plugin-marketplace.prusa3d.com),
it is offered on **Settings → Extensions**. The extension asks to listen on TCP port 17481 and for
a data folder; approve both.

### By hand, from a release

Download `octoprint-upload-<version>.tar.gz` from the
[releases](https://github.com/ZachGoldberg/connect-octoprint-upload/releases), or build it with
`make dist` (see [Build](#build)). Then, on the Connect server:

```bash
prusa-connect module add github.com/zachgoldberg/connect-octoprint-upload@0.1.0 \
  -from octoprint-upload-0.1.0.tar.gz -approve -approve-listen
```

`-approve` switches the extension on and grants its data folder. `-approve-listen` grants the
port; without it the install says the port is still waiting. To use another port, add
`-listen octoprint=<port>`.

Restart the Connect server afterwards: the command writes the declaration, and a running server
reads it on its next start. (Installing over the API, `POST /api/v1/modules`, applies at once.)

**Docker:** the port is the container's. Publish it as well, `-p 17481:17481` (or under `ports:`
in compose), or nothing outside the container reaches it.

## Create an API credential

PrusaSlicer's "API Key" is a normal Prusa Connect API credential, created by the person who will
upload:

1. In Prusa Connect, open **Settings → API credentials** and choose **New credential**.
2. Name it (for example `PrusaSlicer`), keep the default 90 days or choose up to 364, and pick the
   scope **Operate** (upload files and start prints). **Read** is not enough.
3. Copy the credential it shows. It is shown only once, starts with `pcl_` and is 76
   characters long.

> [!NOTE]
> In Connect 0.1.0-beta.1, creating a credential valid for 365 days fails with a 422. Choose 364
> days or fewer.

## Set up PrusaSlicer

In PrusaSlicer, open the printer settings, click the **Add physical printer** (or edit) icon next
to the printer profile, and fill in:

| field | value |
| --- | --- |
| Host Type | `OctoPrint` |
| Hostname, IP or URL | `http://<connect-server>:17481/<printer>` |
| API Key / Password | the credential from the previous step |
| HTTPS CA File | empty |

`<printer>` names the printer in Connect. It may be:

- the printer's id,
- its name, compared without case, spaces or punctuation (`COREONE 10000-...` matches `coreone10000...`), or
- a unique prefix of its name. Connect names printers after the model and serial, so `/core-one`
  works when there is only one Core One.

With exactly one printer in Connect you may leave it out: `http://<connect-server>:17481`.
Make one physical printer in PrusaSlicer per Connect printer.

Click **Test**. It should say the connection works; the version it reports names the printer it
resolved. Then use **Send to printer** or **Upload and Print** as usual.

What the buttons do:

- **Upload** stores the G-code in Connect's `local` storage, under `slicer-uploads/` (plus any
  folder you type in the upload dialog). It does not copy the file to the printer.
- **Upload and Print** stores it the same way, has Connect send it to the printer's USB drive
  (replacing a file of the same name there), and starts the print. PrusaSlicer's dialog waits
  until the file has reached the printer.

Only `.gcode` and `.bgcode` files are accepted, up to 4 GiB.

## Options

Set in the instance's `config:` block, like any module setting:

| key | default | what it controls |
| --- | --- | --- |
| `connect_url` | `http://127.0.0.1:8080` | Where the extension reaches Connect's API. The default is the server it runs inside; change it if Connect's API listens on another port. |
| `storage` | `local` | The Connect storage uploads are kept in. |
| `folder` | `slicer-uploads` | The folder inside that storage. |
| `printer_storage` | `usb` | The printer's storage a file is sent to for printing. |

```yaml
resources:
  sink:
    octoprint-upload:
      config:
        folder: from-prusaslicer
```

`GET /api/v1/modules/com.github.aux.zachgoldberg.connect-octoprint-upload/status` (with a normal
Connect session or bearer) says whether the upload API is serving, on which port, and why not
when it isn't.

## How it works

The extension serves two OctoPrint routes on its own port, each with an optional printer segment
in front:

| route | does |
| --- | --- |
| `GET [/{printer}]/api/version` | checks the API key and the printer, and answers `{"text": "OctoPrint 1.10.0 (...)"}`. PrusaSlicer only checks that the text starts with `OctoPrint`. |
| `POST [/{printer}]/api/files/local` | takes OctoPrint's multipart upload (`print`, `path`, `file`) |

For each request it calls Connect's public HTTP API with the slicer's key as
`Authorization: Bearer <key>`:

1. `GET /api/v1/devices` to find the printer.
2. The upload is written to the extension's data folder until it has fully arrived, because
   Connect needs the size before the bytes. It is deleted afterwards.
3. `PUT /api/v1/storages/{storage}/content?path={folder}/{file}` to store it.
4. With `print=true`, `POST /api/v1/devices/{id}/print` with
   `{"storage", "source", "path", "replace": true}`, which sends the file to the printer and starts
   it.

### Why its own port

A module's routes normally live under `/api/v1/modules/<namespace>/` on Connect's own port. That
does not work for a slicer upload:

- the host caps a request body through the module mount at 1 MiB, and G-code is often far larger;
- the host authorizes mount routes with Connect's own session or bearer token, and strips
  `Authorization` and `X-Api-Key` before the module sees the request, while PrusaSlicer sends an
  OctoPrint `X-Api-Key`.

So the extension asks for a port in its manifest (`needs.listen`), which the operator grants at
install, and serves the OctoPrint API there.

## Security

- **It acts as the person whose credential the slicer sends.** Every call to Connect carries that
  credential, so Connect decides what may be stored and printed, exactly as in the web app, and
  its audit log names that person. A Read-scoped or revoked credential can't upload.
- **The extension holds nothing.** It declares no permissions, is granted no roles, and stores
  no keys. The only things it asks for are the port and a data folder for uploads in progress.
- **The port is plain HTTP.** Like OctoPrint on a LAN, the API key and the G-code cross the
  network unencrypted. Use it on a network you trust, or put a TLS-terminating proxy in front and
  point PrusaSlicer at that. Don't expose the port to the internet.
- The port answers without a key only for `GET /healthz`. Every other request needs a credential
  Connect accepts.

## Limitations

- Prusa Connect is in beta. Its module SDK and HTTP API may change between releases, and this
  extension may need a new release to follow.
- There is no "upload to the printer without printing": **Upload** keeps the file in Connect's
  storage; only **Upload and Print** copies it to the printer.
- One printer per PrusaSlicer physical printer. The extension lists at most 100 printers.
- Only PrusaSlicer 2.x's OctoPrint upload has been tested. Other slicers that speak the same API
  may work.

## Troubleshooting

PrusaSlicer shows the extension's error text in its error dialog.

| what PrusaSlicer shows | what it means |
| --- | --- |
| `401` `missing API key: create an API credential in Prusa Connect ...` | the API Key field is empty |
| `401` `Prusa Connect does not accept this API key: use an API credential from Settings > API credentials (...)` | the key is wrong, expired or revoked. A common cause is pasting the credential's ID from the list instead of the full credential shown once at creation (it starts with `pcl_`). Create a new one. |
| `403` `Prusa Connect refused this API credential: it needs permission to upload files and operate this printer` | the credential's scope is Read, or the person may not operate that printer |
| `404` `no printer named "..." (printers: ...)` | the printer segment in the host URL matches no printer, or more than one. The message lists the names the credential can see |
| `404` `there are N printers; put one in the host address, e.g. /...` | the host URL has no printer segment and Connect has more than one printer |
| `404` with `ROUTE_NOT_FOUND`, `No operation is published at that path` | PrusaSlicer is talking to Connect itself, usually on `:8080`, not to the extension. Use port `17481` |
| connection refused on `:17481` | the extension isn't serving. Check the `status` route above: the port may not be granted (install again with `-approve-listen`), the server may not have been restarted after `module add`, or, under Docker, the port isn't published |
| `502` `cannot reach Prusa Connect at ...` | `connect_url` is wrong for this deployment |
| `409` `the file is on the printer, but the printer refused to start it: ...` | the upload worked and the printer said no, for example because it is already printing |
| `415` `... is not a file a printer prints` | only `.gcode` and `.bgcode` are accepted |

## Build

Needs Go (the version in `go.mod`) and git. Until the Prusa Connect SDK is published, the build
uses a checkout of Prusa Connect at `CORE_REF` in `.core/` (`go.mod` points there with a
`replace` line); `make core` clones it, or symlink `.core` to a checkout you already have.

```bash
make check     # go vet and the tests
make release   # release/: module.yaml, openapi.json, docs, one binary per platform in module.yaml
make dist      # release/ plus checksums.txt, packed as dist/octoprint-upload-<version>.tar.gz
make install   # installs release/ into a throwaway Connect config in bin/try
```

## Publish

Releases go to the marketplace through
[prusa3d/prusa-connect-marketplace-publish](https://github.com/prusa3d/prusa-connect-marketplace-publish):

1. Raise `version` in `module.yaml` and add its heading to `CHANGELOG.md`.
2. Publish a GitHub release tagged `v<version>`. `release.yml` hands the commit to the
   marketplace's builder, which builds every platform, signs the release and uploads it with the
   source. A person at Prusa reviews it before operators can install it.

`ci.yml` runs the checks and the marketplace's own release checks (a dry run with a throwaway key)
on every push. The one-time setup (signing key, `release` environment, trusted publisher) is
described in [the example extension's README](https://github.com/prusa3d/prusa-connect-extension-example#publish).

## Credits

- The idea and the OctoPrint-emulation approach come from
  [connect-slicer-bridge](https://github.com/bada55fa7a55/connect-slicer-bridge) by
  bada55fa7a55 (MIT), a standalone bridge that does the same job outside Connect.
- Lukas Brzak at Prusa suggested building it as a Connect extension in
  [prusa3d/prusa-connect#7](https://github.com/prusa3d/prusa-connect/issues/7).
- The build and release setup follows
  [prusa3d/prusa-connect-extension-example](https://github.com/prusa3d/prusa-connect-extension-example).

## License

MIT. See [LICENSE](LICENSE).
