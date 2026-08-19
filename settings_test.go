package main

import (
	"net/url"
	"testing"
)

func TestBuildShareURIUsesSeparateVLESSUUID(t *testing.T) {
	settings := testSettings()
	share := settings.BuildShareURI(UserRecord{Name: "config_123", UUID: "8f25fc6a-7f5a-4dc5-9ed5-c4c4d49adfa5"})
	parsed, err := url.Parse(share)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.User.Username() != "8f25fc6a-7f5a-4dc5-9ed5-c4c4d49adfa5" {
		t.Fatalf("unexpected VLESS UUID: %s", parsed.User.Username())
	}
	if parsed.Query().Get("sni") != "example.com" || parsed.Query().Get("sid") != "0123456789abcdef" {
		t.Fatalf("unexpected Reality parameters: %s", parsed.RawQuery)
	}
}
