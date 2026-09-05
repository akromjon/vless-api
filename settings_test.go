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

func TestParseExtraInbounds(t *testing.T) {
	got, err := parseExtraInbounds("grpc-in:8443, xhttp-in:28081")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []mirrorInbound{{Tag: "grpc-in", Port: 8443}, {Tag: "xhttp-in", Port: 28081}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %#v want %#v", got, want)
	}
	if _, err := parseExtraInbounds("grpc-in"); err == nil {
		t.Fatal("missing port must error")
	}
	if _, err := parseExtraInbounds("vless-reality:443"); err == nil {
		t.Fatal("primary tag must be rejected")
	}
	if got, _ := parseExtraInbounds(""); len(got) != 0 {
		t.Fatalf("empty must yield no mirrors, got %#v", got)
	}
}
