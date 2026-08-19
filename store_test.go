package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeRuntime struct {
	validateCalls int
	restartCalls  int
	restartErrors []error
	active        bool
}

func (f *fakeRuntime) Validate(string) error {
	f.validateCalls++
	return nil
}

func (f *fakeRuntime) Start() error {
	f.active = true
	return nil
}

func (f *fakeRuntime) Stop() error {
	f.active = false
	return nil
}

func (f *fakeRuntime) Restart() error {
	f.restartCalls++
	if len(f.restartErrors) == 0 {
		return nil
	}
	err := f.restartErrors[0]
	f.restartErrors = f.restartErrors[1:]
	return err
}

func (f *fakeRuntime) IsActive() (bool, error) { return f.active, nil }
func (f *fakeRuntime) Version() string         { return "Xray test" }

func TestConfigStoreAddBulkPersistsAndRestartsOnce(t *testing.T) {
	path := writeTestConfig(t)
	runtime := &fakeRuntime{active: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime)

	results, err := store.AddBulk([]string{"device_1", "device_2"})
	if err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}
	if len(results) != 2 || !results[0].Success || !results[1].Success {
		t.Fatalf("unexpected results: %#v", results)
	}
	if runtime.validateCalls != 1 || runtime.restartCalls != 1 {
		t.Fatalf("expected one validation and restart, got validation=%d restart=%d", runtime.validateCalls, runtime.restartCalls)
	}

	records, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(records) != 2 || records[0].Name != "device_1" || records[1].Name != "device_2" {
		t.Fatalf("unexpected records: %#v", records)
	}
	for _, record := range records {
		if len(record.UUID) != 36 {
			t.Fatalf("expected RFC 4122 UUID, got %q", record.UUID)
		}
	}
}

func TestConfigStoreBulkDuplicateDoesNotRestart(t *testing.T) {
	path := writeTestConfig(t)
	runtime := &fakeRuntime{}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime)
	if _, err := store.Add("device_1"); err != nil {
		t.Fatalf("initial Add returned error: %v", err)
	}
	runtime.validateCalls = 0
	runtime.restartCalls = 0

	results, err := store.AddBulk([]string{"device_1"})
	if err != nil {
		t.Fatalf("duplicate AddBulk returned error: %v", err)
	}
	if len(results) != 1 || results[0].Success || results[0].Message != ErrUserExists.Error() {
		t.Fatalf("unexpected duplicate result: %#v", results)
	}
	if runtime.validateCalls != 0 || runtime.restartCalls != 0 {
		t.Fatalf("duplicate-only request must not touch Xray")
	}
}

func TestConfigStoreRestoresOriginalWhenRestartFails(t *testing.T) {
	path := writeTestConfig(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{restartErrors: []error{errors.New("restart failed"), nil}}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime)

	if _, err := store.Add("device_1"); err == nil {
		t.Fatal("expected restart failure")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("configuration was not restored\noriginal: %s\nafter: %s", original, after)
	}
	if runtime.validateCalls != 2 || runtime.restartCalls != 2 {
		t.Fatalf("expected apply and rollback attempts, got validation=%d restart=%d", runtime.validateCalls, runtime.restartCalls)
	}
}

func TestConfigStoreRejectsDuplicateNamesBeforeMutation(t *testing.T) {
	path := writeTestConfig(t)
	runtime := &fakeRuntime{}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime)

	if _, err := store.AddBulk([]string{"same", "same"}); err == nil {
		t.Fatal("expected duplicate request validation error")
	}
	if runtime.validateCalls != 0 || runtime.restartCalls != 0 {
		t.Fatal("invalid request must not touch Xray")
	}
}

func writeTestConfig(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	document := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{map[string]any{
			"tag":      defaultInboundTag,
			"protocol": "vless",
			"settings": map[string]any{"users": []any{}, "decryption": "none"},
		}},
		"outbounds": []any{map[string]any{"protocol": "freedom"}},
	}
	content, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
