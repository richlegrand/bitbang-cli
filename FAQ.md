# FAQ

## Why not just use SSH?

`bitbang` is shaped like ssh -- `serve`, `connect`, and `cp` map to `sshd`,
`ssh`, and `scp`, and `connect -L` forwards a port with ssh's own spelling.
WebRTC is the transport instead of TCP. For a machine you can already SSH into
comfortably, that difference does not buy you much.

Four places it does:

**Reach.** SSH needs an inbound path, and on most networks opening one is not
your call -- CGNAT on cellular and Starlink, corporate, university, municipal.
So you bolt on a second system to get one: Tailscale, a VPN, ngrok. Another
install, another account, another daemon. `bitbang serve` needs no open port.

**Setup.** SSH has to be enabled and configured before it lets you in. It is
off by default on Raspberry Pi OS and often key-only, which means getting your
public key onto the machine first -- and the usual answers are email or a USB
stick. `bitbang` uses a 6-digit code you can read over the phone, and runs as
an ordinary user with no root, no daemon, and no config file.

**The connecting side.** SSH needs a client and a key or password there.
`bitbang` needs a browser, which means a phone, a borrowed laptop, or someone
who has never opened a terminal.

**Access that ends.** An SSH key is granted until somebody remembers to remove
it. A `bitbang` link carries an expiry, can be revoked on its own, and reaches
only what you named -- one directory, one forwarded port, one command.

## Why not just use Tailscale?

If the machines are yours and you can install on all of
them, a mesh VPN is the better tool: it is free for personal use, it is fast,
and it makes every device reachable everywhere. We are not going to pretend
otherwise, and `bitbang` is not trying to replace it there.

The claim that it covers every case holds as long as two things are true:
**every party is willing to install a client and sign in**, and **you trust
every party with access to your network.** Both are fair assumptions for your
own devices and poor ones for anyone else, which is where the two tools stop
overlapping:

- **The far end is a person, not a device.** A friend who wants one file, a
  contractor who needs one port for one afternoon, a relative whose printer you
  are fixing. On a mesh they install a client, create an account, and join
  something -- to look at one thing. Here they open a URL.
- **Sharing means letting someone onto your network.** The unit of access on a
  mesh is a device on a tailnet, not a folder or a port. ACLs can narrow that,
  but they are tailnet-wide policy you edit and have to get right, and the
  starting posture is connectivity you scope down from. A `bitbang` link starts
  from nothing and grants exactly what you named -- this directory, that port,
  this one command. And if the laptop you shared a movie with is compromised
  next month, it was never inside anything.
- **You cannot install anything where you are sitting.** A borrowed laptop, a
  locked-down work machine, a hotel business center, a phone that is not yours.
  A mesh needs its client on that side. `bitbang` needs a browser.
- **The access should end by itself.** Tailnet membership persists until you
  revoke it, and revoking it is a thing you have to remember on a day when
  nothing is prompting you. A link expires on the date you set.
- **You would rather not have an account at all.** A tailnet is tied to an
  identity provider. Some people will not, and some organizations cannot.

The one-line version, which is also the honest one: **Tailscale is a network
you join. `bitbang` is a link you hand someone.** For your own machines, join
the network. For everyone else, the link is the whole point.

## Isn't this just a RAT? Couldn't malware use it for command and control?

Malware that can run `bitbang` can run `ssh`, `nc`, or anything else. A remote-access
tool is not a privilege-escalation tool, and the important question for any
of them is how code got to run in the first place.

With `bitbang`  there is no listening
port to scan for, and no login to brute-force. Access is a bearer credential in
a URL fragment, verified by the device itself.

## What can the signaling server see?

Enough to introduce two peers, and no more:

- Yes: both ends' IP addresses, the device's public key and derived UID, when
  connections happen and roughly how much data flows.
- No: your traffic. It rides WebRTC's DTLS end to end. Nor the access code --
  that lives in the URL fragment, which browsers never transmit.

The authentication is genuinely independent of the server -- it cannot insert
itself into a connection -- but it is well placed for traffic analysis. *Minimal
trust* is the honest description of the server. If you prefer, it's straightforward to run 
the server yourself. It's open source and compiles to a single Go binary.

## Is it really peer-to-peer if it needs a server?

Every P2P system needs a rendezvous.
What differs is what the rendezvous knows and what it costs.

Here it holds no accounts, no registry, and nothing that grants access. It is a
phonebook, not a keyring. It is also out of the data path once the two ends are
introduced, which is why it's cheap enough to self-host on the smallest VPS.

## Why not use a DHT and skip the server entirely?

Because a browser cannot join one. DHTs need UDP sockets, and browsers do not
have them -- so a DHT-based design requires a native client on the connecting
side, which is the exact thing `bitbang` exists to avoid.

Even with a DHT you would still need somewhere to exchange SDP and ICE
candidates, so the rendezvous does not actually disappear. It moves.

## What about tailcat, netbird, ZeroTier, or Headscale?

The same answer as [Tailscale](#why-not-just-use-tailscale): they build a
network you join, and each needs its own client on both ends. tailcat is the
closest of them in spirit -- no account, one static binary, encrypted end to
end -- but it still needs its binary on the far end, and its browser build
moves files and text over a relay rather than giving you a machine.

## How is this different from iroh, dumbpipe, or Magic Wormhole?

Those are transports, and good ones -- iroh's discovery is genuinely nice, and
node-IDs-as-public-keys is the same idea as our UIDs. What they give you is a
pipe between two programs you write.

`bitbang` is a product on top of a transport: a terminal, a file browser, a
proxy to web apps on the far network, port forwarding, and access links that expire (if you wish).
The far end needs no code at all, which a library cannot offer.

Magic Wormhole is closer in spirit but aimed at one file transfer, with
short human-readable codes and a PAKE to match. We use a 6-digit SAS for
pairing, which is the same idea for the case where you can read something aloud.

## How is this different from ngrok, Cloudflare Tunnel, tmate, or Gradio?

Those relay through servers that belong to someone else -- often with an
account, usually with an expiry, always with your plaintext passing through
their infrastructure. `bitbang` is peer-to-peer after the handshake, and when a
relay is unavoidable it carries ciphertext only.

The other difference is what a browser gets. Those hand back a web server you
were already running; `bitbang` gives a terminal, a file browser, and the web
apps on that machine's network without you running anything first.

## Does my traffic go through your relay? Who pays for it?

Only if a direct path cannot be established -- two symmetric NATs, or a network
that blocks UDP outright, then a TURN relay carries the session, still
encrypted end to end, so the relay sees ciphertext.

The `bitba.ng` relay is provided by us and it's time-limited. 
A TURN relay is needed for around 25% of network topologies. If your topology 
requires TURN and you need sustained connections, you can easily provide your own TURN server using the `-ice-servers` 
argument. For example, Cloudflare's TURN is free to
1,000 GB and $0.05/GB after. The listener hands the config to the signaling
server, which gives it to whoever connects, so both ends use yours and ours is
never involved.

## `curl | sh`? Really?

Never `sudo`, and it installs one binary to `~/.local/bin` after verifying its
SHA-256 against the release's `checksums.txt`.

If you would rather read it first -- and that is a reasonable instinct:

```
curl -sSfL bitba.ng/install -o install.sh && less install.sh && sh install.sh
```

`bitba.ng/install` is a redirect, not a hosted script: it 302s to
[`install.sh`](install.sh) in this repository, so the thing you are piping is
the same file you can read here. Or skip it and download a release binary
directly.

## What about IPv6?

It helps, and it does not remove the need for this.

IPv6 gives every device a routable address, so there is no NAT to traverse. But
it does not open the firewall, which still defaults to blocking inbound, and it
says nothing about authentication -- you still need something to introduce the
peers and verify who they are. What v6 changes is that hole punching gets
easier and more connections end up direct rather than relayed. That is a real
improvement, and `bitbang` benefits from it.

## Was this written with AI?

This is a spare-time project; AI is
good at the drudgery and there is no substitute for reviewing what it writes.
Every line is read before it lands, the test suite runs on three platforms, and
the transport and security design are my own.

## Can I self-host the whole thing?

Yes -- the signaling server is open source and it is the only piece that is
ever ours. Run it, point `-server` at it, and `bitba.ng` is not involved at any
stage. `-ice-servers` does the same for the TURN relay. Neither requires
telling us anything.
