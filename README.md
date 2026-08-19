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
| `POST` | `/api/users/delete-all` | Delete every managed user |
| `GET` | `/api/health` | Check config, Xray service, and TCP listener |
| `GET` | `/api/status` | Return node/protocol metadata |
| `POST` | `/api/start`, `/api/stop`, `/api/restart` | Control Xray |
| `GET` | `/api/stats` | Reserved; returns 501 in this first version |

User mutations are durable. The API writes a temporary configuration, asks
Xray to validate it, atomically replaces the active file, and restarts Xray.
If the restart fails, it restores the original configuration and attempts to
start Xray with that original configuration again.

## Fresh-node installation

The installer intentionally refuses to overwrite an existing Xray
configuration. A suitable REALITY target must be selected and supplied for the
node; it is not safe for a generic installer to silently choose one for every
VPS.

After a release exists:

```bash
curl -fsSLo /tmp/install-vless.sh \
  https://raw.githubusercontent.com/akromjon/vless-api/main/install.sh

sudo REALITY_TARGET=www.example.com:443 \
  PUBLIC_ADDRESS=203.0.113.10 \
  bash /tmp/install-vless.sh
```

For a local binary before the first GitHub release:

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

## Development

```bash
go test ./...
go vet ./...
bash -n install.sh build.sh
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
