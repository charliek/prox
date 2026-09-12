# Remote Proxy Hub (prox over a tailnet)

## Status

Implemented, 2026-09-12, by plan 031. See the
[Remote Proxy Hub guide](../guides/remote-hub.md) for the shipped feature and
its [reference documentation](../reference/configuration.md#hubs). The plan
followed almost all of the recommendations below, with security corrections
found in panel review: the shared-token trust model is documented explicitly
rather than assumed, and the default listen address narrowed to loopback and
the Tailscale CGNAT range only (the plain-LAN ranges below now need an
explicit `allow_unencrypted_lan` opt-in), because the control plane is plain
HTTP and RFC 1918 alone is not a confidentiality boundary. Deferred with
intent (plan 031 §9, unchanged from this document's own phasing): ACME/
Cloudflare DNS-01 certificates and bring-your-own cert files, control-plane
TLS, a Tailscale Services front door, `via` in `prox requests` and a TUI
filter, per-publisher hostname prefixes, direct-dial (non-tunnel) remote
targets, and running a hub on a host with no local daemon of its own.

## Problem Statement

prox today gives one machine friendly HTTPS hostnames for its local
processes: `*.local.stridelabs.ai` resolves to `127.0.0.1`, and the shared
daemon on that machine routes `auth.local.stridelabs.ai` to `localhost:3000`.
Every piece of that story is single-machine: DNS points at loopback, the
daemon speaks only over `~/.prox/proxy.sock`, and the daemon reaches backends
by dialing `host:port` from its own network namespace.

The way development actually happens now is multi-machine:

- The developer types on **machine A** while an agent works on **machine B**
  (driven through herdr or roost), and wants to open what the agent built.
- The work happens inside a **shed VM** or a **dev container**, which has
  outbound network access but is not reachable from the tailnet at all
  (Apple VZ sheds are not even routable from their own host).
- The developer wants to check the result **on a phone**, which is on the
  tailnet but has no mkcert CA and no `/etc/hosts`.

The goal: run one prox process somewhere on the tailnet, point
`*.llt.stridelabs.ai` at it, and have `prox up` on any machine, VM, or
container publish its services there, so `https://auth.llt.stridelabs.ai`
reaches the right process wherever it runs. Local behavior on each machine
must stay exactly as it is.

## Why the Shared Daemon Cannot Do This Today

The shared daemon (`internal/proxyd`) is the right starting point, but six of
its assumptions are single-machine:

| Assumption | Where | Why it breaks remotely |
|---|---|---|
| Control API is a Unix socket | `client.go`, `server.go` | Agents on other machines cannot reach it |
| Backend is `host:port` dialed by the daemon | `dynamic_proxy.go` builds `http://<host>:<port>` and uses one shared `http.Transport` | The daemon cannot dial `localhost:3000` on machine B, and cannot dial into a shed or container at all |
| Liveness is PID-based | `registry.StalePIDs`, the on-502 probe, `daemon.IsProcessAlive` | PIDs are meaningless across machines |
| Exact version match | `handleRegister` | Every machine on the tailnet would have to upgrade in lockstep before any could register |
| Certificates come from mkcert | `certs.go` | The phone does not trust the mkcert CA; each machine has its own CA unless shared by hand |
| Auto-exits when the last project leaves; binds `:<port>` on all interfaces | `daemon.go`, `dynamic_proxy.go` | A hub is a long-lived service that should bind a chosen address |

Everything else (registry semantics, hostname routing, SNI cert selection,
per-project request rings, capture, the forwarder bridge into the local TUI)
carries over unchanged.

## Terminology

- **Hub**: the long-lived prox process on the tailnet that owns the public
  listeners for a domain such as `llt.stridelabs.ai`.
- **Publisher**: a `prox up` on any machine, VM, or container that registers
  its services with a hub. It is the same process that today registers with
  the local shared daemon.
- **Tunnel**: the single outbound, multiplexed connection a publisher holds to
  the hub. The hub reaches the publisher's services only through it.

## Options Explored

### Where the proxy lives

**A. Tailscale Serve / Funnel.** Zero prox code. Rejected: Serve exposes one
hostname per node (`popos.tail4a5be.ts.net`), so services would have to be
path-routed, which breaks absolute paths, cookies, and the whole reason prox
uses subdomains locally. Funnel is public-internet exposure, which is the
opposite of the goal.

**B. Hub dials publishers directly over the tailnet.** The publisher
registers `auth -> 100.82.128.123:3000` and the hub reverse-proxies to that
address. This is the smallest change to the daemon (it already dials
`host:port`). Rejected as the primary mechanism because it only works when
the dev server binds a non-loopback interface (vite, next, and uvicorn
default to loopback or need flags), and it does not work at all for sheds or
containers, which are the main use case. It could return later as an
optimization for publishers that are real tailnet nodes.

**C. Reverse tunnel hub (chosen).** The publisher opens one outbound
connection to the hub and keeps it open. The hub multiplexes one stream per
proxied connection back over it; the publisher dials `localhost:<port>` on
its own side. This is the ngrok model. It works from anywhere with outbound
reachability to the hub, including VZ sheds and containers, needs no port
exposure on the publisher, and the tunnel doubles as the liveness signal.

**D. shed-ext-proxy (the sketch in shed's `proxy_integration_design.md`).**
A guest binary polls the in-VM prox daemon, publishes routes over the shed
message bus, and a host binary registers them with the host's prox daemon and
proxies through shed's Connect API. Phases 1 to 2 (Connect API, DialService)
shipped; phases 3 to 5 (the proxy extension itself) were never built. Option
C makes those phases unnecessary for the prox case: a publisher inside a shed
reaches the hub over ordinary outbound networking, with no bus, no guest
watcher, and no host binary. The Connect API stays useful for non-prox
tunnels.

### Tunnel transport

| Option | Verdict |
|---|---|
| One HTTPS connection, `Upgrade: prox-tunnel`, then a stream multiplexer (`hashicorp/yamux` or `xtaci/smux`) | **Chosen.** One stream per proxied TCP connection, so the hub's existing `httputil.ReverseProxy` works unchanged with a custom `DialContext`. WebSocket upgrades (vite HMR, Next dev overlay), SSE, and HTTP/1.1 keep-alive all just work because the proxy sees an ordinary `net.Conn`. Same shape as shed's Connect API, reversed. |
| Reverse HTTP/2 (`x/net/http2` server on the publisher's outbound conn) | Considered. Stdlib-only, but WebSocket over HTTP/2 needs RFC 8441 extended CONNECT, which Go's client transport does not do. Dev servers lean on WebSocket, so this is disqualifying. |
| SSH `-R` reverse forwards | Considered. Needs sshd on the hub and key management on every publisher, and a shed's outbound SSH story is not a given. |
| One WebSocket per proxied request | Considered. Per-request handshake cost and no clean way to carry a nested upgrade. |

### Tailnet-native front doors

Tailscale itself can supply the hostname, the DNS, and the certificate. Three
mechanisms exist, and they differ on the one thing prox cares about: one
hostname per service.

**Tailscale Serve on the node (what t3code does).** `npx t3 serve
--tailscale-serve` maps the node's own MagicDNS name to the app over HTTPS,
so the pairing link is `https://<machine>.<tailnet>.ts.net/`; the cert is
provisioned by tailscaled. This is one hostname per node with path mounting
(`--set-path`) as the only way to add more, which is option A above. It suits
a single app (t3code is one app) and does not suit `auth`, `app`, and
`kratos` on separate origins. It also needs tailscaled on the node, so it
does nothing for a shed VM or a container. Worth keeping as a cheap
convenience later (`prox share <service>` shelling out to `tailscale serve
--https=443 --bg localhost:<port>`), not as the design.

**Tailscale Services.** A Service is a named virtual resource with its own
TailVIP and MagicDNS name `<service>.<tailnet>.ts.net`; a host advertises it
with:

```shell
tailscale serve --service=svc:auth --https=443 127.0.0.1:3000
```

Tailscale provisions the cert for the service name and tailscaled terminates
TLS, then forwards plain HTTP (with the service hostname in `Host`) to the
target, which may be local or a remote address. This is per-service
hostnames, which is exactly prox's model, with zero DNS records and zero
cert handling on our side. Constraints that shape how it can be used:

- The service must be defined before a host can advertise it (admin console;
  Tailscale states the API can also define services, endpoint details to
  verify).
- Only **tag-based** identities can host services. On this tailnet the
  laptops are user-owned (`charlie.knudsen@`) and cannot host; `mini1..3` and
  `srv*` are tagged and can.
- Each host needs approval per service unless an `autoApprovers.services`
  policy grants it.
- Clients need Tailscale 1.86+ (1.94+ to skip `accept-routes`); the phone is
  fine.

Consequences: a shed VM or a user-owned laptop still cannot publish directly,
so the tunnel and hub stay. What changes is the hub's front door: on a tagged
host, the hub can advertise `svc:<name>` for every registered service
(shelling out to `tailscale serve`, or via the LocalAPI serve config) pointed
at its own plain-HTTP listener, and Tailscale supplies DNS, VIP, and cert.
The hub never touches certificates in this mode. A zero-code version of this
exists today for a tagged Linux box: run the agent on `mini2`, run the
`tailscale serve --service` line above per port, and `auth.<tailnet>.ts.net`
works on the phone. That is the fastest way to validate the experience
before building anything.

**tsnet (embedded nodes).** The `tailscale.com/tsnet` package runs a full
Tailscale node inside a Go process on a userspace network stack, no
tailscaled required. Two shapes are possible:

- *Hub as one tsnet node* advertising Services via `Server.ListenService`
  (tag-based auth key). Same front door as the previous option, but the hub
  needs no tailscaled and can run anywhere, including inside a container.
- *No hub at all*: `prox up` embeds one ephemeral node per service
  (`Hostname: "auth"`) and serves it with `ListenTLS`, which gets a
  Let's Encrypt cert for `auth.<tailnet>.ts.net`. This works from inside a VZ
  shed with only outbound access and needs neither hub, DNS, nor tunnel.

The costs are real: the tailscale module is large (tens of MB in the binary,
gVisor netstack), every publisher needs a reusable pre-approved auth key, the
per-service-node shape turns every service into a device in the admin
console (with `auth-1` renames on collision), and first-run cert issuance per
new node name takes seconds and is rate-limited per tailnet. It is the most
tailnet-native option and the heaviest dependency prox would ever take.

**Where this lands.** Tailscale changes the answer to "who owns DNS and
certs" (D4, D5), not to "how does traffic reach a shed" (D1, D2). The hub's
front door should be a pluggable choice:

| Front door | Hostname | DNS | Cert | Needs |
|---|---|---|---|---|
| `domain` | `auth.llt.stridelabs.ai` | one Cloudflare wildcard record | LE wildcard via DNS-01 (cron) or mkcert | a domain |
| `tailscale-services` | `auth.tail4a5be.ts.net` | Tailscale | Tailscale | tagged hub host, services pre-defined, approval policy |

Both share the same registry, tunnel, and publisher code; they differ in a
small hub-side "front door" interface (bind listeners and pick certs versus
advertise a service per hostname). The `domain` door keeps the user's own
hostnames and works with no tailnet admin changes; the `tailscale-services`
door removes all DNS and cert setup. Which ships first is an open question
below; building the interface so the second can follow is not.

### Certificates

| Option | Verdict |
|---|---|
| mkcert on the hub host, root CA installed once on the phone | **v1.** It is what the code does today: the cert manager generates `*.llt.stridelabs.ai` on the first registration for that domain. The hub host is the machine that already runs mkcert, so nothing is synced. Android and iOS accept a user CA for browsers, but not for every app. |
| Publicly trusted wildcard for `*.llt.stridelabs.ai` via ACME DNS-01 (stridelabs.ai is on Cloudflare) | **Later, likely wanted.** Works on the phone with no CA install. Step one is bring-your-own cert files (a `lego` or `certbot` cron drops `llt_stridelabs_ai.pem` / `-key.pem` into the certs dir, which the cert manager already prefers over generating). Step two is built-in ACME with a Cloudflare token. |
| `tailscale cert` (Let's Encrypt for the node's `ts.net` name) | Rejected: no wildcards, and the name is the node's, not `*.llt.stridelabs.ai`. |

### DNS

**Chosen:** one public wildcard record, `*.llt.stridelabs.ai A <hub tailnet IP>`,
DNS-only (not Cloudflare-proxied). Every tailnet device resolves it, off-tailnet
it resolves to an unreachable CGNAT address, and the same wildcard covers the
hub's own control hostname (`hub.llt.stridelabs.ai`). MagicDNS does not
interfere with a public domain. Split DNS and `/etc/hosts` were considered and
are strictly more setup for the same result.

### Publisher authentication

| Option | Verdict |
|---|---|
| Bearer token, generated by the hub on first start, copied to publishers once | **Chosen for v1.** Same mechanism prox already uses for a non-loopback API bind (`~/.prox/token`). The tailnet is the perimeter; the token stops a stray process on a tailnet machine from publishing. |
| Tailscale identity (`tailscale whois` / LocalAPI on the peer address) | Considered for later. Free identity for real tailnet nodes, but a shed or container is NATed behind its host, so the hub would see the host's identity, and it pulls in tailscale-specific code. |
| Mutual TLS | Overkill for a single-user tailnet; nothing rules it out later. |

### Liveness

PID liveness is replaced by the tunnel. A registration is a **lease held by
the tunnel connection**: when the connection drops, the routes enter a
`disconnected` state for a grace window (proposed 60s) during which the hub
answers with an "publisher offline" page instead of 502, and a reconnecting
publisher with the same identity reclaims them without a conflict. After the
grace the routes are removed. This reuses the identity tuple the registry
already carries (`project_dir`, `pid`, `start_time`) plus a publisher
`machine_id`, and the idempotent re-register path (`registrationMatches`,
`sameRegistrationIdentity`) applies as-is.

### Version compatibility

The local daemon needs an exact match because it shares a binary and on-disk
state with its clients. Hub and publisher share only a JSON API and a tunnel
framing, so the hub requires a matching **protocol version** (a small integer
in the register request) and merely reports the binary version in status.
This is the same relaxation shed's design asked for, done properly.

### Hub as a mode of the shared daemon (refined 2026-09-11)

The first deployment is the coding machine itself (the Mac that owns mkcert
and already runs the shared daemon on `:443` for `*.local.stridelabs.ai`).
A separate hub process could not bind `:443` next to it, and two processes
serving two domains on one machine is the wrong shape anyway. So the hub is
not a second daemon: it is an **additional interface on the existing shared
daemon**. Local projects keep registering over `~/.prox/proxy.sock` exactly
as today; remote publishers register over a network endpoint and hold a
tunnel. Both feed the same registry and the same `:443` / `:80` listeners,
which already bind all interfaces and already route by hostname, so
`auth.local.stridelabs.ai` and `auth.llt.stridelabs.ai` are two routes in
one table with different target kinds.

What hub mode adds to the daemon:

- a network control listener (register, deregister, status, routes, request
  stream, tunnel upgrade) guarded by a bearer token
- a hub domain the daemon composes remote hostnames from
- tunnel sessions as a route target kind, with lease-based liveness
- a "stay alive while hub mode is on" rule in the empty-daemon shutdown check
- mkcert certs for the hub domain, generated on demand by the cert manager
  the daemon already has (`EnsureDomain`), so v1 has no certificate work at
  all; on the phone the mkcert root CA is installed once by hand, and ACME
  or a Cloudflare integration is a later improvement

Default behavior is unchanged: with no hub config on the machine and no
`hub:` in a project, nothing new runs.

### Configuration surface

Three places, each for a different owner:

**Hub host, per user: `~/.prox/hub.yaml`.** Describes the hub this machine
offers.

```yaml
domain: llt.stridelabs.ai       # remote hostnames are <service>.<domain>
listen: 100.120.127.126:8443    # control + tunnel endpoint; default: this
                                # host's tailnet address, port 8443
https_port: 443                 # data-plane ports remote routes are published on;
http_port: 80                   # the hub decides these, publishers never send ports
autostart: false                # true: any daemon start enables hub mode
```

The token is generated into `~/.prox/hub.token` (0600) on first
`prox hub start` and required by default. A hub can be configured to run
without one (`auth: none` in `hub.yaml`); the daemon then insists the
listen address is private (loopback, RFC 1918, or CGNAT) and `prox hub
start` prints a one-line notice that any process on the network can
publish. Publishers whose hub entry has no token simply send none.
The control endpoint is plain HTTP in v1 because the
tailnet already encrypts and authenticates transport, and a mkcert cert on
the control plane would force every publisher to trust the CA, which is the
cert work being kept out of scope. The daemon refuses to bind the control
endpoint to a public address (anything not loopback, RFC 1918, or CGNAT
`100.64/10`) so a typo cannot expose it. TLS on the control plane arrives
with the cert work later.

**Publisher side: one schema, two places.** A `hubs:` map defines hubs by
alias. The same map is accepted in the project's `prox.yaml` and in a
per-user `~/.prox/hubs.yaml`, so a project can be fully self-describing
while a machine can also carry hubs that no project file mentions. Entries
are merged by alias with the project file winning.

```yaml
# prox.yaml (committed): everything a project needs, in one file
proxy:
  enabled: true
  https_port: 443
  domain: local.stridelabs.ai
  hub: llt                          # publish this project to the hub aliased `llt`

hubs:
  llt:
    url: http://100.120.127.126:8443
    token_file: ~/.prox/hubs/llt.token   # or token_env: PROX_HUB_TOKEN, or token: (discouraged in a committed file)

services:
  auth: 3000
  authapi: 8000
```

```yaml
# ~/.prox/hubs.yaml (per user, optional): the same `hubs:` map plus a default
default: llt
hubs:
  llt:
    url: http://100.120.127.126:8443
    token_file: ~/.prox/hubs/llt.token
```

`proxy.hub` takes an alias, or `default` to mean the user file's default
hub. `prox hub add` writes the user file; the project file is edited by
hand like the rest of `prox.yaml`. Secrets stay out of committed files by
convention: `token_file` and `token_env` are the normal forms and `token:`
inline is accepted with a one-line warning when the file is inside a git
work tree.

A publisher sends service **names** and its own side's targets
(`localhost:3000`), plus identity (machine name, project dir, PID, start
token, protocol version) and its capture settings. It never sends a domain
or ports; the hub composes `auth.llt.stridelabs.ai` and publishes it on the
hub's ports. Publishing is **additive**: the local registration or standalone
fallback happens first and is untouched, so one machine serves
`auth.local.stridelabs.ai` and `auth.llt.stridelabs.ai` at once from the
same daemon; they differ only in hostname and target kind.

**Precedence and the unknown-alias rule.** `--hub[=alias]` on the command
line beats `PROX_HUB`, which beats `proxy.hub` in `prox.yaml`; `--no-hub`
disables all of them for one run. A `proxy.hub` alias that resolves to
nothing (not in the project's `hubs:` and not in the user file) prints one
warning and continues without a hub, so a committed `hub: llt` never breaks
`prox up` on a machine without that hub. An explicit `--hub llt` naming an
unknown alias is an error, because the user asked for it by name.

### CLI surface

Hub host:

| Command | Effect |
|---|---|
| `prox hub start` | Ensure the shared daemon is running and switch hub mode on (runtime enable over the socket, reading `hub.yaml`). Prints domain, listen address, token path, and the ready-to-paste `prox hub add` line for other machines. |
| `prox hub stop` | Switch hub mode off, drop remote routes and tunnels; the daemon exits if no local project is registered. |
| `prox hub status [--json]` | Hub on/off, domain, listen address, connected publishers (machine, project dir, connected since, route count). |
| `prox hub token [--rotate]` | Print the token path, or rotate it (existing publishers reconnect with the new token). |
| `prox proxy routes` | Gains a SOURCE column (`local` / `hub`) and shows tunnel targets as `<machine>:localhost:3000`. |

Publisher:

| Command | Effect |
|---|---|
| `prox hub add <alias> <url> --token-file <path>` (or `--token`) | Write an entry to `hubs.yaml`; `--default` sets the default. |
| `prox hub remove <alias>`, `prox hub list` | Manage aliases. |
| `prox up --hub[=alias]` / `--no-hub` | Publish this run (or not), overriding `prox.yaml`. |
| `prox up --hub-takeover` | Take service names held by a connected publisher without the interactive prompt. |
| `prox status` | New `Hub:` line: `Hub: llt (connected, 2 routes)`, `Hub: llt (reconnecting, down 12s)`, `Hub: llt (conflict: auth already published by mac-mini:/home/c/slauth)`. |
| `prox requests` | Hub-served records carry `via: hub/llt`; a filter comes with the TUI work. |

`prox up` output gains one line:

```text
Proxy (shared daemon): https://*.local.stridelabs.ai:443
Registered domains: auth.local.stridelabs.ai, authapi.local.stridelabs.ai
Hub (llt): https://*.llt.stridelabs.ai — auth.llt.stridelabs.ai, authapi.llt.stridelabs.ai
```

### Failure behavior on the publisher

Because publishing is additive, hub problems degrade the hub, never the
project:

| Situation | Behavior |
|---|---|
| Hub unreachable at `prox up` | One warning, project starts, background reconnect keeps trying (same backoff as the forwarder); routes appear when the hub comes up. |
| Service name already published by another publisher whose tunnel is **not** connected (grace window or stale) | The new publisher takes the name. If the old one reconnects later it is told it was displaced, stops retrying that name, and shows `Hub: llt (displaced: auth by mac-mini:/home/c/slauth)`. |
| Service name held by a publisher whose tunnel **is** connected | Interactive `prox up` (a TTY, not `-d`) asks `auth.llt.stridelabs.ai is published by mac-mini:/home/c/slauth. Take it over? [y/N]`. Non-interactive: warning naming the holder, project starts without that route, `prox status` shows the conflict. `prox up --hub-takeover` takes it without asking. Taking over displaces the holder as above. Local daemon rules are unchanged. |
| Protocol version mismatch | Warning, no hub for this run, local untouched. The socket path keeps its exact-version rule; the network path checks a protocol version only. |
| Tunnel drops mid-run | Hub keeps the routes for a 60s grace serving an "offline" page, publisher reconnects and reclaims them by identity; after the grace they are removed. |
| Hub host is also the publisher | Allowed; the tunnel runs over loopback. A direct-target shortcut is a later optimization. |

### Hub lifecycle

Hub mode is switched on explicitly by `prox hub start` (or on every daemon
start when `autostart: true`). While it is on, the empty-daemon shutdown
check treats "hub enabled" as a reason to stay alive, so the daemon outlives
the last local project. `prox hub stop` or `prox proxy stop --force` ends it.
State stays under `~/.prox/` alongside the daemon's existing files
(`hub.yaml`, `hub.token`, and hub fields in `proxy.state`).

## Proposed Design

```
machine A (browser)            hub host on the tailnet                 machine B / shed / container
                               ┌──────────────────────────────┐
https://auth.llt.stridelabs.ai │ shared daemon, hub mode on   │        prox up --hub llt
  DNS: *.llt -> 100.x.y.z ────▶│ :443  TLS (mkcert, v1)       │
                               │   SNI/Host -> Registry       │ tunnel  ┌──────────────────────┐
                               │   local route -> localhost   │         │ tunnel client        │
                               │   tunnel route: DialContext ─┼────────▶│  stream -> dial      │
                               │     = open yamux stream      │ (one    │  localhost:3000      │
                               │                              │ outbound│                      │
                               │ :8443 control API (HTTP+token)│ conn)  │                      │
                               │   /api/v1/register           │◀────────┤ register / heal      │
                               │   /api/v1/tunnel (upgrade)   │         │ forwarder (SSE) ─┐   │
                               │   /api/v1/requests[/stream]  │────────▶│ local RM -> TUI  │   │
                               │   per-project rings, capture │         └──────────────────────┘
                               └──────────────────────────────┘
```

**Hub mode in the daemon.** The existing `RunDaemon` pieces (Registry,
DynamicProxy, Managers, CaptureManager, cert manager, Server) stay as they
are, plus: the `Server`'s chi router is mounted a second time on a TCP
listener behind a bearer-token middleware (the socket mount stays
token-free); routes carry a target that is either `host:port` (local, as
today) or `tunnel:<session id>:<port>`; the stale-PID sweep keeps reaping
local registrations while a lease sweep driven by tunnel disconnects reaps
remote ones; and the empty-daemon shutdown check consults the hub flag.

**Zero-code check available today.** The Mac's daemon already binds `:443`
on every interface and routes by hostname, and mkcert already generates a
cert for any registered domain. A `prox.yaml` on the Mac with
`domain: llt.stridelabs.ai` and `services: {auth: {host: 100.82.128.123,
port: 3000}}` (a popos dev server bound to `0.0.0.0`) routes the phone to
popos right now. It validates DNS, the cert, and the phone flow before any
code is written; what it cannot do is reach a loopback-bound server, a shed,
or a container, which is what the tunnel is for.

**Route target as a dialer.** `DynamicProxy` today builds the target URL and
uses one shared transport. The change is a per-request transport whose
`DialContext` looks up the route's tunnel session and opens a stream, then
writes a one-line preamble (`CONNECT <port>\n`, answered `OK\n` or
`ERR <reason>\n`, mirroring shed's vsock protocol) before handing the stream
to `ReverseProxy`. Everything after that line is byte-for-byte the local
path, including capture and the request record.

**Publisher side.** `tryDaemonProxy` gains a sibling, `tryHubPublish`, that
runs after the local registration succeeds (or standalone fell back). It
registers over HTTPS, opens the tunnel, serves `CONNECT` requests by dialing
the configured service host and port, and runs the same forwarder against the
hub's SSE stream into the project's local `RequestManager`, so `prox
requests` and the TUI show hub traffic alongside local traffic (records tagged
with their origin). The forwarder's reconnect, backfill, heal, and status-sink
machinery is reused; the heal callback becomes "reconnect tunnel and
re-register".

**Client refactor.** `proxyd.Client` is bound to a socket path. It becomes a
`Client` over an injected `http.RoundTripper` plus base URL and optional
bearer token, so the Unix-socket daemon client and the hub client are the same
type. The forwarder takes a `Client` rather than a socket path.

**Status and inspection.** `prox hub status` is the hub HOST's view — on/off,
domain, listen address, ports, auth mode, and every connected or disconnected
publisher — and sits beside `prox proxy status|routes`. (`prox hub list` is
not a control-plane query at all: it manages the publisher's own hub profiles
in `~/.prox/hubs.yaml`, merged with the current project's `hubs:` block, and
talks to nothing.) `prox status` in a publishing project gains a `Hub:` line
with the same connected/degraded/down states the `Proxy:` line has. Hub-side,
a route whose tunnel is disconnected renders a small HTML "publisher offline
since <t>" page with a `Retry-After`, not a bare 502.

**Certificates on the hub.** *Original proposal, not what shipped:* `hub.yaml`
selects `certs.source: files | mkcert` (v1) with `acme` reserved for v2, where
`files` reuses the cert manager's existing prefer-existing-files behavior and
adds a fsnotify-free periodic reload (mtime check on the SNI path) so a renewed
cert is picked up without a restart.

*What shipped:* `hub.yaml` has no certificate-source field. The hub generates
mkcert certificates on demand exactly as the local proxy does (D13), and
bring-your-own certificate files, the reload loop above, and ACME are all
deferred past v1 (plan 031 §9).

## Resolved Decisions (proposed)

| # | Question | Recommendation |
|---|---|---|
| D1 | Placement | Reverse-tunnel hub (option C); direct dial (B) left as a possible later optimization |
| D2 | Transport | One upgraded HTTPS connection multiplexed with yamux; `CONNECT <port>` preamble per stream |
| D3 | Same binary and same daemon | Yes: hub mode is an additional interface on the existing shared daemon, switched on by `prox hub start`; no second process |
| D4 | Certificates | Behind the front-door interface. `domain` door: v1 bring-your-own files (LE wildcard via DNS-01 cron) or mkcert, v2 built-in ACME. `tailscale-services` door: Tailscale provisions them. **Shipped narrower:** mkcert only in v1 (see D13); bring-your-own files and ACME are deferred |
| D5 | DNS | Behind the front-door interface. `domain` door: one public wildcard A record to the hub's tailnet IP. `tailscale-services` door: MagicDNS |
| D5a | Front door | Pluggable hub-side interface from day one; ship one door in phase 1 (open question 6) |
| D6 | Auth | Bearer token; tailnet is the perimeter. **Shipped without TLS:** the v1 control plane speaks plain HTTP, so the token is protected only by the transport underneath it — which is why the listen address is restricted to loopback and the CGNAT range unless `allow_unencrypted_lan` is set (see D12) |
| D7 | Liveness | Lease held by the tunnel, 60s reconnect grace, then removal |
| D8 | Versioning | Protocol version match, binary version informational |
| D9 | Config | One `hubs:` schema accepted in both `prox.yaml` (project self-describing) and `~/.prox/hubs.yaml` (per user, with `default`), merged by alias with the project winning; `proxy.hub: <alias|default>`; `--hub[=alias]` / `PROX_HUB` / `--no-hub` override; explicit opt-in, default unchanged. `~/.prox/hub.yaml` on the host is machine-level and stays separate |
| D10 | Hostnames and collisions | Hub owns the domain and the ports; publishers send service names. A name whose holder is not connected is taken by the newcomer; a connected holder is kept unless the user confirms interactively or passes `--hub-takeover`; a displaced holder is told and stops retrying. Never a failed `prox up` |
| D15 | Capture and trust | Hub routes keep the publisher's own capture settings and the daemon's existing storage; trusted machines are assumed for now |
| D11 | Lifecycle | `prox hub start` / `stop`; hub mode keeps the daemon alive when no local project is registered; `autostart` opt-in |
| D12 | Control plane | Dedicated plain-HTTP port (default 8443) on the host's tailnet address, refuses public binds; bearer token by default, `auth: none` allowed by explicit config on a private address; TLS on the control plane comes with the cert work |
| D13 | Certificates in v1 | mkcert on the hub host, generated on demand as today; phone installs the root CA once; ACME / Cloudflare later |
| D14 | Front door in v1 | `domain` door only; the front-door seam is kept small so `tailscale-services` can follow |

## Open Questions

These change the plan materially and need a call before the plan is written:

Resolved in the 2026-09-11 refinement: the domain is configurable (test
domain `llt.stridelabs.ai`, already pointed at the Mac's tailnet address);
publishing is explicit opt-in; the hub is a mode of the local shared daemon,
not a second process; the `domain` front door ships first with mkcert certs.

Also resolved: `hubs:` is accepted in `prox.yaml` so a project file can be
self-contained; collisions follow the not-connected-is-replaceable rule with
an interactive prompt or `--hub-takeover` for a connected holder; capture
keeps today's behavior on trusted machines.

Still open:

1. **Noun.** `hub` is used throughout (`prox hub …`, `proxy.hub:`,
   `--hub`). Alternatives: `remote`, `relay`, `upstream`.
2. **Hub host configuration entry point.** `~/.prox/hub.yaml` edited by
   hand, or `prox hub start --domain llt.stridelabs.ai --listen …` writing
   that file on first use (proposed: both, flags persist).

## Phasing

One plan per PR, each independently useful:

1. **Hub core.** Hub mode in the shared daemon (`hub.yaml`, network mount
   of the control API with token, `prox hub start|stop|status|token`),
   tunnel endpoint and publisher tunnel client, tunnel targets and
   lease-based liveness in the registry and `DynamicProxy`, `Client`
   refactor onto a transport, `hubs.yaml` + `prox hub add|remove|list`,
   `proxy.hub` / `--hub` / `--no-hub`, forwarder over the hub with the
   `Hub:` status line, docs (DNS record, phone CA install). This is the plan
   to take to the panel first; it is large and the panel may split it.
2. **Operator polish.** Offline page with reconnect grace, SOURCE column and
   tunnel targets in `prox proxy routes`, `via` in `prox requests` and a TUI
   filter, token rotation.
3. **Certificates.** Control-plane TLS, bring-your-own wildcard files, then
   built-in ACME (Cloudflare DNS-01) so the phone needs no CA install.
4. **Later, if wanted.** Direct-dial targets for real tailnet nodes, tailnet
   identity auth, per-publisher hostname prefixes.

## Relationship to Other Repos

- **shed**: this supersedes phases 3 to 5 of `proxy_integration_design.md`
  for the prox case. A shed image needs only the prox binary and outbound
  reachability to the hub. The shed doc should get a status note once the hub
  ships.
- **slauth and other consumers**: no config change beyond `publish: true` (or
  none, if question 2 resolves to publish-by-default).
- **roost / herdr**: no integration needed; they launch the agent, the agent
  runs `prox up`.
