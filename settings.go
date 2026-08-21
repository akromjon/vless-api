package main

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	defaultAPIAddress = "0.0.0.0"
	defaultAPIPort    = 8080
	defaultVLESSPort  = 443
	defaultFlow       = "xtls-rprx-vision"
	defaultInboundTag = "vless-reality"
	defaultAPIServer  = "127.0.0.1:10085"

	// Kept at "chrome" so an existing node's generated URIs do not change
	// shape on upgrade. Chrome's uTLS profile carries a post-quantum key
	// share, which pushes the REALITY ClientHello past one MSS and splits it
	// across two TCP segments; on paths where a middlebox drops the second
	// segment the handshake never completes. Set VLESS_FINGERPRINT=ios (or
	// safari) on nodes serving such paths to keep the ClientHello in a single
	// segment.
	defaultFingerprint = "chrome"
)

// uTLS profiles Xray accepts. Anything outside this set is silently ignored by
// the client, which would leave the fingerprint at Xray's own default without
// any signal that the setting did nothing.
var supportedFingerprints = map[string]bool{
	"chrome": true, "firefox": true, "safari": true, "ios": true,
	"android": true, "edge": true, "360": true, "qq": true,
	"random": true, "randomized": true,
}

var shortIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{2,16}$`)

type AppSettings struct {
	APIAddress       string
	APIPort          int
	APIToken         string
	XrayConfigFile   string
	XrayBinary       string
	XrayService      string
	SystemctlBinary  string
	InboundTag       string
	PublicAddress    string
	VLESSPort        int
	ServerName       string
	RealityPublicKey string
	ShortID          string
	Flow             string
	Fingerprint      string
	XrayAPIAddress   string
}

func loadSettings() (AppSettings, error) {
	settings := AppSettings{
		APIAddress:       envOr("API_ADDRESS", defaultAPIAddress),
		APIPort:          intEnvOr("API_PORT", defaultAPIPort),
		APIToken:         strings.TrimSpace(os.Getenv("API_TOKEN")),
		XrayConfigFile:   envOr("XRAY_CONFIG_FILE", "/usr/local/etc/xray/config.json"),
		XrayBinary:       envOr("XRAY_BINARY", "/usr/local/bin/xray"),
		XrayService:      envOr("XRAY_SERVICE", "xray"),
		SystemctlBinary:  envOr("SYSTEMCTL_BINARY", "systemctl"),
		InboundTag:       envOr("VLESS_INBOUND_TAG", defaultInboundTag),
		PublicAddress:    strings.TrimSpace(os.Getenv("PUBLIC_ADDRESS")),
		VLESSPort:        intEnvOr("VLESS_PORT", defaultVLESSPort),
		ServerName:       strings.TrimSpace(os.Getenv("VLESS_SERVER_NAME")),
		RealityPublicKey: strings.TrimSpace(os.Getenv("VLESS_REALITY_PUBLIC_KEY")),
		ShortID:          strings.ToLower(strings.TrimSpace(os.Getenv("VLESS_SHORT_ID"))),
		Flow:             envOr("VLESS_FLOW", defaultFlow),
		Fingerprint:      strings.ToLower(envOr("VLESS_FINGERPRINT", defaultFingerprint)),
		XrayAPIAddress:   strings.TrimSpace(envOr("XRAY_API_ADDRESS", defaultAPIServer)),
	}

	if err := settings.Validate(); err != nil {
		return AppSettings{}, err
	}
	return settings, nil
}

func (s AppSettings) Validate() error {
	if strings.TrimSpace(s.APIToken) == "" {
		return fmt.Errorf("API_TOKEN is required")
	}
	if s.APIPort < 1 || s.APIPort > 65535 {
		return fmt.Errorf("API_PORT must be between 1 and 65535")
	}
	if s.VLESSPort < 1 || s.VLESSPort > 65535 {
		return fmt.Errorf("VLESS_PORT must be between 1 and 65535")
	}
	if net.ParseIP(s.PublicAddress) == nil {
		if strings.TrimSpace(s.PublicAddress) == "" || strings.ContainsAny(s.PublicAddress, " /:?#@") {
			return fmt.Errorf("PUBLIC_ADDRESS must be an IP address or hostname")
		}
	}
	if strings.TrimSpace(s.ServerName) == "" || strings.ContainsAny(s.ServerName, " /:#?@") {
		return fmt.Errorf("VLESS_SERVER_NAME must be a hostname")
	}
	if strings.TrimSpace(s.RealityPublicKey) == "" {
		return fmt.Errorf("VLESS_REALITY_PUBLIC_KEY is required")
	}
	if !shortIDPattern.MatchString(s.ShortID) || len(s.ShortID)%2 != 0 {
		return fmt.Errorf("VLESS_SHORT_ID must contain 2-16 hexadecimal characters in byte pairs")
	}
	if s.Flow != "" && s.Flow != defaultFlow {
		return fmt.Errorf("VLESS_FLOW must be empty or %q", defaultFlow)
	}
	if !supportedFingerprints[s.Fingerprint] {
		return fmt.Errorf("VLESS_FINGERPRINT %q is not a uTLS profile Xray understands", s.Fingerprint)
	}
	if strings.TrimSpace(s.XrayConfigFile) == "" || strings.TrimSpace(s.InboundTag) == "" {
		return fmt.Errorf("XRAY_CONFIG_FILE and VLESS_INBOUND_TAG are required")
	}
	return nil
}

func (s AppSettings) APIListenAddress() string {
	return net.JoinHostPort(s.APIAddress, strconv.Itoa(s.APIPort))
}

func (s AppSettings) VLESSListenAddress() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(s.VLESSPort))
}

func (s AppSettings) BuildShareURI(record UserRecord) string {
	query := url.Values{}
	query.Set("encryption", "none")
	if s.Flow != "" {
		query.Set("flow", s.Flow)
	}
	query.Set("security", "reality")
	query.Set("sni", s.ServerName)
	query.Set("fp", s.Fingerprint)
	query.Set("pbk", s.RealityPublicKey)
	query.Set("sid", s.ShortID)
	query.Set("type", "tcp")
	query.Set("headerType", "none")

	shareURL := url.URL{
		Scheme:   "vless",
		User:     url.User(record.UUID),
		Host:     net.JoinHostPort(s.PublicAddress, strconv.Itoa(s.VLESSPort)),
		RawQuery: query.Encode(),
		Fragment: record.Name,
	}
	return shareURL.String()
}

func (s AppSettings) TokenMatches(candidate string) bool {
	if len(candidate) != len(s.APIToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.APIToken)) == 1
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func intEnvOr(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return parsed
}
