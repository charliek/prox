# Remote Proxy Hub

Hub mode lets `prox up` on one machine publish its services through a shared
proxy daemon running on **another** machine, so a hostname like
`https://auth.llt.stridelabs.ai` works from any device that can reach the
hub — a teammate's laptop, a phone, or a browser on the machine that started
the hub itself. It is the multi-machine sibling of the
[shared proxy daemon](shared-proxy.md): the same per-user daemon that lets
several local projects share `:443` also gains an optional network control
plane and a reverse tunnel.

## When you want this

Local `*.local.stridelabs.ai` hostnames only ever resolve to `127.0.0.1` —
useful on the machine you're sitting at, useless anywhere else. Hub mode is
for when the process you want to reach isn't there:

- A coding agent runs `prox up` inside a VM or container that has outbound
  network access but is not reachable from anywhere else — not even from the
  host it runs on. It can dial *out* to a hub; nothing can dial *in* to it.
- Your dev server is behind **CGNAT** (Carrier-Grade NAT — the private
  `100.64.0.0/10` address range a VPN mesh like Tailscale hands out to each
  device; from outside that mesh the address is unreachable, the same way
  home routers hide devices behind one public IP).
- You want to open what you're building on a phone, which has no `/etc/hosts`
  and no mkcert root CA installed for your local domain.

Hub mode does not replace the local shared proxy — it runs alongside it.
`prox up` registers locally exactly as it does today, and hub publishing is
strictly additive: turning it off, or having the hub unreachable, never
breaks the local routes you already had.

## Terminology

- **Hub** — the long-lived `prox` daemon, running with hub mode on, that owns
  the public hostnames for a domain (e.g. `llt.stridelabs.ai`).
- **Publisher** — any `prox up` that registers its services with a hub. It is
  the same process that also registers with the local shared daemon; nothing
  else changes about how you run it.
- **Tunnel** — the single outbound, multiplexed connection a publisher holds
  open to the hub. The hub reaches the publisher's services **only** through
  this tunnel — it never dials the publisher directly, which is what makes a
  VM with no inbound reachability work at all.

## Setting up the hub host

The hub host is a machine that is reachable from wherever you want to browse
from — typically a machine on your Tailscale tailnet (a private mesh network;
"tailnet" is Tailscale's name for it) or a home server on your LAN. It already
needs to be running `prox` for at least one local project, or willing to.

### 1. Point DNS at the hub

Hub mode does not manage DNS for you. Create one wildcard record pointing at
the hub host's address on the network you want to reach it from — typically
its tailnet IP:

```text
*.llt.stridelabs.ai.  A  100.82.128.123
```

Every hostname the hub publishes (`auth.llt.stridelabs.ai`,
`app.llt.stridelabs.ai`, …) resolves through this one record. Off that
network, the address is unreachable (CGNAT addresses aren't routable from the
public internet), which is the point — it's not a public exposure.

### 2. Start the hub

```bash
prox hub start --domain llt.stridelabs.ai
```

This starts the shared daemon if it isn't already running, turns on hub mode,
and writes `~/.prox/hub.yaml`. On a first run, `--domain` is required (there's
no default to guess) and everything else defaults sensibly: the daemon binds
its control plane to this machine's own tailnet address on port `8443`,
publishes hub routes on `443` (HTTPS only), and requires a bearer token. It
prints something like:

```text
Hub mode: on
  Domain:     llt.stridelabs.ai
  Listen:     100.82.128.123:8443
  HTTPS port: 443
  HTTP port:  off (0)
  Auth:       token
  Token file: /home/charlie/.prox/hub.token
  Publishers: none

Run this on each publisher machine (it contains the token VALUE -- treat the
line as a secret; the token file above exists only on this machine):
  prox hub add llt http://100.82.128.123:8443 --token <value> --default
```

Copy that last line — it's ready to paste on every machine that will publish
through this hub. It carries the token **value**, not the path printed above
it: `~/.prox/hub.token` is a file on the hub host and names nothing on a
publisher machine.

If this machine has no tailnet address, `prox hub start` falls back to
loopback and says so — only processes on the hub host itself can publish
until you re-run it with `--listen <address>:8443` naming a reachable one.

Certificates need no setup: the hub generates an mkcert wildcard certificate
for `llt.stridelabs.ai` on first registration, exactly as the local proxy
does for `local.stridelabs.ai`.

### 3. Trust the certificate on a phone or other device

mkcert's certificates are trusted automatically wherever mkcert has run
`-install` (normally just the hub host itself). Any *other* device that will
open a hub-published hostname in a browser — a phone, a teammate's laptop —
needs the mkcert root CA installed once:

1. On the hub host, find the CA: `mkcert -CAROOT` prints the directory; the
   file you want is `rootCA.pem`.
2. Transfer it to the device (AirDrop, a file share, email to yourself —
   anything that gets one file onto the device).
3. Install it as a trusted root. On iOS: open the file, install the profile
   under Settings, then separately enable full trust for it under
   **Settings → General → About → Certificate Trust Settings**. On Android:
   Settings → Security → Encryption & credentials → Install a certificate →
   CA certificate.

This is a one-time step per device. It's the same mkcert CA the hub host
already trusts for its own local domains — nothing hub-specific about the
certificate itself.

### Turning the hub off

```bash
prox hub stop
```

Closes every publisher's tunnel, removes their routes, and shuts down the
network control plane. The shared daemon itself keeps running if it still has
local (non-hub) projects registered, and exits once it's empty — same rule as
always.

While hub mode is **on**, the daemon does not exit just because no local
project is registered: a hub with connected publishers is a reason to stay
alive on its own.

## Publishing a project

On any machine that can reach the hub — a laptop, a VM, a container:

### 1. Add the hub connection

```bash
prox hub add llt http://100.82.128.123:8443 --token <value> --default
```

This is the exact line `prox hub start` printed above. It writes
`~/.prox/hubs.yaml` — a per-user file, so every project on this machine can
use the alias `llt` without repeating the URL or token.

```yaml
# ~/.prox/hubs.yaml
default: llt
hubs:
  llt:
    url: http://100.82.128.123:8443
    token: <value>
```

`~/.prox/hubs.yaml` is `0600` because it can hold a secret. Prefer
`--token-file <path>` or `--token-env VAR` over `--token` when you'd rather
not have the literal value sitting in this file — see
[Configuration Reference](../reference/configuration.md#hubs) for all three
forms.

### 2. Publish

```bash
prox up -d --hub llt
```

`--hub` requires a value — `prox up --hub llt`, not a bare `--hub` — because
`prox up` also takes process names as positional arguments, and an
optional-value flag would make `prox up --hub llt` ambiguous between
"publish to hub `llt`" and "publish to the default hub and start a process
named `llt`". Use `--hub default` to mean "whatever `~/.prox/hubs.yaml` calls
its default hub".

A successful publish prints one extra preamble line:

```text
Hub (llt): https://*.llt.stridelabs.ai — auth.llt.stridelabs.ai, authapi.llt.stridelabs.ai
```

and `curl -sk https://auth.llt.stridelabs.ai/` from any machine that can
resolve and reach the hub now reaches `localhost:3000` on the **publisher**,
through the tunnel — including a WebSocket upgrade, SSE, and ordinary
keep-alive connections, all of which work because the hub sees an ordinary
TCP connection down the tunnel, not something hub-specific.

### Publishing from `prox.yaml` instead of a flag

Add `hub: <alias>` under `proxy:` to make a project publish by default, with
no flag needed:

```yaml
proxy:
  enabled: true
  https_port: 443
  domain: local.stridelabs.ai
  hub: llt

services:
  auth: 3000
  authapi: 8000
```

`hub: llt` here means "the hub aliased `llt`, resolved from
`~/.prox/hubs.yaml`, or from a `hubs:` block in this same file if it defines
`llt` itself" — see [Configuration Reference](../reference/configuration.md#hubs)
for a fully self-contained project file that needs no per-user setup at all.

Precedence, highest first: `--hub <alias>` on the command line, then the
`PROX_HUB` environment variable, then `proxy.hub` in `prox.yaml`. `--no-hub`
disables publishing for one run regardless of what `prox.yaml` says.

**An alias committed in `prox.yaml` is never fatal.** If a teammate checks out
this project on a machine with no `llt` hub configured, `prox up` prints one
warning and starts normally — local routes work exactly as if `hub:` were
absent. An alias named explicitly on the command line (`--hub llt`) is
different: you typed it, so an unknown alias is a mistake you can fix right
now, and `prox up` refuses to start rather than silently ignore it.

## Walkthrough 1: publish and reach it from another device

```bash
# hub host
prox hub start --domain llt.stridelabs.ai

# publisher
prox hub add llt http://<hub tailnet ip>:8443 --token <value> --default
prox up -d --hub llt
```

From any device that resolves `*.llt.stridelabs.ai` to the hub (a phone with
the mkcert CA installed, a teammate's laptop, or the hub host's own browser):

```text
https://auth.llt.stridelabs.ai
```

reaches the publisher's `localhost:3000`, wherever that publisher actually
runs — including inside a VM with no inbound reachability at all, since the
tunnel is outbound-only from the publisher's side.

## Walkthrough 2: a name collision and takeover

Two publishers registering the same service name (`auth`, say) is resolved by
whether the first one is still connected:

```bash
# publisher A
prox up -d --hub llt   # auth.llt.stridelabs.ai now points at A

# publisher B, same service name, while A is still connected
prox up -d --hub llt   # warns and starts WITHOUT the hub route
```

`prox up -d` (or any non-interactive run) never blocks waiting for an answer
— it warns and continues locally. A foreground `prox up` in a real terminal
asks instead:

```text
Hub llt already publishes 1 service name(s) this project registers:
  auth.llt.stridelabs.ai — mac-a:/home/charlie/auth (connected)
Take it over? [y/N]:
```

To take the name without being asked — scripted runs, CI, or just because you
know you want it — pass `--hub-takeover`:

```bash
prox up -d --hub llt --hub-takeover
```

This closes A's tunnel and removes **its entire hub registration**, not just
the contested name — a half-published project (one service reachable, another
silently dropped) is a worse outcome than a cleanly displaced one. A's next
`prox status` shows:

```text
Hub: llt (displaced: auth held by mac-b:/home/charlie/auth)
```

If A had already disconnected (network blip, laptop asleep) when B registers,
B takes the name with no prompt at all — an inactive holder is simply
replaced.

## Walkthrough 3: the hub is unreachable, then recovers

Nothing about this needs to be planned for — it's what happens whenever a hub
you've configured happens to be down when you run `prox up`:

```bash
prox up -d --hub llt   # hub is down (refused, DNS failure, or a timeout)
```

`prox up` still exits `0`. Local routes work exactly as without a hub. Exactly
one warning prints:

```text
Warning: hub llt unreachable (connection refused); continuing with local proxy only, will keep retrying
```

`prox status` shows the ongoing state:

```text
Hub: llt (reconnecting, down 12s)
```

and prox keeps retrying, silently, in the background — nothing further is
printed to the terminal, but each state change is logged to `.prox/prox.log`.
When the hub comes up, the routes appear on their own, with no command to
re-run:

```text
Hub: llt (connected, 2 routes)
```

If the tunnel actually drops mid-run (the hub restarts, a network partition),
the hub serves a small "publisher offline" page in place of the vanished
routes for `60s`, then — because the sweep that removes them runs every `30s`
— the routes are gone by `90s` at the latest if the publisher hasn't
reconnected by then. A publisher that reconnects inside that window reclaims
its routes with no user action and no conflict.

## Security

Read this before you decide to run a hub on anything other than a fully
trusted set of machines.

**One shared token, not one per publisher.** The hub authenticates with a
single bearer token (`~/.prox/hub.token` on the hub host,
`~/.prox/hubs.yaml` on each publisher). Every publisher that can reach the
hub and present that token holds the *same* credential — there is no
per-machine or per-project identity underneath it.

**Composition prevents accidents, not malice.** Every registration is keyed
by `<origin>:<project directory>`, where `origin` is a name the publisher
sends about *itself*. This makes cross-tenant access **impossible by
accident**: an honest publisher can only ever construct its own key, and no
key a publisher can construct is shaped like a *local* (non-hub) project's
key at all — a local project on the hub host can never be named by a remote
caller, full stop. But a publisher that **deliberately** sends another
publisher's origin string reaches that publisher's registration: it can
deregister it, read its captured requests, or replace its tunnel outright.
This is the accepted trust posture for v1 — mutually trusted machines, the
same assumption a Tailscale tailnet already makes of its members — and it is
demonstrated on purpose by a named test in the suite
(`TestHubOrigin_ForgedOriginReachesAnotherPublisher_KnownLimitation`), so it
is a documented decision rather than an unnoticed hole.

**The control plane is plain HTTP, not HTTPS.** The register/deregister/
tunnel endpoints speak unencrypted HTTP with the bearer token in an
`Authorization` header. That is why the hub's listen address defaults to
loopback and the CGNAT range (`100.64.0.0/10`) only — traffic on a tailnet is
already encrypted end to end at the network layer (WireGuard, in Tailscale's
case), so the token is never in cleartext on a wire a stranger shares. Any
other private range — plain LAN addresses like `10.0.0.0/8`, `172.16.0.0/12`,
`192.168.0.0/16`, or IPv6 **ULA** (Unique Local Address, `fc00::/7` — the
IPv6 equivalent of a private range) — needs an explicit opt-in
(`--allow-unencrypted-lan`, or `allow_unencrypted_lan: true` in
`hub.yaml`), because a shared LAN (a café, a hotel, a co-working space) is
private but not confidential: anyone else on it can read the token off the
wire with a passive capture, then exercise the same-token limitation above as
if they'd been handed the credential directly. `0.0.0.0`, `::`, and any
public address are refused outright, with no opt-in — those aren't a judgment
call.

**What closes the gap.** Per-origin credentials — a distinct token (or an
opaque per-registration lease) issued to each publisher rather than one
shared secret — would remove the forged-origin risk above entirely. That is
explicitly out of scope for this release (see
[the discovery doc](../discovery/remote-proxy-hub.md) and plan 031 §8) and is
the natural next step if you need to publish across machines you don't fully
trust each other.

**Bottom line:** hub mode is designed for machines you already trust with
each other's traffic — your own devices, or a small team's tailnet — not for
publishing across an untrusted boundary.

## Troubleshooting

**`prox hub start` refuses my listen address.** By default only loopback and
CGNAT (tailnet) addresses are accepted — see the security section above. Add
`--allow-unencrypted-lan` if you've deliberately decided a plain LAN address
is acceptable, or use a tailnet address instead.

**A publisher gets `401`.** The token it's using doesn't match what the hub
currently accepts — check `prox hub token` on the hub host against the
`token`/`token_file`/`token_env` the publisher resolved (`prox hub list` on
the publisher shows which alias resolves to which URL). A token rotation
(`prox hub token --rotate`) invalidates the old value immediately for anyone
who hasn't been given the new one.

**`prox proxy routes` on the hub host doesn't change columns.** The `SOURCE`
column (and the `tunnel -> host:port` target rendering) only appears once at
least one hub-origin route is registered — with none, the table is
byte-identical to a hub-less daemon's. See
[`prox proxy routes`](../reference/cli.md#proxy).

**The hub is up but a hostname 404s or connection-refuses.** Confirm the
service is actually registered: `prox hub status` on the hub host lists every
publisher and route count. If the publisher shows `connected: false`, its
tunnel is down — that publisher's own `prox status` will show `Hub: … 
(reconnecting, …)`.

**Certificates.** The hub generates its own mkcert wildcard exactly like the
local proxy — there's nothing hub-specific to configure. If a device other
than the hub host doesn't trust it, it's missing the mkcert root CA install
step above.
