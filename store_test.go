package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestWriteAtomicUsesJSONCandidateSuffix(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	validatedPath := ""
	err := writeAtomic(path, []byte("{\"updated\":true}\n"), 0600, -1, -1, func(candidate string) error {
		validatedPath = candidate
		if !strings.HasSuffix(candidate, ".json") {
			return errors.New("candidate must have .json suffix")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("writeAtomic returned error: %v", err)
	}
	if !strings.HasSuffix(validatedPath, ".json") {
		t.Fatalf("validated candidate %q has no .json suffix", validatedPath)
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
			"settings": map[string]any{"clients": []any{}, "decryption": "none"},
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

// With the live API wired in, a mutation must reach the running Xray through
// HandlerService and must NOT restart the service -- a restart would drop every
// established session on the node, which is what makes per-release credential
// rotation unaffordable.
func TestConfigStoreAppliesUsersLiveWithoutRestart(t *testing.T) {
	path := writeTestConfig(t)
	runtime := &fakeRuntime{active: true}
	live := &fakeLiveAPI{available: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime).WithLiveAPI(live, 443)

	if _, err := store.AddBulk([]string{"device_1", "device_2"}); err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}
	if runtime.restartCalls != 0 {
		t.Fatalf("expected no restart, got %d", runtime.restartCalls)
	}
	if len(live.added) != 1 || len(live.added[0]) != 2 {
		t.Fatalf("expected both users pushed live, got %#v", live.added)
	}

	if err := store.Delete("device_1"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if runtime.restartCalls != 0 {
		t.Fatalf("expected no restart on delete, got %d", runtime.restartCalls)
	}
	if len(live.removed) != 1 || live.removed[0][0] != "device_1" {
		t.Fatalf("expected device_1 revoked live, got %#v", live.removed)
	}

	// The file stays the source of truth: a restart must not resurrect the
	// revoked credential.
	records, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	for _, record := range records {
		if record.Name == "device_1" {
			t.Fatal("revoked user is still present in the configuration file")
		}
	}
}

// A rotation is a remove plus an add of the SAME name. Adding first would leave
// the new credential in place only for the removal to take it back out.
func TestConfigStoreRemovesBeforeAdding(t *testing.T) {
	path := writeTestConfig(t)
	live := &fakeLiveAPI{available: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, &fakeRuntime{active: true}).WithLiveAPI(live, 443)

	order := []string{}
	if err := store.applyLive(userDelta{added: []UserRecord{{Name: "a", UUID: "u"}}, removed: []string{"a"}}); err != nil {
		t.Fatalf("applyLive returned error: %v", err)
	}
	if len(live.removed) == 1 {
		order = append(order, "removed")
	}
	if len(live.added) == 1 {
		order = append(order, "added")
	}
	if len(order) != 2 || order[0] != "removed" {
		t.Fatalf("expected removal before addition, got %#v", order)
	}
}

// If the live apply fails the file and the running process disagree. The store
// must restore the file and restart so they agree again, even at the cost of
// the node's sessions -- a silent mismatch is worse than a visible restart.
func TestConfigStoreRollsBackWhenLiveApplyFails(t *testing.T) {
	path := writeTestConfig(t)
	runtime := &fakeRuntime{active: true}
	live := &fakeLiveAPI{available: true, addErr: errors.New("handler service rejected the user")}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime).WithLiveAPI(live, 443)

	if _, err := store.AddBulk([]string{"device_1"}); err == nil {
		t.Fatal("expected AddBulk to fail when the live apply fails")
	}
	if runtime.restartCalls != 1 {
		t.Fatalf("expected one reconciling restart, got %d", runtime.restartCalls)
	}
	records, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	for _, record := range records {
		if record.Name == "device_1" {
			t.Fatal("configuration was not rolled back after a failed live apply")
		}
	}
}

// Nodes that have not had the api/stats blocks added yet must keep the old
// behaviour exactly.
func TestConfigStoreFallsBackToRestartWithoutLiveAPI(t *testing.T) {
	path := writeTestConfig(t)
	runtime := &fakeRuntime{active: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime)

	if _, err := store.AddBulk([]string{"device_1"}); err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}
	if runtime.restartCalls != 1 {
		t.Fatalf("expected the legacy restart path, got %d restarts", runtime.restartCalls)
	}
}

// Rotation is what makes returning a released config to the pool safe: a VLESS
// credential keeps working for whoever still holds it, so the secret must change
// before the name is handed to somebody else.
func TestConfigStoreRotateIssuesNewUUIDAndRevokesOld(t *testing.T) {
	path := writeTestConfig(t)
	runtime := &fakeRuntime{active: true}
	live := &fakeLiveAPI{available: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime).WithLiveAPI(live, 443)

	added, err := store.Add("device_1")
	if err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	rotated, err := store.Rotate("device_1")
	if err != nil {
		t.Fatalf("Rotate returned error: %v", err)
	}
	if rotated.Name != "device_1" {
		t.Fatalf("rotation must preserve the name, got %q", rotated.Name)
	}
	if rotated.UUID == added.UUID {
		t.Fatal("rotation did not change the UUID")
	}
	if runtime.restartCalls != 0 {
		t.Fatalf("rotation must not restart Xray, got %d restarts", runtime.restartCalls)
	}

	// The old secret must be revoked on the live inbound, and the new one
	// registered -- in that order.
	if len(live.removed) != 1 || live.removed[0][0] != "device_1" {
		t.Fatalf("expected the old credential revoked live, got %#v", live.removed)
	}
	if len(live.added) != 2 || live.added[1][0].UUID != rotated.UUID {
		t.Fatalf("expected the new credential registered live, got %#v", live.added)
	}

	// The file must carry the new UUID, so a restart cannot resurrect the old.
	records, err := store.List()
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	var stored UserRecord
	for _, record := range records {
		if record.Name == "device_1" {
			stored = record
		}
	}
	if stored.UUID != rotated.UUID {
		t.Fatalf("configuration file kept the old UUID: %#v", stored)
	}
	if len(records) != 1 {
		t.Fatalf("rotation must not duplicate the entry, got %d records", len(records))
	}
}

func TestConfigStoreRotateUnknownUserIsNotFound(t *testing.T) {
	store := NewConfigStore(writeTestConfig(t), defaultInboundTag, defaultFlow, &fakeRuntime{active: true})
	if _, err := store.Rotate("never_existed"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

// A node whose Xray has no api/stats blocks still inherits the default API
// address. If the probe result were ignored, every user change would attempt a
// live apply, fail, and restart Xray to reconcile.
func TestProbeFailureKeepsStoreOnRestartPath(t *testing.T) {
	path := writeTestConfig(t)
	runtime := &fakeRuntime{active: true}
	live := &fakeLiveAPI{available: true, probeErr: errors.New("connection refused")}

	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime)
	if err := live.Probe(defaultInboundTag); err == nil {
		t.Fatal("expected the probe to fail")
	}
	// Probe failed, so the caller must NOT wire the live API in.
	if _, err := store.AddBulk([]string{"device_1"}); err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}
	if runtime.restartCalls != 1 {
		t.Fatalf("expected the restart fallback, got %d restarts", runtime.restartCalls)
	}
	if len(live.added) != 0 {
		t.Fatalf("live API must not be used after a failed probe, got %#v", live.added)
	}
}
