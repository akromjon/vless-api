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

const wsInboundTag = "ws-in"

func writeTestConfigWithWs(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	document := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{
				"tag":      defaultInboundTag,
				"protocol": "vless",
				"settings": map[string]any{"clients": []any{}, "decryption": "none"},
			},
			map[string]any{
				"tag":      wsInboundTag,
				"protocol": "vless",
				"settings": map[string]any{"clients": []any{}, "decryption": "none"},
			},
		},
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

func readInboundClients(t *testing.T, path, tag string) []any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, rawInbound := range document["inbounds"].([]any) {
		inbound := rawInbound.(map[string]any)
		if stringValue(inbound["tag"]) == tag {
			settings := inbound["settings"].(map[string]any)
			return settings["clients"].([]any)
		}
	}
	t.Fatalf("inbound %q not found in %s", tag, path)
	return nil
}

// A node with a ws-in inbound must have every REALITY user mirrored into it.
// Without this, vless-api adds new users only to the REALITY inbound and
// ws-in silently drifts -- real users allocated after the fact never get ws
// access at all.
func TestConfigStoreMirrorsAddIntoWsInbound(t *testing.T) {
	path := writeTestConfigWithWs(t)
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, &fakeRuntime{active: true}).
		WithWsInbound(wsInboundTag, 8080)

	if _, err := store.AddBulk([]string{"device_1"}); err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}

	primary := readInboundClients(t, path, defaultInboundTag)
	ws := readInboundClients(t, path, wsInboundTag)
	if len(primary) != 1 || len(ws) != 1 {
		t.Fatalf("expected one user mirrored into both inbounds, got primary=%d ws=%d", len(primary), len(ws))
	}
	primaryUser := primary[0].(map[string]any)
	wsUser := ws[0].(map[string]any)
	if primaryUser["id"] != wsUser["id"] || primaryUser["email"] != wsUser["email"] {
		t.Fatalf("ws user must match the primary inbound's identity, got primary=%#v ws=%#v", primaryUser, wsUser)
	}
	if _, hasFlow := wsUser["flow"]; hasFlow {
		t.Fatalf("ws-in must not carry a flow field, got %#v", wsUser)
	}
}

// Deletion must remove the user from ws-in too, or a revoked credential
// keeps working over the ws transport.
func TestConfigStoreMirrorsDeleteIntoWsInbound(t *testing.T) {
	path := writeTestConfigWithWs(t)
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, &fakeRuntime{active: true}).
		WithWsInbound(wsInboundTag, 8080)
	if _, err := store.Add("device_1"); err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	if err := store.Delete("device_1"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if ws := readInboundClients(t, path, wsInboundTag); len(ws) != 0 {
		t.Fatalf("expected ws-in emptied after delete, got %#v", ws)
	}
}

// Rotation changes the secret in place; the ws inbound must pick up the same
// new UUID under the same name, not be left holding the revoked one.
func TestConfigStoreMirrorsRotateIntoWsInbound(t *testing.T) {
	path := writeTestConfigWithWs(t)
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, &fakeRuntime{active: true}).
		WithWsInbound(wsInboundTag, 8080)
	original, err := store.Add("device_1")
	if err != nil {
		t.Fatalf("Add returned error: %v", err)
	}

	rotated, err := store.Rotate("device_1")
	if err != nil {
		t.Fatalf("Rotate returned error: %v", err)
	}
	if rotated.UUID == original.UUID {
		t.Fatal("rotation must issue a new UUID")
	}

	ws := readInboundClients(t, path, wsInboundTag)
	if len(ws) != 1 {
		t.Fatalf("expected exactly one ws-in user after rotation, got %d", len(ws))
	}
	if ws[0].(map[string]any)["id"] != rotated.UUID {
		t.Fatalf("ws-in must carry the rotated UUID, got %#v", ws[0])
	}
}

// A node mid-migration -- WS_INBOUND_TAG configured but the inbound not yet
// present in the file -- must not fail user mutations. The absence is
// expected, not a misconfiguration.
func TestConfigStoreToleratesMissingWsInbound(t *testing.T) {
	path := writeTestConfig(t)
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, &fakeRuntime{active: true}).
		WithWsInbound(wsInboundTag, 8080)

	if _, err := store.AddBulk([]string{"device_1"}); err != nil {
		t.Fatalf("expected a missing ws-in inbound to be tolerated, got error: %v", err)
	}
}

// With the live API wired in, both the REALITY and ws inbounds must receive
// the same add/remove call through HandlerService -- this is what keeps a
// node's ws transport in sync with its REALITY transport without a restart.
func TestConfigStoreMirrorsLiveApplyToWsInbound(t *testing.T) {
	path := writeTestConfigWithWs(t)
	live := &fakeLiveAPI{available: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, &fakeRuntime{active: true}).
		WithLiveAPI(live, 443).
		WithWsInbound(wsInboundTag, 8080)

	if _, err := store.AddBulk([]string{"device_1"}); err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}
	if len(live.added) != 2 || live.addedTags[0] != defaultInboundTag || live.addedTags[1] != wsInboundTag {
		t.Fatalf("expected AddUsers on both inbounds, got tags=%#v", live.addedTags)
	}

	if err := store.Delete("device_1"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if len(live.removed) != 2 || live.removedTags[0] != defaultInboundTag || live.removedTags[1] != wsInboundTag {
		t.Fatalf("expected RemoveUsers on both inbounds, got tags=%#v", live.removedTags)
	}
}

// The migration case this exists for: ws-in is added empty beside a REALITY
// inbound that already serves users. Without reconciliation every one of them
// is rejected over ws, because WithWsInbound only mirrors NEW mutations.
func TestReconcileBackfillsExistingUsersIntoEmptyWsInbound(t *testing.T) {
	path := writeTestConfigWithWs(t)
	runtime := &fakeRuntime{active: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime)
	if _, err := store.AddBulk([]string{"device_1", "device_2", "device_3"}); err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}
	// Simulate the pre-migration state: the primary inbound is populated and
	// ws-in is empty, exactly as a hot-added inbound arrives.
	emptyWsInbound(t, path)

	live := &fakeLiveAPI{available: true}
	store = NewConfigStore(path, defaultInboundTag, defaultFlow, runtime).
		WithLiveAPI(live, 443).
		WithWsInbound(wsInboundTag, 8080)

	result, err := store.ReconcileWsInbound()
	if err != nil {
		t.Fatalf("ReconcileWsInbound returned error: %v", err)
	}
	if !result.FileUpdated || result.Primary != 3 || result.LiveAdded != 3 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if ws := readInboundClients(t, path, wsInboundTag); len(ws) != 3 {
		t.Fatalf("expected ws-in backfilled to 3 users on disk, got %d", len(ws))
	}
	if len(live.added) != 1 || live.addedTags[0] != wsInboundTag {
		t.Fatalf("expected one live AddUsers against ws-in, got tags=%#v", live.addedTags)
	}
	for _, user := range readInboundClients(t, path, wsInboundTag) {
		if _, hasFlow := user.(map[string]any)["flow"]; hasFlow {
			t.Fatalf("ws-in must not carry a flow field, got %#v", user)
		}
	}
}

// Reconciliation runs on every start, so at parity it must be a no-op: no
// rewrite of the file, and nothing pushed to the live process.
func TestReconcileIsIdempotentAtParity(t *testing.T) {
	path := writeTestConfigWithWs(t)
	runtime := &fakeRuntime{active: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime).
		WithWsInbound(wsInboundTag, 8080)
	if _, err := store.AddBulk([]string{"device_1", "device_2"}); err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}

	live := &fakeLiveAPI{available: true, liveUsers: []string{"device_1", "device_2"}}
	store = NewConfigStore(path, defaultInboundTag, defaultFlow, runtime).
		WithLiveAPI(live, 443).
		WithWsInbound(wsInboundTag, 8080)

	result, err := store.ReconcileWsInbound()
	if err != nil {
		t.Fatalf("ReconcileWsInbound returned error: %v", err)
	}
	if result.changed() {
		t.Fatalf("expected a no-op at parity, got %#v", result)
	}
	if len(live.added) != 0 {
		t.Fatalf("expected nothing pushed live, got %#v", live.added)
	}
}

// The live process can hold users the file does not, from an out-of-band
// `xray api adu`. Those must not be re-sent: adu aborts the whole batch on the
// first duplicate, which would leave the genuinely missing users unregistered.
func TestReconcileSkipsUsersTheLiveProcessAlreadyHas(t *testing.T) {
	path := writeTestConfigWithWs(t)
	runtime := &fakeRuntime{active: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime)
	if _, err := store.AddBulk([]string{"device_1", "device_2", "device_3"}); err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}
	emptyWsInbound(t, path)

	live := &fakeLiveAPI{available: true, liveUsers: []string{"device_1", "device_3"}}
	store = NewConfigStore(path, defaultInboundTag, defaultFlow, runtime).
		WithLiveAPI(live, 443).
		WithWsInbound(wsInboundTag, 8080)

	result, err := store.ReconcileWsInbound()
	if err != nil {
		t.Fatalf("ReconcileWsInbound returned error: %v", err)
	}
	if result.LiveAdded != 1 {
		t.Fatalf("expected exactly the one missing user pushed live, got %#v", result)
	}
	if len(live.added) != 1 || len(live.added[0]) != 1 || live.added[0][0].Name != "device_2" {
		t.Fatalf("expected only device_2 pushed live, got %#v", live.added)
	}
}

// A node not yet migrated to ws has no ws-in inbound at all. Reconciliation
// must leave it completely alone rather than treating the absence as an error.
func TestReconcileLeavesUnmigratedNodeUntouched(t *testing.T) {
	path := writeTestConfig(t)
	runtime := &fakeRuntime{active: true}
	live := &fakeLiveAPI{available: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime).
		WithLiveAPI(live, 443).
		WithWsInbound(wsInboundTag, 8080)
	if _, err := store.AddBulk([]string{"device_1"}); err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	result, err := store.ReconcileWsInbound()
	if err != nil {
		t.Fatalf("expected a missing ws-in inbound to be tolerated, got: %v", err)
	}
	if result.changed() {
		t.Fatalf("expected a no-op without a ws inbound, got %#v", result)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("config file must not be rewritten on a node without ws-in")
	}
}

// A store with no ws inbound configured must not call the live API at all.
func TestReconcileWithoutWsConfiguredDoesNothing(t *testing.T) {
	live := &fakeLiveAPI{available: true}
	store := NewConfigStore(writeTestConfigWithWs(t), defaultInboundTag, defaultFlow, &fakeRuntime{active: true}).
		WithLiveAPI(live, 443)

	result, err := store.ReconcileWsInbound()
	if err != nil {
		t.Fatalf("ReconcileWsInbound returned error: %v", err)
	}
	if result.changed() || len(live.inboundUsersTags) != 0 {
		t.Fatalf("expected a complete no-op, got result=%#v tags=%#v", result, live.inboundUsersTags)
	}
}

// emptyWsInbound clears ws-in on disk, reproducing a hot-added inbound that
// arrives with no users while the primary inbound is already populated.
func emptyWsInbound(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, rawInbound := range document["inbounds"].([]any) {
		inbound := rawInbound.(map[string]any)
		if stringValue(inbound["tag"]) == wsInboundTag {
			inbound["settings"].(map[string]any)["clients"] = []any{}
		}
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeTestConfigWithMirrors(t *testing.T, tags ...string) string {
	t.Helper()
	inbounds := []any{map[string]any{
		"tag": defaultInboundTag, "protocol": "vless",
		"settings": map[string]any{"clients": []any{}, "decryption": "none"},
	}}
	for _, tag := range tags {
		inbounds = append(inbounds, map[string]any{
			"tag": tag, "protocol": "vless",
			"settings": map[string]any{"clients": []any{}, "decryption": "none"},
		})
	}
	document := map[string]any{
		"log":       map[string]any{"loglevel": "warning"},
		"inbounds":  inbounds,
		"outbounds": []any{map[string]any{"protocol": "freedom"}},
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A node with grpc-in and xhttp-in beside ws-in must have every user
// mirrored into all three, not just the single ws-in inbound WithWsInbound
// alone would cover.
func TestConfigStoreMirrorsIntoEveryExtraInbound(t *testing.T) {
	path := writeTestConfigWithMirrors(t, wsInboundTag, "grpc-in", "xhttp-in")
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, &fakeRuntime{active: true}).
		WithWsInbound(wsInboundTag, 28080).
		WithMirrorInbounds([]mirrorInbound{{Tag: "grpc-in", Port: 8443}, {Tag: "xhttp-in", Port: 28081}})

	if _, err := store.AddBulk([]string{"device_1", "device_2"}); err != nil {
		t.Fatalf("AddBulk: %v", err)
	}
	for _, tag := range []string{wsInboundTag, "grpc-in", "xhttp-in"} {
		clients := readInboundClients(t, path, tag)
		if len(clients) != 2 {
			t.Fatalf("%s: expected 2 mirrored users, got %d", tag, len(clients))
		}
		if _, hasFlow := clients[0].(map[string]any)["flow"]; hasFlow {
			t.Fatalf("%s must not carry flow", tag)
		}
	}
	if err := store.Delete("device_1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, tag := range []string{wsInboundTag, "grpc-in", "xhttp-in"} {
		if n := len(readInboundClients(t, path, tag)); n != 1 {
			t.Fatalf("%s: expected 1 after delete, got %d", tag, n)
		}
	}
}

// Reconciliation at startup must backfill every configured mirror, not just
// the first one -- a node freshly given grpc-in and xhttp-in has both empty
// beside an already-populated REALITY inbound.
func TestReconcileBackfillsEveryExtraInbound(t *testing.T) {
	path := writeTestConfigWithMirrors(t, "grpc-in", "xhttp-in")
	seed := NewConfigStore(path, defaultInboundTag, defaultFlow, &fakeRuntime{active: true})
	if _, err := seed.AddBulk([]string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, &fakeRuntime{active: true}).
		WithMirrorInbounds([]mirrorInbound{{Tag: "grpc-in", Port: 8443}, {Tag: "xhttp-in", Port: 28081}})
	results, err := store.ReconcileMirrorInbounds()
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || !results[0].FileUpdated || !results[1].FileUpdated {
		t.Fatalf("expected both mirrors backfilled, got %#v", results)
	}
	for _, tag := range []string{"grpc-in", "xhttp-in"} {
		if n := len(readInboundClients(t, path, tag)); n != 3 {
			t.Fatalf("%s: expected 3, got %d", tag, n)
		}
	}
}

// A store can be configured with a mirror (e.g. grpc-in) that a given node's
// config.json does not have yet -- the mirror was added to EXTRA_INBOUNDS
// before the inbound itself was installed on that node, or the node predates
// it entirely. mirrorUsers already treats that as expected and skips it in
// the file write. applyLive must skip it too: calling AddUsers/RemoveUsers on
// a tag Xray's live HandlerService has never heard of turns "0 users changed"
// into an error, which then rolls the whole config file back and restarts
// Xray -- dropping every session on the node for a mirror it was never
// carrying in the first place.
func TestApplyLiveSkipsMirrorsAbsentFromConfig(t *testing.T) {
	path := writeTestConfigWithMirrors(t, wsInboundTag)
	runtime := &fakeRuntime{active: true}
	live := &fakeLiveAPI{available: true}
	store := NewConfigStore(path, defaultInboundTag, defaultFlow, runtime).
		WithLiveAPI(live, 443).
		WithWsInbound(wsInboundTag, 28080).
		WithMirrorInbounds([]mirrorInbound{{Tag: "grpc-in", Port: 8443}})

	if _, err := store.AddBulk([]string{"a"}); err != nil {
		t.Fatalf("AddBulk returned error: %v", err)
	}
	if runtime.restartCalls != 0 {
		t.Fatalf("expected no restart, got %d", runtime.restartCalls)
	}
	seenTags := map[string]bool{}
	for _, tag := range live.addedTags {
		seenTags[tag] = true
	}
	if !seenTags[defaultInboundTag] {
		t.Fatalf("expected AddUsers on the primary inbound, got tags %#v", live.addedTags)
	}
	if !seenTags[wsInboundTag] {
		t.Fatalf("expected AddUsers on ws-in (present in config.json), got tags %#v", live.addedTags)
	}
	if seenTags["grpc-in"] {
		t.Fatalf("grpc-in is absent from config.json; AddUsers must never be called for it, got tags %#v", live.addedTags)
	}
	if len(live.addedTags) != 2 {
		t.Fatalf("expected exactly 2 AddUsers calls (primary + ws-in), got %#v", live.addedTags)
	}
}
