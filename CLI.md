# `bitbang` command reference

Every subcommand and flag. `bitbang <command> --help` prints the same thing at
the terminal. For what the tool is and why, see the [README](README.md); for
questions people actually ask, the [FAQ](FAQ.md); for worked examples, the
[cookbook](https://github.com/richlegrand/bitbang/blob/main/cookbook.md).

Flags accept either form (`-pin` or `--pin`). Boolean flags default off unless noted.

```
bitbang serve                          Everything: shell + proxy + files + forward
bitbang serve WORD [ARG] ...           Name what to serve, in any combination:
                                         shell, proxy [TARGET,...],
                                         files [PATH], forward [HOST:PORT,...]
bitbang share [flags]                  Publish a running tmux session
bitbang share status|stop|rotate       Inspect, stop, or replace a share
bitbang connect <target> [-- cmd …]    Client shell (interactive or one-shot)
bitbang cp <src> <dst>                 Copy files (one side is <URL>:/path, or '-')
bitbang version                        Print version (also --version)
bitbang help                           Usage (also --help, -h)
```

### `bitbang serve` -- run a listener

Name what to serve. Each word takes the one thing it serves; bare `serve`
means all four.

```
bitbang serve                                    everything, files from cwd
bitbang serve shell                              a terminal, nothing else
bitbang serve files ~/share                      one directory
bitbang serve proxy nas.lan:8096                 one web app, straight at the URL
bitbang serve proxy a.lan:80,b.lan:80            several, chosen in the browser
bitbang serve forward 127.0.0.1:22               TCP for connect -L, one target
bitbang serve shell files ~/share proxy nas.lan:8096 forward db:5432
bitbang serve shell tmux attach                  a command, which may be several words
```

| Word              | Argument                        | Without one                      |
| ----------------- | ------------------------------- | -------------------------------- |
| `shell [COMMAND]` | the command to run, quoted if it is more than one word | `$SHELL`, or `%COMSPEC%` on Windows |
| `files [PATH]`    | a directory or file             | the working directory            |
| `proxy [TARGET…]` | one target, or a comma list     | the browser names its own        |
| `forward [HOST:PORT…]` | one target, or a comma list | any host:port the listener can reach |

**One proxy target pins it.** With nothing else served, the bare device URL is
that app -- no landing page. Alongside other capabilities it becomes an entry in
the caret menu that goes straight there. Several targets are offered as a
choice, and in both cases the proxy can reach only what was named.

**A capability word is never eaten as another word's argument**, so
`serve files proxy` shares the working directory and serves a proxy. A
directory genuinely called `proxy` needs `./proxy`.

**A command of more than one word is quoted.** Every word takes exactly one
argument, so quoting is what says where a command ends -- nothing has to guess,
and a flag inside the quotes belongs to the command rather than to `bitbang`:

```
bitbang serve shell "ssh -p 2222 host"
bitbang serve shell "tmux attach" forward     # a command, and forwarding
bitbang serve shell "tmux attach forward"     # one command, no forwarding
bitbang serve shell tmux attach               # error: "attach" is not something to serve
```

An argument that itself contains a space is quoted again inside, which is how
a Windows path is spelled: `shell "'C:\Program Files\Git\bin\bash.exe' --login"`.

The rule for the flags below: **a word says what is served, a flag says how.**
A flag whose capability was not named is an error rather than a setting that
does nothing.

| Flag                       | Needs     | Default        | Description                                                                                                          |
| -------------------------- | --------- | -------------- | ---------------------------------------------------------------------------------------------------------------------- |
| `-server HOST`             | --        | `bitba.ng`     | Signaling server hostname                                                                                            |
| `-pin PIN`                 | --        | (none)         | Require this PIN for connections                                                                                     |
| `-ephemeral`               | --        | off            | Temporary identity: a fresh URL each run, and connectors do not save it to `devices.json`                            |
| `-program NAME`            | --        | `bitbang`      | Identity name; keypair stored at `~/.bitbang/<NAME>/identity.pem`                                                    |
| `-ice-servers PATH`        | --        | (ours)         | JSON file of your own STUN/TURN servers; see [Bring your own TURN](#bring-your-own-turn)                              |
| `-nocode`                  | --        | off            | Disable code-exchange pairing -- no 6-digit code is issued; the URL still works. For headless listeners that can't complete the SAS prompt. |
| `-v`                       | --        | off            | Verbose logging (adds the browser `!debug` overlay)                                                                  |
| `-shell-max-sessions N`    | `shell`   | `10`           | Max concurrent shell sessions (0 = unlimited)                                                                        |
| `-disable-shell-mirror`    | `shell`   | off            | Stop echoing shell output to the listener's console                                                                  |
| `-files-upload`            | `files`   | off            | Allow uploads into the shared directory                                                                              |
| `-proxy-client-ip`         | `proxy`   | off            | Stamp the real browser IP as `X-Forwarded-For`. Enable only when the backend trusts localhost for auth.               |

**Targets are matched as written and never resolved**: allowing
`192.168.1.50:22` does not allow `nas.lan:22` even when the name points there.
Resolving would check a name at one moment and dial it a moment later, and the
two can disagree. Given no targets at all, forwarding reaches any host:port the
listener can reach.

*(Advanced: `-video-fd N` passes an inherited socketpair FD to an external
video helper; for internal/embedding use, and hidden from `--help`.)*

### `bitbang share` -- publish a running tmux session

| Flag               | Default           | Description                                                  |
| ------------------ | ----------------- | ------------------------------------------------------------ |
| `-read-only`       | off               | Do not generate a control credential                         |
| `-ttl DURATION`    | `0` (no expiry)   | Lifetime up to `8760h`; `0` means until stopped              |
| `-target SESSION`  | enclosing session | Session name or `$id`; required when run outside tmux        |
| `-socket PATH`     | enclosing server  | tmux socket; needed for a non-default server outside tmux    |
| `-max-viewers N`   | `16`              | Maximum concurrent view-only peers                           |
| `-server HOST`     | `bitba.ng`        | Signaling server hostname                                    |
| `-v`               | off               | Verbose logging                                              |

`share status`, `share stop`, and `share rotate` accept the same target and
socket flags. `rotate` also accepts publication flags and issues fresh URLs.

### `bitbang link` -- access links for a listener

| Command                  | What it does                                            |
|--------------------------|---------------------------------------------------------|
| `bitbang link ls`        | List this listener's links: grant, expiry, code          |
| `bitbang link edit`      | Open `links.json` in `$EDITOR`, validated on save        |
| `bitbang link rm LABEL`  | Delete a link (reload the listener to close its sessions)|
| `bitbang link qr LABEL`  | Print a link's URL and QR code                           |

| Entry field | Meaning                                                                      |
|-------------|------------------------------------------------------------------------------|
| `label`     | Names the link; identifies it to `rm` and `qr`, and must be unique             |
| `grant`     | What the link reaches, in the words `serve` takes: `files [DIR]`, `proxy [TARGETS]`, `forward [TARGETS]`, `shell [COMMAND]`. Can only narrow what the listener serves. Omit for all of it |
| `expires`   | RFC 3339 timestamp. Omit for a link that does not lapse                        |
| `code`      | Filled in by the listener on reload. Leave it out to have one minted           |

`--program NAME` picks a listener other than the default, matching `serve
--program`.

### `bitbang connect <target> [-- command …]` -- client shell

`<target>` may be any of:

- a **saved name** -- e.g. `nas1`; resolved from the known-hosts table (see below)
- a **6-digit pair code** -- e.g. `482731`; runs the pairing flow, then connects
- a **URL** -- `https://bitba.ng/<id>#<code>`, `bitba.ng/<id>#<code>`, or bare `<id>#<code>`

With no `-- command`, opens an interactive shell (a PTY when stdin is a terminal). With `-- command args…`, runs that single command non-interactively and exits with its status (signal exits report 128).

| Flag                                    | Default    | Description                                                                                                 |
| --------------------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------- |
| `-L LOCAL_PORT:REMOTE_HOST:REMOTE_PORT` | (none)     | Forward `LOCAL_PORT` to `REMOTE_HOST:REMOTE_PORT` without a shell. TCP only (repeatable; bracket IPv6 hosts) |
| `-g`                                    | off        | Bind forwarded ports on `0.0.0.0` instead of `127.0.0.1`                                                   |
| `-name NAME`                            | (auto)     | Remember this host under NAME (new hosts only; auto-assigns `device<N>` if omitted)                         |
| `-relay`                                | off        | Request a TURN relay up front instead of only on fallback (ICE still prefers a direct path if one succeeds) |
| `-norelay`                              | off        | Refuse STUN/TURN entirely -- host candidates only, so a connection that would need a relay fails instead. Answers whether the direct path actually works. |
| `-nosave`                               | off        | Do not write this device to `~/.bitbang/devices.json`. That table stores the access code, which is a working credential -- so this is what you want on a machine that is not yours. Cannot be combined with `-name`. |
| `-pin PIN`                              | (prompt)   | PIN to send if the listener requires one (skips the interactive prompt)                                     |
| `-timeout DUR`                          | `30s`      | Dial timeout (e.g. `45s`, `1m`)                                                                             |
| `-server HOST`                          | `bitba.ng` | Signaling server -- **pair-code mode only**; the URL form carries its own host                              |
| `-v`                                    | off        | Verbose logging                                                                                             |

### `bitbang cp <src> <dst>` -- copy files

Exactly one of `<src>` / `<dst>` is remote, written `<URL>:/path` (URL in any form accepted by `connect`). `-` means stdin/stdout, so `cp <URL>:/f -` streams to stdout and `cp - <URL>:/f` uploads from stdin. A trailing `/` or `.` on the local side keeps the remote basename (scp-style).

| Flag           | Default  | Description                                     |
| -------------- | -------- | ----------------------------------------------- |
| `-relay`       | off      | Request a TURN relay up front (as in `connect`) |
| `-pin PIN`     | (prompt) | PIN to send if required                         |
| `-timeout DUR` | `30s`    | Dial timeout                                    |
| `-v`           | off      | Verbose logging                                 |

### Device names & the known-hosts table

Every successful connect or pairing is remembered in `~/.bitbang/devices.json` (mode `0600`), so you can reconnect by a short name instead of a URL or code:

```
bitbang connect 482731 -name nas1     # pair once, save it as "nas1"
bitbang connect nas1                  # thereafter, just the name
```

- **`-name NAME`** chooses the name; it applies only to a *new* host. Without it, an auto name (`device1`, `device2`, …) is assigned and printed (`Saved as "device1".`).
- **Naming rules:** a name must start with a letter and contain only letters, digits, `-`, or `_`. That guarantees it can never be mistaken for a 6-digit code or a URL. Lookups and uniqueness are case-insensitive.
- **No renaming via connect:** `bitbang connect nas1 -name nas2` is rejected -- `-name` is for first-time saves only.
- **When it's saved:** a pairing is recorded as soon as the SAS is verified (so a flaky reconnect doesn't lose it); a URL connect is recorded once connected.
- Each entry stores `{name, uid, access_code, server, paired_at}`. Reconnecting a known host (by name or URL) refreshes it in place and keeps the name.
