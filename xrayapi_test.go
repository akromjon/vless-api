package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// fakeLiveAPI records what the store asked the running Xray to do, so a test
// can assert that a mutation went through HandlerService instead of a restart.
type fakeLiveAPI struct {
	available   bool
	added       [][]UserRecord
	addedTags   []string
	removed     [][]string
	removedTags []string
	addErr      error
	removeErr   error
	traffic     []UserTraffic
	statsErr    error
	probeErr    error
	resetSeen   []bool

	liveUsers        []string
	liveUsersErr     error
	inboundUsersTags []string
}

func (f *fakeLiveAPI) Available() bool { return f.available }

func (f *fakeLiveAPI) Probe(string) error { return f.probeErr }

func (f *fakeLiveAPI) AddUsers(tag string, _ int, _ string, users []UserRecord) error {
	f.added = append(f.added, users)
	f.addedTags = append(f.addedTags, tag)
	return f.addErr
}

func (f *fakeLiveAPI) RemoveUsers(tag string, names []string) error {
	f.removed = append(f.removed, names)
	f.removedTags = append(f.removedTags, tag)
	return f.removeErr
}

func (f *fakeLiveAPI) InboundUsers(tag string) ([]string, error) {
	f.inboundUsersTags = append(f.inboundUsersTags, tag)
	return f.liveUsers, f.liveUsersErr
}

func (f *fakeLiveAPI) UserTraffic(reset bool) ([]UserTraffic, error) {
	f.resetSeen = append(f.resetSeen, reset)
	return f.traffic, f.statsErr
}

func TestParseUserTrafficPairsUplinkAndDownlink(t *testing.T) {
	output := `{
		"stat": [
			{"name": "user>>>config_a>>>traffic>>>uplink", "value": 100},
			{"name": "user>>>config_a>>>traffic>>>downlink", "value": 900},
			{"name": "user>>>config_b>>>traffic>>>uplink", "value": 7},
			{"name": "inbound>>>api>>>traffic>>>downlink", "value": 24}
		]
	}`
	traffic, err := parseUserTraffic(output)
	if err != nil {
		t.Fatalf("parseUserTraffic returned error: %v", err)
	}
	if len(traffic) != 2 {
		t.Fatalf("expected 2 users, got %#v", traffic)
	}
	if traffic[0].Name != "config_a" || traffic[0].Uplink != 100 || traffic[0].Downlink != 900 {
		t.Fatalf("unexpected first entry: %#v", traffic[0])
	}
	// Xray omits a counter that has never moved; the pair must not be dropped
	// just because one half is missing.
	if traffic[1].Name != "config_b" || traffic[1].Uplink != 7 || traffic[1].Downlink != 0 {
		t.Fatalf("unexpected second entry: %#v", traffic[1])
	}
}

func TestParseUserTrafficHandlesEmptyDocument(t *testing.T) {
	// A node with no traffic since the last reset answers with "{}".
	traffic, err := parseUserTraffic("{}")
	if err != nil {
		t.Fatalf("parseUserTraffic returned error: %v", err)
	}
	if len(traffic) != 0 {
		t.Fatalf("expected no entries, got %#v", traffic)
	}
}

// The CLI exits 0 even when the payload was rejected, so the printed count is
// the only way to tell a no-op from a real add.
func TestParseAffectedReadsCount(t *testing.T) {
	if got := parseAffected("Added 3 user(s) in total.\n"); got != 3 {
		t.Fatalf("expected 3, got %d", got)
	}
	if got := parseAffected("Removed 1 user(s) in total.\n"); got != 1 {
		t.Fatalf("expected 1, got %d", got)
	}
	if got := parseAffected("failed to build config: something\nAdded 0 user(s) in total.\n"); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
	if got := parseAffected("unrelated output"); got != -1 {
		t.Fatalf("expected -1 for unparseable output, got %d", got)
	}
}

func TestStatsEndpointReportsPerUserTraffic(t *testing.T) {
	store := NewConfigStore(writeTestConfig(t), defaultInboundTag, defaultFlow, &fakeRuntime{active: true})
	live := &fakeLiveAPI{available: true, traffic: []UserTraffic{
		{Name: "config_a", Uplink: 10, Downlink: 20},
		{Name: "config_b", Uplink: 1, Downlink: 2},
	}}
	server := NewAPIServer(testSettings(), store, &fakeRuntime{active: true}).WithLiveAPI(live)

	request := httptest.NewRequest(http.MethodGet, "/api/stats?reset=1", nil)
	request.Header.Set("key", "secret-token")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"config_a"`, `"total_uplink":11`, `"total_downlink":22`, `"active_users":2`} {
		if !contains(body, want) {
			t.Fatalf("response missing %s: %s", want, body)
		}
	}
	if len(live.resetSeen) != 1 || !live.resetSeen[0] {
		t.Fatalf("expected reset=true to reach the API, got %#v", live.resetSeen)
	}
}

// Without the Xray API the endpoint must keep its old 501 rather than pretend
// there is no traffic.
func TestStatsEndpointStays501WithoutLiveAPI(t *testing.T) {
	store := NewConfigStore(writeTestConfig(t), defaultInboundTag, defaultFlow, &fakeRuntime{active: true})
	server := NewAPIServer(testSettings(), store, &fakeRuntime{active: true})

	request := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	request.Header.Set("key", "secret-token")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestStatsEndpointSurfacesAPIFailure(t *testing.T) {
	store := NewConfigStore(writeTestConfig(t), defaultInboundTag, defaultFlow, &fakeRuntime{active: true})
	live := &fakeLiveAPI{available: true, statsErr: errors.New("api unreachable")}
	server := NewAPIServer(testSettings(), store, &fakeRuntime{active: true}).WithLiveAPI(live)

	request := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	request.Header.Set("key", "secret-token")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestFingerprintIsConfigurableAndValidated(t *testing.T) {
	settings := testSettings()
	settings.Fingerprint = "ios"
	share := settings.BuildShareURI(UserRecord{Name: "config_1", UUID: "8f25fc6a-7f5a-4dc5-9ed5-c4c4d49adfa5"})
	parsed, err := url.Parse(share)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("fp") != "ios" {
		t.Fatalf("expected fp=ios, got %s", parsed.RawQuery)
	}

	// An unknown profile must be refused at startup: Xray would silently
	// ignore it and fall back to its own default, hiding the misconfiguration.
	settings.Fingerprint = "nonsense"
	if err := settings.Validate(); err == nil {
		t.Fatal("expected validation to reject an unknown fingerprint")
	}
}

// The default must stay chrome so upgrading the binary on an existing node
// does not change the URIs it hands out.
func TestFingerprintDefaultsToChrome(t *testing.T) {
	t.Setenv("API_TOKEN", "token")
	t.Setenv("PUBLIC_ADDRESS", "203.0.113.10")
	t.Setenv("VLESS_SERVER_NAME", "example.com")
	t.Setenv("VLESS_REALITY_PUBLIC_KEY", "public-key")
	t.Setenv("VLESS_SHORT_ID", "0123456789abcdef")

	settings, err := loadSettings()
	if err != nil {
		t.Fatalf("loadSettings returned error: %v", err)
	}
	if settings.Fingerprint != "chrome" {
		t.Fatalf("expected chrome by default, got %q", settings.Fingerprint)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
