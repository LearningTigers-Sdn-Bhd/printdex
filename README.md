# PrintDex

Badge and sticker printing station for events. An operator scans a guest's QR
code; PrintDex looks the ticket up on EventzFlow, checks the guest in, and
prints a badge — in about the time it takes the two network calls to return.

- [How a scan works](#how-a-scan-works)
- [Badge design](#badge-design)
- [Tech stack](#tech-stack)
- [Install](#install)
- [Updates](#updates)
- [Settings](#settings)
- [Local API](#local-api)
- [Develop](#develop)
- [Release](#release)
- [Status](#status)

## How a scan works

```text
QR scan ─▶ POST /scan/{id}
            ├─ 1. look the ticket up on EventzFlow
            ├─ 2. check the guest in
            ├─ 3. draw the badge as a 1-bit bitmap at the printer's resolution
            └─ 4. hand the bitmap to the printer
```

The reply carries one of four outcomes, which the dashboard shows:

| Status | Meaning |
| :-- | :-- |
| `printed` | Checked in, badge sent to the printer |
| `no_badge` | Checked in; this ticket type is on the "skip printing" list |
| `print_failed` | Checked in, but the printer refused. The operator can reprint |
| `already` | Checked in earlier. Nothing printed; who and when are shown |

A printer fault is reported as an outcome, not an error: the guest is already
checked in, and scanning again would only say "already checked in".

Where the time goes is returned with every scan (`timing_ms`: lookup,
check-in, print, total) and logged, so a slow station can be diagnosed from
the log instead of guessed at.

### Why it is fast

- **One kept-alive connection** to EventzFlow, opened at start-up. A scan
  costs two round trips and no TLS handshake.
- **No PDF.** The badge is drawn straight to a bitmap at the printer's own
  resolution — no PDF step, no temp files, no resampling. Drawing plus
  encoding takes about 5 ms.
- **One job at a time**, in scan order, so badges never come out shuffled.

## Badge design

A badge is a vertical stack of elements, centered on the paper. Which
elements appear, in what order, and on what paper is the **layout**, edited in
the dashboard and saved with the station.

```text
┌──────────────────────────┐
│                          │
│    MOHAMED RAFI          │   name      auto-sized, up to 2 lines
│    BIN ABDULLAH          │
│       DELEGATE           │   role      the ticket type
│  LEARNING TIGERS SDN BHD │   company   supporting text
│          ▄▄▄▄▄           │
│          █▀▀▀█           │   qr        encodes the ticket's public id
│          ▀▀▀▀▀           │
└──────────────────────────┘
        100 × 80 mm (default paper)
```

### Elements

| Element | Prints | Bold by default |
| :-- | :-- | :-- |
| `name` | Guest name | yes |
| `role` | Ticket type. Conference passes read `DELEGATE` | yes |
| `ticket_role` | The ticket's role field | no |
| `ticket_group` | Ticket group (room, track…) | no |
| `company` | Company or organisation | no |
| `title` | Job title | no |
| `country` | Country | no |
| `table_no` | `TABLE 12` | yes |
| `qr` | QR code of the ticket id | — |
| `custom_*` | Up to 6 custom fields you define, fed from the event's registration fields (matched by key, else by label) | no |

An element with nothing to print for a guest is skipped, so a guest without a
title simply has a shorter badge. All text is set in capitals.

### How text is sized

The layout engine works in three passes (`internal/badge/layout.go`):

1. **Measure.** Each element gets its natural size and line breaks.
   - Name and ticket type take the *largest* size at which they fit on one
     line, or on two evenly balanced lines — big type on two lines reads
     better than small type on one. Name tries 26 pt down to 12 pt; ticket
     type and table number 22 pt down to 12 pt.
   - Supporting text starts at 13 pt and steps down to 11 pt, then 9 pt, as
     it needs more lines.
2. **Grow.** A sparse badge (few fields) scales its text up in 5 % steps
   until the stack fills 80 % of the paper height. Growth is capped per
   element — name 44 pt, ticket type 36 pt, table 30 pt, others 20 pt — so a
   two-field badge never prints "VVIP" at poster size and the name stays the
   biggest thing on the badge. No line may use more than 92 % of the width.
3. **Shrink.** A crowded badge scales the QR code down first, to a floor of
   0.55 in (smaller stops scanning reliably), then the largest text a point at
   a time, to a floor of 8 pt. Line breaks are kept, so every step makes the
   stack shorter. If it still doesn't fit it prints clipped rather than
   vanish.

The finished stack is centered above a small bottom margin.

### Controls in the layout editor

| Control | Range | Effect |
| :-- | :-- | :-- |
| Paper size | 20–500 mm each way | Width and height of the label |
| Element order | — | Top-to-bottom order of the stack |
| Size multiplier | 0.5× – 2× per element | Scales one element's type (or the QR) |
| Bold | per element | Overrides the default weight |
| Nudge | ±10 mm per element | Moves one element without moving the rest |
| Vertical offset | ±80 mm | Slides the whole stack down (+) or up (−) |
| Presets | up to 20 | Named layouts, one click to switch between events |
| Skip printing | per ticket type | Check these types in without printing |
| Direct thermal | on/off | Thickens strokes by a pixel for rough direct-thermal stock |

The preview in the dashboard uses the same renderer and resolution as the
printer, so what you see is what prints, including unsaved edits.

### Print quality

Label printers are 1-bit, and a grey pixel is halftoned into a visible dot
pattern. So the badge is drawn in pure black and white: after drawing, every
pixel darker than 220/255 becomes black. This keeps thin strokes solid. If
text prints too heavy on some stock, lower `inkThreshold` in
`internal/badge/render.go`.

The QR code is drawn from whole-pixel modules with a two-module quiet zone,
so module edges are crisp at the printer's resolution.

Printer resolution: Windows reads it from the driver. On macOS and Linux it
is `PRINT_DPI` (default 203, the common thermal-label head); set 300 for a
300 dpi head.

## Tech stack

| Layer | Choice | Why |
| :-- | :-- | :-- |
| Language | Go 1.27 | One static binary, fast start-up, no runtime to install or break |
| Window | [Wails v2](https://wails.io): WebView2 on Windows, WKWebView on macOS | Native window around the dashboard, small binary |
| Dashboard | Plain HTML, CSS and JS, embedded in the binary (`go:embed`) | No build step, works offline at venues |
| Badge text | [Liberation Sans](https://github.com/go-fonts/liberation) via `golang.org/x/image` | Metric-compatible with Helvetica, so badge text widths match Helvetica layouts |
| QR codes | [`skip2/go-qrcode`](https://github.com/skip2/go-qrcode), error correction M | Tolerates scuffed stickers |
| Windows printing | GDI through `syscall`, no cgo | Cross-compiles from any machine; bitmap sent to the spooler directly |
| macOS / Linux printing | CUPS `lp`, 1-bit PNG on stdin | No temp files |
| Backend | EventzFlow REST API over a pooled `net/http` client | Kept-alive TLS |
| Installer | NSIS (Windows, per-user), `.dmg` (macOS) | Same flow as RfiDex |
| Updates | Ed25519-signed binaries listed in `latest.json` on GitHub Releases | Seconds to apply, no admin rights |

Direct dependencies are limited to the Wails runtime, the Go image/font
packages, the Liberation fonts and the QR library.

### Layout of the code

```text
main.go                 wiring and release commands (keygen, sign)
window_desktop.go       native window (release builds)
window_headless.go      server only (go run .)
internal/
  config/               settings and badge layout; the config.json store
  ticket/               what a badge can show; lenient ticket parsing
  eventzflow/           backend client: lookup, check-in, search
  badge/
    layout.go           sizing rules: measure, grow, shrink
    render.go           fonts, drawing, 1-bit PNG
  printer/              printer choice; GDI (Windows) and CUPS spooling
  server/               local HTTP API; dashboard in ui/
  update/               self-update and release signing
```

Dependencies point one way: `server` → `eventzflow`, `badge`, `printer`,
`update` → `config`, `ticket`. Nothing in `internal/` keeps global state;
`main.go` builds the pieces and hands them to the server.

## Install

**Windows** — download `PrintDex_<version>_x64-setup.exe` from the latest
release and run it. It installs for the current user (no admin prompt) and
fetches WebView2 if the PC doesn't have it. SmartScreen: **More info → Run
anyway** (the installer is not code-signed).

**macOS** — open `PrintDex_<version>_universal.dmg` and drag PrintDex to
Applications. First launch: right-click → **Open** (the app is not notarized).

Then open the dashboard, enter the EventzFlow address, the event ID or slug (either works) and the
event's API key, and press **Test print**.

Stations upgrading from the earlier `event-printer` app keep their event
connection and layouts: PrintDex adopts its `config.json` on first start. Close
`event-printer` first; both use port 8000.

## Updates

Press **Check for updates** in the dashboard. PrintDex downloads the new
build, verifies its Ed25519 signature against the public key baked into the
app, swaps its own executable and relaunches. No installer runs and no admin
rights are needed. A build with a bad or missing signature is never installed.

Builds made without a signing key (including `go run`) cannot self-update;
they say so instead of failing.

## Settings

### Where things are stored

| Host | Config file |
| :-- | :-- |
| Windows | `%APPDATA%\printdex\config.json` |
| macOS | `~/Library/Application Support/printdex/config.json` |

The file holds the EventzFlow API key in plain text, so it is written
readable by its owner only. Use a dedicated OS account on shared PCs.

### Environment variables

Set them in the environment, or in a `.env` file beside the executable.

| Name | Default | What |
| :-- | :-- | :-- |
| `PRINTER_NAME` | auto | Printer to use. Auto: the only printer, else the system default |
| `PORT` | `8000` | Local API port. RfiDex expects 8000 |
| `HOST` | `127.0.0.1` | `0.0.0.0` exposes the station to the venue network. The API has no login |
| `PRINT_DPI` | `203` | macOS/Linux: the printer's resolution |
| `HEADLESS` | | Any value: run without a window |
| `EVENTZ_BACKEND_URL`, `EVENTZ_EVENT_SLUG`, `EVENTZ_API_KEY` | | Preset the event connection on a fresh station |

## Local API

The dashboard and RfiDex use this. It answers on `127.0.0.1:8000` only, and
rejects state-changing requests from other websites.

| Route | Does |
| :-- | :-- |
| `GET /health` | Version and chosen printer. RfiDex checks this |
| `POST /scan/{id}` | Look up, check in, print |
| `POST /scan/{id}/reprint` | Print again, no check-in. RfiDex uses this |
| `GET /search?q=` | Find guests by name, email or phone |
| `POST /print-ticket` | Print a badge from details in the request body |
| `POST /print-test` | Print a test page |
| `POST /png-preview` | Render a badge as PNG, with an optional unsaved layout |
| `GET /printers` | Installed printers and the one in use |
| `GET /event` | The connected event's name, ID and slug |
| `GET /ticket-types`, `GET /ticket-groups` | The connected event's types and groups |
| `GET /help` | The built-in help page: setup guide, troubleshooting, self-check |
| `GET`, `PUT`, `DELETE /config` | Read, change, or disconnect the settings (API key masked) |
| `GET /update/status`, `POST /update/{check,download,apply}` | Self-update |
| `GET /server/status`, `POST /server/{restart,quit}` | Process control |

Errors are `{"detail": "..."}` with a fitting status code.

## Develop

```bash
go run .          # server only: open http://127.0.0.1:8000
go test ./...
```

Desktop build with the native window needs the [Wails CLI](https://wails.io):

```bash
wails build -s                                             # this machine
wails build -s -nsis -installscope user -webview2 embed    # Windows installer
```

Tests cover the layout engine, rendering, config, ticket parsing, the backend
client, signing, and the full scan flow against fake EventzFlow and printer
doubles. Cross-check Windows code with `GOOS=windows go vet ./...`.

## Release

One time: run `go run . keygen`, then store the public key as repository
variable `UPDATE_PUBLIC_KEY` and the private key as repository secret
`UPDATE_PRIVATE_KEY`. Builds without the public key cannot self-update.

```bash
git tag v1.0.0 && git push origin v1.0.0
```

The workflow (`.github/workflows/release.yml`) tests, builds both platforms,
smoke-tests each binary's `/health`, signs the update binaries and publishes
the release with `latest.json`.

## Status

Built and tested: layout and rendering, config, backend client, scan flow,
signing and verification, native-window builds for macOS and Windows.

Not yet verified on real hardware or CI: printing to a physical printer, the
Windows GDI print path, the installer build, and the release workflow. Run a
**Test print** on the first Windows station before an event.
