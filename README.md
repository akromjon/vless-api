# CHOP VLESS Reality node API

`vless-api` is the VLESS/REALITY counterpart to
[`wireguard-api-2`](https://github.com/akromjon/wireguard-api-2). It installs
Xray on a Linux VPS, exposes an authenticated node-management API on TCP 8080,
and provisions VLESS users for a REALITY inbound on TCP 443.

This repository is currently an initial, locally verified implementation. It
has not yet been deployed to a VPS, connected to the CHOP backend, or consumed
by a released mobile app.

## AWG3 mapping

| `wireguard-api-2` | `vless-api` |
| --- | --- |
| AmneziaWG interface | Xray VLESS inbound tagged `vless-reality` |
| UDP listener | TCP 443 listener with REALITY |
| WireGuard public/private keys | Random VLESS UUID per provisioned config |
| Peer name | Xray `email` management label; no real email is used |
| WireGuard `.conf` | `vless://` REALITY share URI |
| Apply peers to `awg0` | Atomically update Xray JSON and restart Xray once |
| `awg show` health/stats | systemd state plus a TCP listener probe |

The CHOP mobile-user UUID and VLESS UUID are separate values. The backend
should store the VLESS UUID as a secret belonging to one VPN configuration or
device.

## API contract

All endpoints require the same `key: <API_TOKEN>` header shape used by
`wireguard-api-2`. Invalid authentication is deliberately returned as 404.

| Method | Endpoint | Purpose |
| --- | --- | --- |
| `GET` | `/api/users` | List provisioned users and share URIs |
| `POST` | `/api/users/add` | Add one user from `{"name":"config_123"}` |
| `POST` | `/api/users/add-bulk` | Add up to 500 names with one Xray restart |
| `POST` | `/api/users/delete` | Delete one user by name |
| `POST` | `/api/users/rotate` | Issue a fresh VLESS UUID for an existing name, revoking the old one |
| `POST` | `/api/users/delete-all` | Delete every managed user |
| `GET` | `/api/health` | Check config, Xray service, and TCP listener |
| `GET` | `/api/status` | Return node/protocol metadata |
| `POST` | `/api/start`, `/api/stop`, `/api/restart` | Control Xray |
| `GET` | `/api/stats` | Per-config traffic counters; `?reset=1` returns deltas. 501 until the Xray API is enabled |

User mutations are durable. The API writes a temporary configuration, asks
Xray to validate it, and atomically replaces the active file. How the running
process is then brought in step depends on whether Xray's gRPC API is enabled:

- **With the API** (`XRAY_API_ADDRESS`, default `127.0.0.1:10085`): users are
  added and removed on the live inbound through `HandlerService`. Xray is never
  restarted, so other users' sessions survive. This is what makes per-release
  credential rotation affordable — without it, rotating one config would
  disconnect every other user on the node.
- **Without it**: Xray is restarted, exactly as before. If the restart fails the
  original configuration is restored and Xray is started with it again.

If a live apply fails, the file is restored and Xray restarted, so the file and
the running process can never silently disagree.

Runtime changes made over the API are not persisted by Xray, which is why the
configuration file is always written first and remains the source of truth
across restarts.

## Enabling the Xray API on a node

`/api/stats` and restart-free user changes both require these blocks in the
node's `config.json`, plus a restart to pick them up:

```json
{
  "api":    { "tag": "api", "services": ["HandlerService", "StatsService"] },
  "stats":  {},
  "policy": {
    "levels": { "0": { "statsUserUplink": true, "statsUserDownlink": true } },
    "system": { "statsInboundUplink": true, "statsInboundDownlink": true }
  },
  "routing": { "rules": [ { "type": "field", "inboundTag": ["api"], "outboundTag": "api" } ] }
}
```

and an extra inbound, appended **after** the VLESS one:

```json
{ "tag": "api", "port": 10085, "listen": "127.0.0.1", "protocol": "dokodemo-door",
  "settings": { "address": "127.0.0.1" } }
```

The inbound is looked up by tag, not by position, so appending is safe. Counters
live in memory: a restart resets them, so a consumer must read a decrease as a
restart rather than as negative traffic.

## Client fingerprint

`VLESS_FINGERPRINT` (default `chrome`) sets the `fp` parameter of the generated
`vless://` URI. Chrome's uTLS profile carries a post-quantum key share, which
pushes the REALITY ClientHello past one MSS and splits it across two TCP
segments. On paths where a middlebox drops the second segment, the handshake
never completes and the client stalls with no reply. Set `ios` or `safari` on
nodes serving such paths to keep the ClientHello in a single segment.

## Fresh-node installation

The installer intentionally refuses to overwrite an existing Xray
configuration. A suitable REALITY target must be selected for the node; it is
not safe for a generic installer to silently choose one for every VPS.

On a fresh Ubuntu server, copy and paste:

```bash
curl -sSL https://raw.githubusercontent.com/akromjon/vless-api/main/install.sh -o install.sh
chmod +x install.sh
sudo ./install.sh
```

The installer asks for the REALITY target as `hostname:port`. It then detects
the server's public IPv4 address, generates the API token, downloads the
matching AMD64 or ARM64 binary from the latest GitHub release, verifies its
SHA-256 checksum, installs the services, and prints the node credentials.

For unattended installation, provide the target without waiting for a prompt:

```bash
sudo REALITY_TARGET=your-selected-target.example:443 ./install.sh
```

For a locally built binary:

```bash
GOOS=linux GOARCH=amd64 OUTPUT=vless-api-linux-amd64 ./build.sh

sudo REALITY_TARGET=www.example.com:443 \
  PUBLIC_ADDRESS=203.0.113.10 \
  VLESS_API_BINARY="$PWD/vless-api-linux-amd64" \
  ./install.sh
```

The installer:

1. Installs Xray using the official
   [`XTLS/Xray-install`](https://github.com/XTLS/Xray-install) project. The
   tested Xray version is pinned by default and can be changed with
   `XRAY_VERSION`.
2. Generates the REALITY X25519 key pair and short ID on the node.
3. Creates an empty VLESS/REALITY TCP 443 inbound.
4. Installs `vless-api` and its root-only environment file.
5. Starts both systemd services and verifies authenticated health locally.

Open TCP 443 to VPN clients. TCP 8080 should be restricted at the provider
firewall to the CHOP backend addresses even though the API also requires a
token.

## Transparent TCP relay installation

Run the relay installer on a separate VPS whose public IP will be shown to
clients. The real VLESS node keeps the REALITY keys, users, TCP 443 listener,
and management API; the relay only forwards raw TCP to it.

On a fresh Ubuntu relay VPS:

```bash
curl -sSL https://raw.githubusercontent.com/akromjon/vless-api/main/install-relay.sh -o install-relay.sh
chmod +x install-relay.sh

sudo ORIGIN_HOST=203.0.113.10 \
  ORIGIN_PORT=443 \
  RELAY_PORT=443 \
  ./install-relay.sh
```

`ORIGIN_HOST` is the real VLESS node IP or hostname. Prefer relay TCP 443 when
it is available; choose another port such as `8444` if another service already
owns 443. The installer refuses port conflicts and forwarding loops, verifies
that the origin is reachable before changing the VPS, installs Xray when
needed, creates a dedicated hardened systemd service, and rotates its access
and error logs daily. Re-running the same command verifies and restarts the
same configuration; different settings are not silently overwritten.

To run more than one isolated relay on the same VPS, give each one a unique
lowercase name and port:

```bash
sudo RELAY_NAME=zurich-vless-relay \
  ORIGIN_HOST=203.0.113.10 \
  RELAY_PORT=8444 \
  ./install-relay.sh
```

After installation, edit the **real VLESS server record** in CHOP admin:

| CHOP field | Value |
| --- | --- |
| Server IP | Real VLESS node IP; do not change it to the relay |
| API port | Real node management API, normally `8080` |
| Relay host | Relay VPS public IP printed by the installer |
| Relay port | `RELAY_PORT` used by the installer |

Do not create a second CHOP server record for the relay. Open the relay port to
clients, allow the relay VPS to reach the origin's TCP 443, and keep the origin
API restricted to the CHOP backend. For a fresh node with no direct clients,
the origin's TCP 443 can additionally be restricted to the relay IP after the
end-to-end connection test passes.

## Development

```bash
go test ./...
go vet ./...
bash -n install.sh install-relay.sh build.sh
shellcheck install.sh install-relay.sh build.sh
```

Release binaries are built and published manually. A SHA-256 file must be
published beside each binary because `install.sh` verifies that checksum before
installation.

## Next integration stages

- Add a `vless` VPN type and a structured VLESS configuration record to the
  Laravel/Rust backend. Do not reuse the mobile-user UUID as the VLESS UUID.
- Add VLESS/REALITY support to the iOS and Android tunnel cores and release new
  app versions before routing customers to these nodes.
- Add Xray `StatsService` and real client traffic probes. A running service and
  open TCP port do not prove that a specific user successfully connected.
- Optionally add Xray `HandlerService` for zero-restart mutations while keeping
  this JSON reconciliation path as the durable source of truth.

Configuration fields follow the official [VLESS](https://xtls.github.io/en/config/inbounds/vless.html)
and [REALITY](https://xtls.github.io/en/config/transports/reality.html)
documentation.
