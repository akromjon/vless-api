package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIAuthenticationAndAddUser(t *testing.T) {
	settings := testSettings()
	runtime := &fakeRuntime{active: false}
	store := NewConfigStore(writeTestConfig(t), defaultInboundTag, defaultFlow, runtime)
	server := NewAPIServer(settings, store, runtime).Handler()

	unauthorized := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	unauthorizedResponse := httptest.NewRecorder()
	server.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusNotFound {
		t.Fatalf("expected masked 404, got %d", unauthorizedResponse.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/users/add", strings.NewReader(`{"name":"device_1"}`))
	request.Header.Set("key", settings.APIToken)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Success bool           `json:"success"`
		Data    ClientResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Success || body.Data.Name != "device_1" || !strings.HasPrefix(body.Data.Config, "vless://") {
		t.Fatalf("unexpected response: %#v", body)
	}
	if !strings.Contains(body.Data.Config, "security=reality") || !strings.Contains(body.Data.Config, "pbk=public-key") {
		t.Fatalf("share URI is missing Reality parameters: %s", body.Data.Config)
	}
}

func TestAPIBulkRestartsOnce(t *testing.T) {
	settings := testSettings()
	runtime := &fakeRuntime{}
	store := NewConfigStore(writeTestConfig(t), defaultInboundTag, defaultFlow, runtime)
	server := NewAPIServer(settings, store, runtime).Handler()

	request := httptest.NewRequest(http.MethodPost, "/api/users/add-bulk", strings.NewReader(`{"names":["a","b","c"]}`))
	request.Header.Set("key", settings.APIToken)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if runtime.restartCalls != 1 {
		t.Fatalf("expected one Xray restart, got %d", runtime.restartCalls)
	}
}

func TestHealthRequiresVLESSListener(t *testing.T) {
	settings := testSettings()
	settings.VLESSPort = 65432
	runtime := &fakeRuntime{active: true}
	store := NewConfigStore(writeTestConfig(t), defaultInboundTag, defaultFlow, runtime)
	server := NewAPIServer(settings, store, runtime).Handler()

	assertHealth := func(expectedHealthy bool) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		request.Header.Set("key", settings.APIToken)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
		}
		var body struct {
			Data struct {
				Healthy   bool `json:"healthy"`
				Listening bool `json:"listening"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Data.Healthy != expectedHealthy || body.Data.Listening {
			t.Fatalf("unexpected health response: %s", response.Body.String())
		}
	}

	assertHealth(false)
	if _, err := store.Add("device_1"); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	assertHealth(false)
}

func testSettings() AppSettings {
	return AppSettings{
		APIAddress:       "127.0.0.1",
		APIPort:          8080,
		APIToken:         "secret-token",
		XrayConfigFile:   "/tmp/config.json",
		XrayBinary:       "xray",
		XrayService:      "xray",
		SystemctlBinary:  "systemctl",
		InboundTag:       defaultInboundTag,
		PublicAddress:    "203.0.113.10",
		VLESSPort:        443,
		ServerName:       "example.com",
		RealityPublicKey: "public-key",
		ShortID:          "0123456789abcdef",
		Flow:             defaultFlow,
		Fingerprint:      defaultFingerprint,
		XrayAPIAddress:   defaultAPIServer,
	}
}
