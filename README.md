# BitBangProxy

![Tests](https://github.com/richlegrand/bitbangproxy/actions/workflows/tests.yml/badge.svg)
![License](https://img.shields.io/github/license/richlegrand/bitbangproxy)

BitBangProxy enables full access to your NAS / Jellyfin / Plex / Open WebUI / Flask apps / Node-RED dashboard / etc. from outside your network. It's a single executable that runs on a machine on your local network. It doesn't require port forwarding, VPNs, accounts, or software to be installed on the target machine.

This is part of the [BitBang project](https://github.com/richlegrand/bitbang). 

## Quick start

```bash
# Download the binary for your platform from Releases, then:
./bitbangproxy
```

This prints a URL and waits for connections. Open the URL in a browser, enter a local server address (e.g. `nas.local`, `192.168.1.10:8080`), and you're connected.

Or specify the target directly in the URL:

```
https://bitba.ng/<proxy-id>/nas.local
https://bitba.ng/<proxy-id>/192.168.1.10:8080
https://bitba.ng/<proxy-id>/localhost:3000/admin
```

## Features

- **HTTP proxy** -- GET, POST, uploads, downloads, streaming (SSE)
- **WebSocket proxy** -- bidirectional, multiplexed over the same data channel
- **Dynamic targets** -- target server specified in the URL, no restart needed
- **Cookie/session support** -- login flows work (managed by the service worker)
- **PIN protection** -- optional `--pin` flag to restrict access
- **Redirect handling** -- follows cross-host redirects, passes same-host redirects to the browser
- **No installation on the target** -- proxy runs on any machine on the same network

## Comparison

| | ngrok | Cloudflare Tunnel | Tailscale | BitBang |
|---|---|---|---|---|
| Account required | Yes | Yes | Yes | No |
| Free tunnels | 1 | Unlimited | Unlimited | Unlimited |
| Data path | Their servers | Their servers | P2P | P2P |
| Viewer needs install | No | No | Yes | No |
| Configuration | CLI flags | Config file + DNS | Dashboard | None |

BitBang's data path is direct between peers. The signaling server (`bitba.ng`) brokers the initial connection, then steps aside.

## Usage

```bash
# Dynamic target (from URL)
./bitbangproxy

# Fixed target
./bitbangproxy --target localhost:8080

# With PIN protection
./bitbangproxy --pin 1234

# Ephemeral identity (new URL each run)
./bitbangproxy --ephemeral

# Custom signaling server
./bitbangproxy --server my-signaling-server.com

# Verbose logging
./bitbangproxy -v
```

## CLI flags

```
--target HOST:PORT   Local server to proxy (default: dynamic from URL)
--pin PIN            PIN to protect proxy access
--ephemeral          Use a temporary identity (new URL each run)
--server HOST        Signaling server hostname (default: bitba.ng)
-v                   Verbose logging and browser debug UI (?debug)
```

When `-v` is enabled, the printed URL includes `?debug`, which activates a browser-side debug UI showing connection steps. Without it, the browser shows a simple "Loading..." while connecting. Verbose mode also logs all HTTP requests and dependency versions at startup.

## How it works

![BitBangProxy Block Diagram](https://raw.githubusercontent.com/richlegrand/bitbangproxy/refs/heads/main/assets/bitbangproxy.png)

The signaling server (`bitba.ng`) brokers the WebRTC handshake, then steps aside. All traffic flows directly between the browser and the proxy via an encrypted data channel (DTLS).

## Building from source

Requires Go 1.19+:

```bash
go build ./cmd/bitbangproxy/
```

Cross-compile for other platforms:

```bash
GOOS=windows GOARCH=amd64 go build -o bitbangproxy.exe ./cmd/bitbangproxy/
GOOS=darwin GOARCH=amd64 go build -o bitbangproxy-macos ./cmd/bitbangproxy/
GOOS=linux GOARCH=amd64 go build -o bitbangproxy ./cmd/bitbangproxy/
```

## Architecture

```
cmd/bitbangproxy/main.go       -- entry point, CLI flags, connection management
internal/identity/identity.go  -- RSA keypair, UID derivation, persistence
internal/signaling/client.go   -- WebSocket signaling, challenge-response auth
internal/peer/connection.go    -- WebRTC peer connection, ICE, SDP
internal/protocol/swsp.go      -- SWSP frame parsing/building
internal/proxy/http.go         -- HTTP proxying, redirects, cookies, landing page
internal/proxy/websocket.go    -- WebSocket bridging
internal/auth/pin.go           -- PIN verification
```

See [implementation_notes.md](implementation_notes.md) for detailed design decisions.

## License

MIT See [LICENSE](LICENSE).

## Contributing

This is a one-person project. Issues and PRs are welcome and genuinely appreciated. I'll do my best to respond promptly.
