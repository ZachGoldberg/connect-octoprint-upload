# Changelog

What an operator sees change when they upgrade this module, newest release
first. `version:` in `module.yaml` is the newest heading below.

## 0.1.1 - 2026-11-19

### Changed

- `module.yaml` declares the module's one outside connection under `egress`: the process calls
  the Prusa Connect API at `connect_url` and sends it the upload and the person's API credential,
  so the operator sees that at install. The code is unchanged.
- The README's Security section says the upload port is plain HTTP on every interface.

## 0.1.0 - 2026-11-18

### Added

- An OctoPrint-compatible upload API on its own TCP port, 17481 (`needs.listen`, name `octoprint`),
  so PrusaSlicer 2.x's OctoPrint host type can upload to and print through a self-hosted Prusa Connect.
- `GET [/{printer}]/api/version` answers as OctoPrint, so PrusaSlicer's Test button passes.
- `POST [/{printer}]/api/files/local` stores the upload in Prusa Connect and, with `print=true`,
  sends it to the printer (replacing a file of the same name there) and starts it.
- The printer is the first path segment: its id, its name compared without case or punctuation,
  or a unique prefix of the name (`/core-one`). With one printer, it may be left out.
- The slicer's API key is a Prusa Connect API credential; every call to Connect carries it, so
  Connect authorizes each upload and print as that person. The module holds no permissions.
- The settings `connect_url`, `storage`, `folder` and `printer_storage`.
- `GET /status` under the module's mount says whether the upload API is serving, on which port,
  and why not when it is not.
