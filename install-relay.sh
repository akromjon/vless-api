#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_VERSION="0.1.0"
XRAY_INSTALL_URL="${XRAY_INSTALL_URL:-https://raw.githubusercontent.com/XTLS/Xray-install/main/install-release.sh}"
XRAY_VERSION="${XRAY_VERSION:-v26.3.27}"

ORIGIN_HOST="${ORIGIN_HOST:-}"
ORIGIN_PORT="${ORIGIN_PORT:-443}"
RELAY_PORT="${RELAY_PORT:-443}"
RELAY_LISTEN_ADDRESS="${RELAY_LISTEN_ADDRESS:-0.0.0.0}"
RELAY_NAME="${RELAY_NAME:-vless-tcp-relay}"
PUBLIC_ADDRESS="${PUBLIC_ADDRESS:-}"

XRAY_BINARY="/usr/local/bin/xray"
CONFIG_DIRECTORY="/usr/local/etc/${RELAY_NAME}"
CONFIG_FILE="${CONFIG_DIRECTORY}/relay.json"
LOG_DIRECTORY="/var/log/${RELAY_NAME}"
SERVICE_NAME="${RELAY_NAME}.service"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}"
LOGROTATE_FILE="/etc/logrotate.d/${RELAY_NAME}"

temporary_directory=""
new_installation=false
installation_files_written=false
created_log_directory=false

cleanup() {
	if [[ -n "${temporary_directory}" && -d "${temporary_directory}" ]]; then
		find "${temporary_directory}" -depth -delete
	fi
	if [[ "${created_log_directory}" == true ]]; then
		find "${LOG_DIRECTORY}/access.log" "${LOG_DIRECTORY}/error.log" -maxdepth 0 -type f -delete 2>/dev/null || true
		find "${LOG_DIRECTORY}" -maxdepth 0 -type d -empty -delete 2>/dev/null || true
	fi
}

rollback_new_installation() {
	if [[ "${installation_files_written}" != true ]]; then
		return
	fi

	systemctl disable --now "${SERVICE_NAME}" >/dev/null 2>&1 || true
	find "${SERVICE_FILE}" "${CONFIG_FILE}" "${LOGROTATE_FILE}" -maxdepth 0 -type f -delete 2>/dev/null || true
	find "${CONFIG_DIRECTORY}" "${LOG_DIRECTORY}" -maxdepth 0 -type d -empty -delete 2>/dev/null || true
	systemctl daemon-reload >/dev/null 2>&1 || true
}

on_error() {
	local exit_code=$?
	rollback_new_installation
	echo "VLESS TCP relay installation failed on line ${BASH_LINENO[0]} (exit ${exit_code})." >&2
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

validate_origin_host() {
	local value=$1
	if [[ "${value}" == *:* ]]; then
		if [[ ! "${value}" =~ ^[0-9A-Fa-f:]+$ ]]; then
			echo "ORIGIN_HOST must be a hostname or IP address without brackets." >&2
			exit 1
		fi
	elif [[ ! "${value}" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$ ]] || [[ "${value}" == *..* ]]; then
		echo "ORIGIN_HOST must be a hostname or IP address without a port." >&2
		exit 1
	fi
}

validate_listen_address() {
	if [[ ! "${RELAY_LISTEN_ADDRESS}" =~ ^[0-9A-Fa-f:.]+$ ]]; then
		echo "RELAY_LISTEN_ADDRESS must be an IPv4 or IPv6 address." >&2
		exit 1
	fi
}

install_dependencies() {
	if command -v apt-get >/dev/null 2>&1; then
		apt-get update
		DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl iproute2 logrotate unzip
	elif command -v dnf >/dev/null 2>&1; then
		dnf install -y ca-certificates curl iproute logrotate unzip
	elif command -v yum >/dev/null 2>&1; then
		yum install -y ca-certificates curl iproute logrotate unzip
	else
		echo "Supported package managers: apt-get, dnf, or yum." >&2
		exit 1
	fi
}

check_tcp_connection() {
	local host=$1
	local port=$2
	# The parameters expand inside the child Bash, where $1 and $2 are the
	# separately passed host and port rather than interpolated shell source.
	# shellcheck disable=SC2016
	timeout 5 bash -c 'exec 3<>"/dev/tcp/${1}/${2}"' relay-check "${host}" "${port}"
}

listener_details() {
	ss -H -ltnp "sport = :${RELAY_PORT}" 2>/dev/null || true
}

verify_selected_port() {
	local listeners
	listeners="$(listener_details)"
	if [[ -z "${listeners}" ]]; then
		return
	fi

	local service_pid
	service_pid="$(systemctl show "${SERVICE_NAME}" --property MainPID --value 2>/dev/null || true)"
	if [[ "${service_pid}" =~ ^[1-9][0-9]*$ ]] && grep -Fq "pid=${service_pid}," <<<"${listeners}"; then
		return
	fi

	echo "TCP ${RELAY_PORT} is already in use:" >&2
	echo "${listeners}" >&2
	echo "Choose another RELAY_PORT or stop the conflicting service." >&2
	exit 1
}

install_xray_if_needed() {
	if [[ -x "${XRAY_BINARY}" ]]; then
		return
	fi

	if ! id xray >/dev/null 2>&1; then
		useradd --system --no-create-home --shell /usr/sbin/nologin xray
	fi

	curl -fsSL --retry 3 -o "${temporary_directory}/xray-install.sh" "${XRAY_INSTALL_URL}"
	bash "${temporary_directory}/xray-install.sh" install --install-user xray --version "${XRAY_VERSION}"

	# The official installer creates a generic xray.service. This relay uses a
	# dedicated config and unit, so stop only the newly installed generic unit.
	systemctl disable --now xray.service >/dev/null 2>&1 || true
}

ensure_xray_user() {
	if ! id xray >/dev/null 2>&1; then
		useradd --system --no-create-home --shell /usr/sbin/nologin xray
	fi
}

prepare_log_directory() {
	if [[ -e "${LOG_DIRECTORY}" && ! -d "${LOG_DIRECTORY}" ]]; then
		echo "Log path exists but is not a directory: ${LOG_DIRECTORY}" >&2
		exit 1
	fi
	if [[ ! -d "${LOG_DIRECTORY}" ]]; then
		install -d -o xray -g xray -m 0750 "${LOG_DIRECTORY}"
		install -o xray -g xray -m 0640 /dev/null "${LOG_DIRECTORY}/access.log"
		install -o xray -g xray -m 0640 /dev/null "${LOG_DIRECTORY}/error.log"
		created_log_directory=true
	fi
}

write_candidate_files() {
	local config_candidate="${temporary_directory}/relay.json"
	local service_candidate="${temporary_directory}/${SERVICE_NAME}"
	local logrotate_candidate="${temporary_directory}/${RELAY_NAME}.logrotate"
	local inbound_tag="${RELAY_NAME}-inbound"
	local outbound_tag="${RELAY_NAME}-origin"

	cat >"${config_candidate}" <<EOF
{
  "log": {
    "loglevel": "warning",
    "access": "${LOG_DIRECTORY}/access.log",
    "error": "${LOG_DIRECTORY}/error.log"
  },
  "inbounds": [
    {
      "tag": "${inbound_tag}",
      "listen": "${RELAY_LISTEN_ADDRESS}",
      "port": ${RELAY_PORT},
      "protocol": "dokodemo-door",
      "settings": {
        "address": "${ORIGIN_HOST}",
        "port": ${ORIGIN_PORT},
        "network": "tcp"
      }
    }
  ],
  "outbounds": [
    {
      "tag": "${outbound_tag}",
      "protocol": "freedom"
    }
  ],
  "routing": {
    "domainStrategy": "AsIs",
    "rules": [
      {
        "type": "field",
        "inboundTag": ["${inbound_tag}"],
        "outboundTag": "${outbound_tag}"
      }
    ]
  }
}
EOF

	cat >"${service_candidate}" <<EOF
[Unit]
Description=Transparent TCP relay to VLESS origin ${ORIGIN_HOST}:${ORIGIN_PORT}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=xray
Group=xray
ExecStart=${XRAY_BINARY} run -config ${CONFIG_FILE}
Restart=on-failure
RestartSec=2s
LimitNOFILE=1048576
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
ReadWritePaths=${LOG_DIRECTORY}

[Install]
WantedBy=multi-user.target
EOF

	cat >"${logrotate_candidate}" <<EOF
${LOG_DIRECTORY}/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
    su xray xray
}
EOF
}

detect_existing_installation() {
	local config_candidate="${temporary_directory}/relay.json"
	local service_candidate="${temporary_directory}/${SERVICE_NAME}"
	local logrotate_candidate="${temporary_directory}/${RELAY_NAME}.logrotate"
	local config_exists=false
	local existing_file_count=0

	if [[ -e "${CONFIG_FILE}" ]]; then
		config_exists=true
		existing_file_count=$((existing_file_count + 1))
	fi
	if [[ -e "${SERVICE_FILE}" ]]; then
		existing_file_count=$((existing_file_count + 1))
	fi
	if [[ -e "${LOGROTATE_FILE}" ]]; then
		existing_file_count=$((existing_file_count + 1))
	fi

	if ((existing_file_count != 0 && existing_file_count != 3)); then
		echo "Found an incomplete existing ${RELAY_NAME} installation; refusing to overwrite it." >&2
		exit 1
	fi
	if [[ "${config_exists}" == false ]]; then
		new_installation=true
		return
	fi
	if ! cmp -s "${config_candidate}" "${CONFIG_FILE}" \
		|| ! cmp -s "${service_candidate}" "${SERVICE_FILE}" \
		|| ! cmp -s "${logrotate_candidate}" "${LOGROTATE_FILE}"; then
		echo "${RELAY_NAME} already exists with different settings; refusing to overwrite it." >&2
		echo "Use a different RELAY_NAME for another relay instance." >&2
		exit 1
	fi
}

install_candidate_files() {
	if [[ "${new_installation}" != true ]]; then
		return
	fi

	installation_files_written=true
	install -d -o root -g xray -m 0750 "${CONFIG_DIRECTORY}"
	install -d -o xray -g xray -m 0750 "${LOG_DIRECTORY}"
	install -o root -g xray -m 0640 "${temporary_directory}/relay.json" "${CONFIG_FILE}"
	install -o root -g root -m 0644 "${temporary_directory}/${SERVICE_NAME}" "${SERVICE_FILE}"
	install -o root -g root -m 0644 "${temporary_directory}/${RELAY_NAME}.logrotate" "${LOGROTATE_FILE}"
}

verify_relay() {
	local health_host="${RELAY_LISTEN_ADDRESS}"
	if [[ "${health_host}" == "0.0.0.0" ]]; then
		health_host="127.0.0.1"
	elif [[ "${health_host}" == "::" ]]; then
		health_host="::1"
	fi

	local listeners=""
	for _ in {1..20}; do
		if systemctl is-active --quiet "${SERVICE_NAME}"; then
			listeners="$(listener_details)"
			if [[ -n "${listeners}" ]] && check_tcp_connection "${health_host}" "${RELAY_PORT}"; then
				return
			fi
		fi
		sleep 1
	done

	echo "Relay service failed verification." >&2
	systemctl status "${SERVICE_NAME}" --no-pager >&2 || true
	return 1
}

main() {
	require_root

	if [[ "$(uname -s)" != "Linux" ]]; then
		echo "This installer supports Linux relay nodes only." >&2
		exit 1
	fi
	if ! command -v systemctl >/dev/null 2>&1; then
		echo "systemd is required." >&2
		exit 1
	fi
	if [[ -z "${ORIGIN_HOST}" && -t 0 ]]; then
		read -r -p "Real VLESS node IP or hostname: " ORIGIN_HOST || true
	fi
	if [[ -z "${ORIGIN_HOST}" ]]; then
		echo "ORIGIN_HOST is required, for example ORIGIN_HOST=203.0.113.10." >&2
		exit 1
	fi
	if [[ ! "${RELAY_NAME}" =~ ^[a-z0-9][a-z0-9-]{0,47}$ ]]; then
		echo "RELAY_NAME must contain 1-48 lowercase letters, digits, or hyphens." >&2
		exit 1
	fi
	if [[ ! "${XRAY_VERSION}" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
		echo "XRAY_VERSION must be a release such as v26.3.27." >&2
		exit 1
	fi

	validate_origin_host "${ORIGIN_HOST}"
	validate_listen_address
	validate_port "ORIGIN_PORT" "${ORIGIN_PORT}"
	validate_port "RELAY_PORT" "${RELAY_PORT}"
	if [[ "${ORIGIN_HOST}" == "0.0.0.0" || "${ORIGIN_HOST}" == "::" ]]; then
		echo "ORIGIN_HOST cannot be a wildcard address." >&2
		exit 1
	fi

	install_dependencies
	if ! check_tcp_connection "${ORIGIN_HOST}" "${ORIGIN_PORT}"; then
		echo "Cannot reach the real VLESS node at ${ORIGIN_HOST}:${ORIGIN_PORT}." >&2
		echo "Allow TCP ${ORIGIN_PORT} from this relay before retrying." >&2
		exit 1
	fi

	if [[ -z "${PUBLIC_ADDRESS}" ]]; then
		PUBLIC_ADDRESS="$(curl -4 -fsS --max-time 10 https://api.ipify.org || true)"
	fi
	if [[ -z "${PUBLIC_ADDRESS}" || ! "${PUBLIC_ADDRESS}" =~ ^[A-Za-z0-9:._-]+$ ]]; then
		echo "PUBLIC_ADDRESS could not be detected; provide it explicitly." >&2
		exit 1
	fi
	if [[ "${PUBLIC_ADDRESS}" == "${ORIGIN_HOST}" && "${RELAY_PORT}" == "${ORIGIN_PORT}" ]]; then
		echo "Relay and origin resolve to the same endpoint; refusing to create a forwarding loop." >&2
		exit 1
	fi

	temporary_directory="$(mktemp -d)"
	install_xray_if_needed
	ensure_xray_user
	prepare_log_directory
	write_candidate_files
	"${XRAY_BINARY}" run -test -config "${temporary_directory}/relay.json"
	detect_existing_installation
	verify_selected_port
	install_candidate_files

	systemctl daemon-reload
	systemctl enable "${SERVICE_NAME}" >/dev/null
	systemctl restart "${SERVICE_NAME}"
	verify_relay
	new_installation=false
	installation_files_written=false
	created_log_directory=false

	echo
	echo "VLESS TCP relay installed successfully (installer ${SCRIPT_VERSION})."
	echo "Relay endpoint: ${PUBLIC_ADDRESS}:${RELAY_PORT}/tcp"
	echo "Real VLESS origin: ${ORIGIN_HOST}:${ORIGIN_PORT}/tcp"
	echo "Service: ${SERVICE_NAME}"
	echo
	echo "CHOP admin settings for the real VLESS server record:"
	echo "  relay_host = ${PUBLIC_ADDRESS}"
	echo "  relay_port = ${RELAY_PORT}"
	echo "Keep the server IP and API port pointed at the real VLESS node."
	echo "Open relay TCP ${RELAY_PORT} to clients; do not expose an API port on this relay."
}

main "$@"
