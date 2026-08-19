#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_VERSION="0.1.0"
REPOSITORY="akromjon/vless-api"
XRAY_INSTALL_URL="${XRAY_INSTALL_URL:-https://raw.githubusercontent.com/XTLS/Xray-install/main/install-release.sh}"
XRAY_VERSION="${XRAY_VERSION:-v26.3.27}"

API_PORT="${API_PORT:-8080}"
VLESS_PORT="${VLESS_PORT:-443}"
REALITY_TARGET="${REALITY_TARGET:-}"
REALITY_SERVER_NAME="${REALITY_SERVER_NAME:-}"
PUBLIC_ADDRESS="${PUBLIC_ADDRESS:-}"
API_TOKEN="${API_TOKEN:-}"
VLESS_FLOW="${VLESS_FLOW:-xtls-rprx-vision}"
VLESS_INBOUND_TAG="${VLESS_INBOUND_TAG:-vless-reality}"
VLESS_API_BINARY="${VLESS_API_BINARY:-}"
VLESS_API_RELEASE_TAG="${VLESS_API_RELEASE_TAG:-latest}"

XRAY_CONFIG_FILE="/usr/local/etc/xray/config.json"
API_CONFIG_DIR="/etc/vless-api"
API_INSTALL_PATH="/usr/local/bin/vless-api"
API_SERVICE_FILE="/etc/systemd/system/vless-api.service"

temporary_directory=""

cleanup() {
	if [[ -n "${temporary_directory}" && -d "${temporary_directory}" ]]; then
		rm -rf -- "${temporary_directory}"
	fi
}

on_error() {
	local exit_code=$?
	echo "vless-api installation failed on line ${BASH_LINENO[0]} (exit ${exit_code})." >&2
	exit "${exit_code}"
}

trap cleanup EXIT
trap on_error ERR

require_root() {
	if [[ "$(id -u)" -ne 0 ]]; then
		echo "Run this installer as root." >&2
		exit 1
	fi
}

validate_port() {
	local name=$1
	local value=$2
	if [[ ! "${value}" =~ ^[0-9]+$ ]] || ((value < 1 || value > 65535)); then
		echo "${name} must be between 1 and 65535." >&2
		exit 1
	fi
}

install_dependencies() {
	if command -v apt-get >/dev/null 2>&1; then
		apt-get update
		DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl openssl unzip
	elif command -v dnf >/dev/null 2>&1; then
		dnf install -y ca-certificates curl openssl unzip
	elif command -v yum >/dev/null 2>&1; then
		yum install -y ca-certificates curl openssl unzip
	else
		echo "Supported package managers: apt-get, dnf, or yum." >&2
		exit 1
	fi
}

detect_architecture() {
	case "$(uname -m)" in
		x86_64 | amd64) echo "amd64" ;;
		aarch64 | arm64) echo "arm64" ;;
		*)
			echo "Unsupported architecture: $(uname -m)" >&2
			exit 1
			;;
	esac
}

install_api_binary() {
	local architecture=$1
	local binary_source="${temporary_directory}/vless-api"

	if [[ -n "${VLESS_API_BINARY}" ]]; then
		if [[ ! -f "${VLESS_API_BINARY}" ]]; then
			echo "VLESS_API_BINARY does not exist: ${VLESS_API_BINARY}" >&2
			exit 1
		fi
		install -m 0755 "${VLESS_API_BINARY}" "${API_INSTALL_PATH}"
		return
	fi

	local asset="vless-api-linux-${architecture}"
	local base_url
	if [[ "${VLESS_API_RELEASE_TAG}" == "latest" ]]; then
		base_url="https://github.com/${REPOSITORY}/releases/latest/download"
	else
		base_url="https://github.com/${REPOSITORY}/releases/download/${VLESS_API_RELEASE_TAG}"
	fi
	curl -fsSL --retry 3 -o "${binary_source}" "${base_url}/${asset}"
	curl -fsSL --retry 3 -o "${binary_source}.sha256" "${base_url}/${asset}.sha256"
	local expected
	expected="$(awk '{print $1}' "${binary_source}.sha256")"
	local actual
	actual="$(sha256sum "${binary_source}" | awk '{print $1}')"
	if [[ -z "${expected}" || "${expected}" != "${actual}" ]]; then
		echo "vless-api binary checksum verification failed." >&2
		exit 1
	fi
	install -m 0755 "${binary_source}" "${API_INSTALL_PATH}"
}

require_root

if [[ "$(uname -s)" != "Linux" ]]; then
	echo "This installer supports Linux nodes only." >&2
	exit 1
fi
if ! command -v systemctl >/dev/null 2>&1; then
	echo "systemd is required." >&2
	exit 1
fi
if [[ -z "${REALITY_TARGET}" ]]; then
	echo "REALITY_TARGET is required, for example REALITY_TARGET=www.example.com:443." >&2
	exit 1
fi
if [[ ! "${REALITY_TARGET}" =~ ^([A-Za-z0-9.-]+):([0-9]+)$ ]]; then
	echo "REALITY_TARGET must be a hostname and port, for example www.example.com:443." >&2
	exit 1
fi
target_host="${BASH_REMATCH[1]}"
target_port="${BASH_REMATCH[2]}"
validate_port "REALITY_TARGET port" "${target_port}"
validate_port "API_PORT" "${API_PORT}"
validate_port "VLESS_PORT" "${VLESS_PORT}"

if [[ -z "${REALITY_SERVER_NAME}" ]]; then
	REALITY_SERVER_NAME="${target_host}"
fi
if [[ ! "${REALITY_SERVER_NAME}" =~ ^[A-Za-z0-9.-]+$ ]]; then
	echo "REALITY_SERVER_NAME must be a hostname." >&2
	exit 1
fi
if [[ "${VLESS_FLOW}" != "xtls-rprx-vision" && -n "${VLESS_FLOW}" ]]; then
	echo "VLESS_FLOW must be empty or xtls-rprx-vision." >&2
	exit 1
fi
if [[ ! "${XRAY_VERSION}" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "XRAY_VERSION must be a release such as v26.3.27." >&2
	exit 1
fi
if [[ ! "${VLESS_INBOUND_TAG}" =~ ^[A-Za-z0-9_-]{1,64}$ ]]; then
	echo "VLESS_INBOUND_TAG contains unsupported characters." >&2
	exit 1
fi
if [[ -e "${XRAY_CONFIG_FILE}" ]]; then
	echo "Refusing to overwrite existing Xray configuration: ${XRAY_CONFIG_FILE}" >&2
	echo "This initial installer is for a fresh node." >&2
	exit 1
fi

install_dependencies

if [[ -z "${API_TOKEN}" ]]; then
	API_TOKEN="$(openssl rand -hex 32)"
fi
if [[ ! "${API_TOKEN}" =~ ^[A-Za-z0-9._~-]{16,256}$ ]]; then
	echo "API_TOKEN must contain 16-256 URL-safe characters." >&2
	exit 1
fi

if [[ -z "${PUBLIC_ADDRESS}" ]]; then
	PUBLIC_ADDRESS="$(curl -4 -fsS --max-time 10 https://api.ipify.org || true)"
fi
if [[ -z "${PUBLIC_ADDRESS}" || ! "${PUBLIC_ADDRESS}" =~ ^[A-Za-z0-9:._-]+$ ]]; then
	echo "PUBLIC_ADDRESS could not be detected; provide it explicitly." >&2
	exit 1
fi

if ! id xray >/dev/null 2>&1; then
	useradd --system --no-create-home --shell /usr/sbin/nologin xray
fi

temporary_directory="$(mktemp -d)"
curl -fsSL --retry 3 -o "${temporary_directory}/xray-install.sh" "${XRAY_INSTALL_URL}"
bash "${temporary_directory}/xray-install.sh" install --install-user xray --version "${XRAY_VERSION}"

key_output="$(/usr/local/bin/xray x25519)"
reality_private_key="$(awk -F': ' '/^PrivateKey:/ {print $2}' <<<"${key_output}")"
reality_public_key="$(awk -F': ' '/^(Password \(PublicKey\)|Password|PublicKey):/ {print $2; exit}' <<<"${key_output}")"
short_id="$(openssl rand -hex 8)"
if [[ -z "${reality_private_key}" || -z "${reality_public_key}" ]]; then
	echo "Could not generate the REALITY X25519 key pair." >&2
	exit 1
fi

install -d -o root -g xray -m 0750 "$(dirname "${XRAY_CONFIG_FILE}")"
config_candidate="${temporary_directory}/config.json"
cat >"${config_candidate}" <<EOF
{
  "log": {
    "loglevel": "warning"
  },
  "inbounds": [
    {
      "tag": "${VLESS_INBOUND_TAG}",
      "listen": "0.0.0.0",
      "port": ${VLESS_PORT},
      "protocol": "vless",
      "settings": {
        "clients": [],
        "decryption": "none"
      },
      "streamSettings": {
        "network": "tcp",
        "security": "reality",
        "realitySettings": {
          "show": false,
          "target": "${REALITY_TARGET}",
          "xver": 0,
          "serverNames": ["${REALITY_SERVER_NAME}"],
          "privateKey": "${reality_private_key}",
          "shortIds": ["${short_id}"]
        }
      },
      "sniffing": {
        "enabled": true,
        "destOverride": ["http", "tls", "quic"],
        "routeOnly": true
      }
    }
  ],
  "outbounds": [
    {
      "protocol": "freedom",
      "tag": "direct"
    },
    {
      "protocol": "blackhole",
      "tag": "block"
    }
  ]
}
EOF
/usr/local/bin/xray run -test -config "${config_candidate}"
install -o root -g xray -m 0640 "${config_candidate}" "${XRAY_CONFIG_FILE}"

architecture="$(detect_architecture)"
install_api_binary "${architecture}"

install -d -o root -g root -m 0700 "${API_CONFIG_DIR}"
cat >"${API_CONFIG_DIR}/.env" <<EOF
API_ADDRESS=0.0.0.0
API_PORT=${API_PORT}
API_TOKEN=${API_TOKEN}
XRAY_CONFIG_FILE=${XRAY_CONFIG_FILE}
XRAY_BINARY=/usr/local/bin/xray
XRAY_SERVICE=xray
SYSTEMCTL_BINARY=systemctl
VLESS_INBOUND_TAG=${VLESS_INBOUND_TAG}
PUBLIC_ADDRESS=${PUBLIC_ADDRESS}
VLESS_PORT=${VLESS_PORT}
VLESS_SERVER_NAME=${REALITY_SERVER_NAME}
VLESS_REALITY_PUBLIC_KEY=${reality_public_key}
VLESS_SHORT_ID=${short_id}
VLESS_FLOW=${VLESS_FLOW}
EOF
chmod 0600 "${API_CONFIG_DIR}/.env"

cat >"${API_SERVICE_FILE}" <<EOF
[Unit]
Description=CHOP VLESS Reality node API
After=network-online.target xray.service
Wants=network-online.target xray.service

[Service]
Type=simple
User=root
Group=root
EnvironmentFile=${API_CONFIG_DIR}/.env
ExecStart=${API_INSTALL_PATH}
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=full
ReadWritePaths=/usr/local/etc/xray
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "${API_SERVICE_FILE}"

systemctl daemon-reload
systemctl enable xray.service
systemctl restart xray.service
systemctl enable --now vless-api.service

health_response=""
for _ in {1..20}; do
	if health_response="$(curl -fsS --max-time 2 -H "key: ${API_TOKEN}" "http://127.0.0.1:${API_PORT}/api/health" 2>/dev/null)"; then
		if [[ "${health_response}" == *'"healthy":true'* ]]; then
			break
		fi
	fi
	sleep 1
done
if [[ "${health_response}" != *'"healthy":true'* ]]; then
	echo "Node API health verification failed: ${health_response}" >&2
	exit 1
fi

echo
echo "VLESS Reality node installed successfully (installer ${SCRIPT_VERSION})."
echo "API: http://${PUBLIC_ADDRESS}:${API_PORT}"
echo "API token: ${API_TOKEN}"
echo "VLESS: ${PUBLIC_ADDRESS}:${VLESS_PORT}/tcp"
echo "Reality SNI: ${REALITY_SERVER_NAME}"
echo "Open TCP ${VLESS_PORT} for clients and restrict TCP ${API_PORT} to the CHOP backend."
