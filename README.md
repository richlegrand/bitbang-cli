# bitbang

**Secure remote access to any machine — shell, files, and web apps — from a browser or another terminal. No SSH, no port forwarding, no account, and nothing to install on the far end.**

![Tests](https://github.com/richlegrand/bitbang-cli/actions/workflows/tests.yml/badge.svg)
![License](https://img.shields.io/github/license/richlegrand/bitbang-cli)

`bitbang` is a single Go binary. Run `bitbang serve` on a machine — a Raspberry Pi, a home server, a workstation behind NAT — and it prints one URL. Open that URL in any browser, or `bitbang connect` to it from another machine, and you get a terminal, a file browser, and a gateway to that machine's network. The connection is peer-to-peer and end-to-end encrypted; the `bitba.ng` signaling server only introduces the two ends, then steps aside.

Part of the [BitBang project](https://github.com/richlegrand/bitbang).

## Why

- **Nothing to forward or configure.** Works from behind NAT, CGNAT, or a locked-down network — no router changes, no VPN, no tunnel daemon.
- **Nothing to install on the client.** A browser is enough. A CLI is there when you want scripting, pipes, and file copy.
- **Private by design.** Traffic is WebRTC/DTLS, peer-to-peer. The signaling server never sees it; if a direct path isn't possible, a TURN relay carries ciphertext only.
- **No account, no telemetry.**

## Install

Download the binary for your platform from [Releases](https://github.com/richlegrand/bitbang-cli/releases), or build it (see [Building](#building-from-source)):

```bash
go build ./cmd/bitbang/     # produces ./bitbang
```

## Quick start

Every `serve` command prints a URL like `https://bitba.ng/<id>#<code>`. Open it in a browser, or hand it to `bitbang connect` / `bitbang cp`.

### Shell

```bash
bitbang serve shell
```

A full terminal in the browser (xterm.js — colors, resize, copy/paste), or from another machine:

```bash
bitbang connect https://bitba.ng/<id>#<code>                              # interactive shell
bitbang connect https://bitba.ng/<id>#<code> -- tail -f /var/log/syslog   # one-shot command
```

### Files

```bash
bitbang serve files ~/share                  # share a directory (read-only)
bitbang serve files ~/share -files-upload    # ...and allow uploads
```

Browse, preview, download, and upload in the browser — or copy from the CLI, scp-style:

```bash
bitbang cp https://bitba.ng/<id>#<code>:/var/log/app.log ./app.log        # remote -> local
bitbang cp ./firmware.bin https://bitba.ng/<id>#<code>:/tmp/firmware.bin   # local -> remote
```

### Proxy

Reach any HTTP / WebSocket service on the machine's network — a NAS, Jellyfin, Node-RED, a Flask dashboard — through your browser:

```bash
bitbang serve proxy                         # choose the target in the URL
bitbang serve proxy -target localhost:8080  # or pin a single target
```

Open the URL, type a LAN address (`nas.local`, `192.168.1.10:8080`, `localhost:3000/admin`), and you're in. Logins, cookies, uploads, downloads, and server-sent events all work — sessions are handled by a service worker, so apps behave normally.

### Everything at once

```bash
bitbang serve     # shell + files + proxy on one URL; the browser offers each
```

## Connecting

Same URL, two front ends:

- **Browser** — open `https://bitba.ng/<id>#<code>`. Nothing to install.
- **CLI** — `bitbang connect <url>` for a shell (add `-- cmd` for one-shot), `bitbang cp` for files.

The access code lives in the URL **fragment** (`#…`), which browsers never send to a server — so `bitba.ng` brokers the connection without ever seeing the secret that authorizes it.

## Security

- **Self-certifying identity.** On first run, `bitbang` generates an RSA keypair under `~/.bitbang/<program>/`; the device UID is *derived from* the public key, so it can't be impersonated.
- **Optional PIN.** Add `--pin` for permanent or headless setups — connectors must supply it.
- **End-to-end encryption.** All traffic rides WebRTC's DTLS. The signaling server sees only the public key, the derived UID, and connection metadata — never your data. A TURN relay, if one is needed, sees ciphertext only.
- **Throwaway mode.** `-ephemeral` uses a temporary identity (a fresh URL each run).

## How it works

```
  browser / bitbang connect           bitba.ng              bitbang serve
         (client)        ── handshake ─ (signaling) ─ handshake ──  (device)
            └──────────────  encrypted WebRTC data channel  ──────────────┘
                            (direct peer-to-peer, or TURN-relayed)
```

`bitba.ng` brokers the WebRTC handshake and then gets out of the way — application traffic flows directly between the two peers over an encrypted data channel. Shell, file, and proxy traffic are multiplexed over that single channel using **SWSP** (Simple WebRTC Streaming Protocol): a small `streamId | flags | length | payload` framing, carrying HTTP requests for file/proxy operations and long-lived streams for the shell.

## How it compares

| | ngrok | Cloudflare Tunnel | Tailscale | bitbang |
|---|---|---|---|---|
| Account required | Yes | Yes | Yes | **No** |
| Client install | No | No | **Yes** | **No** (browser) |
| Port forwarding / router config | No | No | No | **No** |
| Data path | Their servers | Their servers | P2P | **P2P** |
| Configuration | CLI flags | Config + DNS | Dashboard | **None** |

## Common flags

**`serve`**
- `-pin PIN` — require a PIN for connections
- `-ephemeral` — temporary identity (new URL each run)
- `-target HOST:PORT` — fixed proxy target (proxy mode)
- `-files PATH` / `-files-upload` — directory to share / allow uploads
- `-shell-cmd CMD` — shell to spawn (default `$SHELL` or `/bin/sh`)
- `-shell-max-sessions N` — cap concurrent shells (0 = unlimited)
- `-server HOST` — signaling server (default `bitba.ng`)
- `-v` — verbose logging (adds the browser `?debug` overlay)

**`connect` / `cp`** — `-pin`, `-timeout`, `-v`

## Building from source

Requires Go 1.25+. Pure Go, statically linked (`CGO_ENABLED=0`) — trivial cross-compilation, no runtime dependencies.

```bash
go build ./cmd/bitbang/

# cross-compile:
GOOS=linux   GOARCH=arm64        go build -o bitbang-arm64 ./cmd/bitbang/
GOOS=linux   GOARCH=arm GOARM=7  go build -o bitbang-armv7 ./cmd/bitbang/
GOOS=windows GOARCH=amd64        go build -o bitbang.exe   ./cmd/bitbang/
GOOS=darwin  GOARCH=arm64        go build -o bitbang-macos ./cmd/bitbang/
```

## Roadmap

Shipping today: **shell, files, and proxy**, reachable from the browser or the CLI, plus scp-style file copy. Designed and on the way:

- **Serial bridging** — drive a remote `/dev/ttyUSB0` from a local virtual port (e.g. the Arduino IDE, over the internet).
- **TCP port forwarding** — `-L 5432:db.internal:5432` to reach LAN-only services.
- **Remote desktop** — screen over a WebRTC video track, keyboard/mouse over the data channel.
- **Ad-hoc pairing** — short numeric codes with a human-verified challenge, plus a saved device table (`bitbang connect pi-sensor`).
- **Network mode** — team/fleet access with enrollment, scoped tokens, device discovery, and audit logging.

## License

MIT — see [LICENSE](LICENSE).

## Contributing

Issues and PRs welcome.
