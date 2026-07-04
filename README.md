# bitbang

**Secure remote access to any machine -- shell, files, and web apps -- from a browser or another terminal. No SSH, no port forwarding, no account, and nothing to install on the far end.**

![Tests](https://github.com/richlegrand/bitbang-cli/actions/workflows/tests.yml/badge.svg)
![License](https://img.shields.io/github/license/richlegrand/bitbang-cli)

`bitbang` is a single Go binary. Run `bitbang serve` on a machine -- a Raspberry Pi, a home server, a workstation behind NAT -- and it prints one URL. Open that URL in any browser, or `bitbang connect` to it from another machine, and you get a terminal, a file browser, and a gateway to that machine's network. The connection is peer-to-peer and end-to-end encrypted; the `bitba.ng` signaling server only introduces the two ends, then steps aside.

Part of the [BitBang project](https://github.com/richlegrand/bitbang).

## Why

- **Nothing to forward or configure.** Works from behind NAT, CGNAT, or a locked-down network -- no router changes, no VPN, no tunnel daemon.
- **Nothing to install on the client.** A browser is enough. A CLI is there when you want scripting, pipes, and file copy.
- **Private by design.** Traffic is WebRTC/DTLS, peer-to-peer. The signaling server never sees it; if a direct path isn't possible, a TURN relay carries ciphertext only.
- **No account, no telemetry.**

## Install

**Linux (one-liner):**

```bash
curl -sSfL bitba.ng/install | sh
```

Detects your arch (`amd64`, `arm64`, `armv7`), downloads the matching binary from the latest [GitHub release](https://github.com/richlegrand/bitbang-cli/releases), verifies its SHA-256 against the release's `checksums.txt`, and installs to `~/.local/bin/bitbang`. If `~/.local/bin` isn't on your PATH, the script tells you what to add.

Pin a version or change the install location:

```bash
curl -sSfL bitba.ng/install | sh -s -- --version v0.5.0
curl -sSfL bitba.ng/install | sh -s -- --prefix /usr/local/bin
```

Audit first (recommended in shared environments -- the script is short and readable):

```bash
curl -sSfL bitba.ng/install -o install.sh
less install.sh
sh install.sh
```

macOS and Windows builds are coming -- check the [releases page](https://github.com/richlegrand/bitbang-cli/releases) for status.

**Manual install (any platform):** download the binary for your OS from [Releases](https://github.com/richlegrand/bitbang-cli/releases) and place it on your PATH.

**Build from source:** see [Building from source](#building-from-source) below.

### How the install URL works

`bitba.ng/install` is a redirect, not a hosted script. The chain:

1. `curl` hits `https://bitba.ng/install`, which 302s to [`install.sh`](install.sh) in this repo (on `main`).
2. The script runs in your shell, detects OS+arch, and downloads the binary asset from `https://github.com/richlegrand/bitbang-cli/releases/latest/download/bitbang-linux-<arch>`.
3. It fetches `checksums.txt` from the same release and verifies the binary's SHA-256.
4. Installs to `~/.local/bin` (overridable).

The install script lives in this repo, next to the code it installs -- so contributors review it alongside the binary, and the canonical bitba.ng host owns only the short URL. Self-hosters can point their own host's `/install` at whatever script they ship: the signaling server's `INSTALL_URL` env var controls the redirect target (empty → 404).

## Background

I spend a lot of time on Raspberry Pis, Jetson Nanos, and similar machines, and the file-shuffle pain is real: USB stick out, SSH keys onto it, USB into the Pi, copy, eject. Running an ad-hoc command on a headless machine is also more trouble than it should be. The fix I wanted was something pairing-based -- like a [Magic Wormhole](https://github.com/magic-wormhole/magic-wormhole) code, but for a full session, not just file transfer.

What makes BitBang a little unusual: once you start a server on the remote machine, the same URL works two ways. Open it in a browser and you get an in-page terminal, a file browser, and a web-app proxy. Hand it to `bitbang connect` from another machine and you get scriptable CLI access -- pipes, redirects, `bitbang cp`, the works. One protocol, two front ends.

## Quick start

Every `serve` command prints a URL like `https://bitba.ng/<id>#<code>`. Open it in a browser, or hand it to `bitbang connect` / `bitbang cp`.

### Shell

```bash
bitbang serve shell
```

A full terminal in the browser (xterm.js -- colors, resize, copy/paste), or from another machine:

```bash
bitbang connect https://bitba.ng/<id>#<code>                              # interactive shell
bitbang connect https://bitba.ng/<id>#<code> -- tail -f /var/log/syslog   # one-shot command
```

### Files

```bash
bitbang serve files ~/share                  # share a directory (read-only)
bitbang serve files ~/share -files-upload    # ...and allow uploads
```

Browse, preview, download, and upload in the browser -- or copy from the CLI, scp-style:

```bash
bitbang cp https://bitba.ng/<id>#<code>:/var/log/app.log ./app.log        # remote -> local
bitbang cp ./firmware.bin https://bitba.ng/<id>#<code>:/tmp/firmware.bin   # local -> remote
```

### Proxy

Reach any HTTP / WebSocket service on the machine's network -- a NAS, Jellyfin, Node-RED, a Flask dashboard -- through your browser:

```bash
bitbang serve proxy                  # choose the target in the URL
bitbang serve proxy localhost:8080   # or pin a single target
```

Open the URL, type a LAN address (`nas.local`, `192.168.1.10:8080`, `localhost:3000/admin`), and you're in. Logins, cookies, uploads, downloads, and server-sent events all work -- sessions are handled by a service worker, so apps behave normally.

### Everything at once

```bash
bitbang serve     # shell + files + proxy on one URL; the browser offers each
```

## Connecting

Same URL, two front ends:

- **Browser** -- open `https://bitba.ng/<id>#<code>`. Nothing to install.
- **CLI** -- `bitbang connect <url>` for a shell (add `-- cmd` for one-shot), `bitbang cp` for files.

The access code lives in the URL **fragment** (`#…`), which browsers never send to a server -- so `bitba.ng` brokers the connection without ever seeing the secret that authorizes it.

## Security

- **Self-certifying identity.** On first run, `bitbang` generates an RSA keypair under `~/.bitbang/<program>/`; the device UID is *derived from* the public key, so it can't be impersonated.
- **Optional PIN.** Add `--pin` for permanent or headless setups -- connectors must supply it.
- **End-to-end encryption.** All traffic rides WebRTC's DTLS. The signaling server sees only the public key, the derived UID, and connection metadata -- never your data. A TURN relay, if one is needed, sees ciphertext only.
- **Throwaway mode.** `-ephemeral` uses a temporary identity (a fresh URL each run).

## How it works

```
  browser / bitbang connect           bitba.ng              bitbang serve
         (client)        ── handshake ─ (signaling) ─ handshake ──  (device)
            └──────────────  encrypted WebRTC data channel  ──────────────┘
                            (direct peer-to-peer, or TURN-relayed)
```

`bitba.ng` brokers the WebRTC handshake and then gets out of the way -- application traffic flows directly between the two peers over an encrypted data channel. Shell, file, and proxy traffic are multiplexed over that single channel using **SWSP** (Simple WebRTC Streaming Protocol): a small `streamId | flags | length | payload` framing, carrying HTTP requests for file/proxy operations and long-lived streams for the shell.

## How it compares

| | ngrok | Cloudflare Tunnel | Tailscale | bitbang |
|---|---|---|---|---|
| Account required | Yes | Yes | Yes | **No** |
| Client install | No | No | **Yes** | **No** (browser) |
| Port forwarding / router config | No | No | No | **No** |
| Data path | Their servers | Their servers | P2P | **P2P** |
| Configuration | CLI flags | Config + DNS | Dashboard | **None** |

## Command reference

Flags accept either form (`-pin` or `--pin`). Boolean flags default off unless noted.

```
bitbang serve [flags]                  All caps: shell + files + proxy on one URL
bitbang serve shell [flags]            Shell only
bitbang serve files [PATH] [flags]     Files only (PATH defaults to cwd)
bitbang serve proxy [TARGET] [flags]   HTTP/WebSocket reverse proxy (TARGET pins one host:port)
bitbang connect <target> [-- cmd …]    Client shell (interactive or one-shot)
bitbang cp <src> <dst>                 Copy files (one side is <URL>:/path, or '-')
bitbang version                        Print version (also --version)
bitbang help                           Usage (also --help, -h)
```

### `bitbang serve` -- run a listener

**Shared flags** (all four `serve` forms):

| Flag | Default | Description |
|---|---|---|
| `-server HOST` | `bitba.ng` | Signaling server hostname |
| `-pin PIN` | (none) | Require this PIN for connections |
| `-ephemeral` | off | Temporary identity (a fresh URL each run) |
| `-nocode` | off | Disable code-exchange pairing -- no 6-digit code is issued; the URL still works. Use for headless/non-TTY listeners that can't complete the SAS prompt. |
| `-program NAME` | `bitbang` | Identity name; keypair stored at `~/.bitbang/<NAME>/identity.pem` |
| `-target HOST:PORT` | (dynamic) | Fixed proxy target (proxy mode); empty = pick the target in the browser. `serve proxy host:port` is shorthand for this. |
| `-v` | off | Verbose logging (adds the browser `!debug` overlay) |

**Shell flags** (`serve` and `serve shell`):

| Flag | Default | Description |
|---|---|---|
| `-shell-cmd CMD` | `$SHELL` or `/bin/sh` | Shell to spawn |
| `-shell-max-sessions N` | `1` | Max concurrent shell sessions (0 = unlimited) |
| `-shell-mirror` | on | Mirror shell output to the listener's console |

**Files flags:**

| Form | Path | Upload flag |
|---|---|---|
| `serve` (all caps) | `-files PATH` (default cwd) | `-files-upload` |
| `serve files [PATH]` | positional `PATH` (default cwd) | `-upload` |

*(Advanced: `-video-fd N` passes an inherited socketpair FD to an external video helper; for internal/embedding use.)*

### `bitbang connect <target> [-- command …]` -- client shell

`<target>` may be any of:

- a **saved name** -- e.g. `nas1`; resolved from the known-hosts table (see below)
- a **6-digit pair code** -- e.g. `482731`; runs the pairing flow, then connects
- a **URL** -- `https://bitba.ng/<id>#<code>`, `bitba.ng/<id>#<code>`, or bare `<id>#<code>`

With no `-- command`, opens an interactive shell (a PTY when stdin is a terminal). With `-- command args…`, runs that single command non-interactively and exits with its status (signal exits report 128).

| Flag | Default | Description |
|---|---|---|
| `-name NAME` | (auto) | Remember this host under NAME (new hosts only; auto-assigns `device<N>` if omitted) |
| `-relay` | off | Request a TURN relay up front instead of only on fallback (ICE still prefers a direct path if one succeeds) |
| `-pin PIN` | (prompt) | PIN to send if the listener requires one (skips the interactive prompt) |
| `-timeout DUR` | `30s` | Dial timeout (e.g. `45s`, `1m`) |
| `-server HOST` | `bitba.ng` | Signaling server -- **pair-code mode only**; the URL form carries its own host |
| `-v` | off | Verbose logging |

### `bitbang cp <src> <dst>` -- copy files

Exactly one of `<src>` / `<dst>` is remote, written `<URL>:/path` (URL in any form accepted by `connect`). `-` means stdin/stdout, so `cp <URL>:/f -` streams to stdout and `cp - <URL>:/f` uploads from stdin. A trailing `/` or `.` on the local side keeps the remote basename (scp-style).

| Flag | Default | Description |
|---|---|---|
| `-relay` | off | Request a TURN relay up front (as in `connect`) |
| `-pin PIN` | (prompt) | PIN to send if required |
| `-timeout DUR` | `30s` | Dial timeout |
| `-v` | off | Verbose logging |

### Device names & the known-hosts table

Every successful connect or pairing is remembered in `~/.bitbang/devices.json` (mode `0600`), so you can reconnect by a short name instead of a URL or code:

```bash
bitbang connect 482731 -name nas1     # pair once, save it as "nas1"
bitbang connect nas1                  # thereafter, just the name
```

- **`-name NAME`** chooses the name; it applies only to a *new* host. Without it, an auto name (`device1`, `device2`, …) is assigned and printed (`Saved as "device1".`).
- **Naming rules:** a name must start with a letter and contain only letters, digits, `-`, or `_`. That guarantees it can never be mistaken for a 6-digit code or a URL. Lookups and uniqueness are case-insensitive.
- **No renaming via connect:** `bitbang connect nas1 -name nas2` is rejected -- `-name` is for first-time saves only.
- **When it's saved:** a pairing is recorded as soon as the SAS is verified (so a flaky reconnect doesn't lose it); a URL connect is recorded once connected.
- Each entry stores `{name, uid, access_code, server, paired_at}`. Reconnecting a known host (by name or URL) refreshes it in place and keeps the name.

## Building from source

Requires Go 1.25+. Pure Go, statically linked (`CGO_ENABLED=0`) -- trivial cross-compilation, no runtime dependencies.

```bash
go build ./cmd/bitbang/

# cross-compile:
GOOS=linux   GOARCH=arm64        go build -o bitbang-arm64 ./cmd/bitbang/
GOOS=linux   GOARCH=arm GOARM=7  go build -o bitbang-armv7 ./cmd/bitbang/
GOOS=windows GOARCH=amd64        go build -o bitbang.exe   ./cmd/bitbang/
GOOS=darwin  GOARCH=arm64        go build -o bitbang-macos ./cmd/bitbang/
```

## Roadmap

Shipping today: **shell, files, and proxy**, reachable from the browser or the CLI, plus scp-style file copy and **ad-hoc pairing** -- short 6-digit codes with a human-verified challenge (SAS) and a saved device table (`bitbang connect nas1`). Designed and on the way:

- **Serial bridging** -- drive a remote `/dev/ttyUSB0` from a local virtual port (e.g. the Arduino IDE, over the internet).
- **TCP port forwarding** -- `-L 5432:db.internal:5432` to reach LAN-only services.
- **Remote desktop** -- screen over a WebRTC video track, keyboard/mouse over the data channel.
- **Network mode** -- team/fleet access with enrollment, scoped tokens, device discovery, and audit logging.

## License

MIT -- see [LICENSE](LICENSE).

## Contributing

Issues and PRs welcome.
