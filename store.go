package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const maxBulkUsers = 500

var userNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var (
	ErrUserExists      = errors.New("user already exists")
	ErrUserNotFound    = errors.New("user not found")
	errInboundNotFound = errors.New("inbound not found")
)

type UserRecord struct {
	Name string `json:"name"`
	UUID string `json:"uuid"`
}

type BulkUserResult struct {
	Name    string     `json:"name"`
	Success bool       `json:"success"`
	Message string     `json:"message"`
	User    UserRecord `json:"user,omitempty"`
}

type ConfigStore struct {
	path       string
	inboundTag string
	flow       string
	runtime    XrayRuntime
	liveAPI    XrayLiveAPI
	vlessPort  int
	mirrors    []mirrorInbound
	mu         sync.Mutex
}

// userDelta is the exact set of users a mutation added or removed. The live
// apply path needs it because Xray's HandlerService works per user; it cannot
// diff a whole config document the way a restart implicitly does.
type userDelta struct {
	added   []UserRecord
	removed []string
	// mirrors is the set of mirror inbounds actually present in config.json at
	// the time of this mutation (a subset of ConfigStore.mirrors). applyLive
	// must touch only these: calling AddUsers/RemoveUsers on a mirror tag the
	// node doesn't have turns "0 users changed" into an error over the live
	// API, which then rolls the whole file back and restarts Xray.
	mirrors []mirrorInbound
}

func (d userDelta) empty() bool {
	return len(d.added) == 0 && len(d.removed) == 0
}

func NewConfigStore(path, inboundTag, flow string, runtime XrayRuntime) *ConfigStore {
	return &ConfigStore{path: path, inboundTag: inboundTag, flow: flow, runtime: runtime}
}

// WithLiveAPI opts the store into restart-free mutations. Without it every
// change falls back to a full Xray restart, which drops every established
// session on the node.
func (s *ConfigStore) WithLiveAPI(api XrayLiveAPI, vlessPort int) *ConfigStore {
	s.liveAPI = api
	s.vlessPort = vlessPort
	return s
}

// WithWsInbound mirrors every user mutation into a second VLESS inbound —
// the ws+Cloudflare-Tunnel transport running alongside REALITY on nodes that
// have it. Without this, a node's ws-in inbound never receives newly added,
// rotated, or removed users: it silently drifts from the REALITY inbound it
// is supposed to carry the same identities as.
func (s *ConfigStore) WithWsInbound(tag string, port int) *ConfigStore {
	s.mirrors = append(s.mirrors, mirrorInbound{Tag: tag, Port: port})
	return s
}

// WithMirrorInbounds mirrors every user mutation into additional VLESS
// inbounds beside whatever WithWsInbound already registered — e.g. grpc-in
// and xhttp-in carrying the same client list as ws-in and REALITY, flow
// stripped.
func (s *ConfigStore) WithMirrorInbounds(mirrors []mirrorInbound) *ConfigStore {
	s.mirrors = append(s.mirrors, mirrors...)
	return s
}

func (s *ConfigStore) Path() string {
	return s.path
}

// ReconcileMirrorInbounds brings every configured mirror inbound (ws-in,
// grpc-in, xhttp-in, ...) to parity with the primary one, on disk and in the
// running process, one mirror at a time.
//
// WithWsInbound / WithMirrorInbounds only mirror mutations as they happen,
// which leaves a node wrong the moment a mirror inbound is introduced: it is
// added empty beside a REALITY inbound that already serves hundreds of
// users, and none of them are ever registered on it. Every one of those
// users is then rejected over that transport with "invalid request user id".
//
// Drift the other way is just as real. A live apply is not persisted by Xray,
// so an operator's out-of-band `xray api adu` disappears at the next restart,
// when Xray reloads a file that never learned about it.
//
// Running this at startup makes both self-healing: adding a mirror inbound
// becomes "add the inbound, restart vless-api", and any drift is corrected on
// the next boot. It is idempotent -- at parity it writes and sends nothing.
func (s *ConfigStore) ReconcileMirrorInbounds() ([]reconcileResult, error) {
	if len(s.mirrors) == 0 {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	results := make([]reconcileResult, 0, len(s.mirrors))
	for _, mirror := range s.mirrors {
		result, err := s.reconcileMirror(mirror)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

// ReconcileWsInbound is kept for callers that predate multi-mirror support.
func (s *ConfigStore) ReconcileWsInbound() (reconcileResult, error) {
	results, err := s.ReconcileMirrorInbounds()
	if err != nil || len(results) == 0 {
		return reconcileResult{}, err
	}
	return results[0], nil
}

// reconcileMirror runs the ReconcileMirrorInbounds body for a single mirror.
// The caller already holds s.mu.
func (s *ConfigStore) reconcileMirror(mirror mirrorInbound) (reconcileResult, error) {
	document, original, err := s.load()
	if err != nil {
		return reconcileResult{}, err
	}
	primary, err := findUserList(document, s.inboundTag)
	if err != nil {
		return reconcileResult{}, err
	}
	mirrorList, err := findUserList(document, mirror.Tag)
	if err != nil {
		// Not every node has every mirror inbound. An absent inbound is the
		// expected state there, not a misconfiguration.
		if errors.Is(err, errInboundNotFound) {
			return reconcileResult{Tag: mirror.Tag}, nil
		}
		return reconcileResult{}, err
	}

	target, err := decodeUserRecords(primary.values)
	if err != nil {
		return reconcileResult{}, err
	}
	result := reconcileResult{Tag: mirror.Tag, Primary: len(target), WsBefore: len(mirrorList.values)}

	// The file is rebuilt from the primary inbound rather than diffed into,
	// so a mirror holding entries the primary no longer has loses them too.
	fileChanged := !sameUserSet(mirrorList.values, target)
	if fileChanged {
		mirrorList.values = mirrorList.values[:0]
		addUsersToList(mirrorList, target, "")
		mirrorList.commit()
		if err := s.persist(document, original); err != nil {
			return result, err
		}
		result.FileUpdated = true
	}

	if s.liveAPI == nil || !s.liveAPI.Available() {
		return result, nil
	}
	liveNames, err := s.liveAPI.InboundUsers(mirror.Tag)
	if err != nil {
		return result, fmt.Errorf("read live %s users: %w", mirror.Tag, err)
	}
	live := make(map[string]struct{}, len(liveNames))
	for _, name := range liveNames {
		live[name] = struct{}{}
	}
	missing := make([]UserRecord, 0)
	for _, record := range target {
		if _, found := live[record.Name]; !found {
			missing = append(missing, record)
		}
	}
	result.LiveBefore = len(liveNames)
	result.LiveAdded = len(missing)
	if len(missing) == 0 {
		return result, nil
	}
	// Only additions: a live user the primary inbound has dropped is already
	// revoked on the transport that matters, and removing it here would risk
	// cutting an established session on a name the file disagrees about.
	if err := s.liveAPI.AddUsers(mirror.Tag, mirror.Port, "", missing); err != nil {
		return result, fmt.Errorf("register missing %s users: %w", mirror.Tag, err)
	}
	return result, nil
}

type reconcileResult struct {
	Tag         string
	Primary     int
	WsBefore    int
	FileUpdated bool
	LiveBefore  int
	LiveAdded   int
}

func (r reconcileResult) changed() bool {
	return r.FileUpdated || r.LiveAdded > 0
}

func sameUserSet(values []any, target []UserRecord) bool {
	if len(values) != len(target) {
		return false
	}
	have := make(map[string]string, len(values))
	for _, raw := range values {
		user, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		have[stringValue(user["email"])] = stringValue(user["id"])
	}
	for _, record := range target {
		if have[record.Name] != record.UUID {
			return false
		}
	}
	return true
}

func (s *ConfigStore) List() ([]UserRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	document, _, err := s.load()
	if err != nil {
		return nil, err
	}
	users, err := findUserList(document, s.inboundTag)
	if err != nil {
		return nil, err
	}
	records, err := decodeUserRecords(users.values)
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	return records, nil
}

func (s *ConfigStore) Add(name string) (UserRecord, error) {
	results, err := s.AddBulk([]string{name})
	if err != nil {
		return UserRecord{}, err
	}
	if len(results) != 1 || !results[0].Success {
		if len(results) == 1 && results[0].Message == ErrUserExists.Error() {
			return UserRecord{}, ErrUserExists
		}
		return UserRecord{}, fmt.Errorf("failed to add user")
	}
	return results[0].User, nil
}

func (s *ConfigStore) AddBulk(names []string) ([]BulkUserResult, error) {
	if err := validateBulkNames(names); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	document, original, err := s.load()
	if err != nil {
		return nil, err
	}
	users, err := findUserList(document, s.inboundTag)
	if err != nil {
		return nil, err
	}
	records, err := decodeUserRecords(users.values)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(records))
	for _, record := range records {
		existing[record.Name] = struct{}{}
	}

	results := make([]BulkUserResult, 0, len(names))
	added := make([]UserRecord, 0, len(names))
	changed := false
	for _, name := range names {
		if _, found := existing[name]; found {
			results = append(results, BulkUserResult{Name: name, Message: ErrUserExists.Error()})
			continue
		}
		id, err := randomUUID()
		if err != nil {
			return nil, fmt.Errorf("generate UUID for %s: %w", name, err)
		}
		record := UserRecord{Name: name, UUID: id}
		user := map[string]any{
			"id":    record.UUID,
			"email": record.Name,
		}
		if s.flow != "" {
			user["flow"] = s.flow
		}
		users.values = append(users.values, user)
		existing[name] = struct{}{}
		changed = true
		added = append(added, record)
		results = append(results, BulkUserResult{Name: name, Success: true, Message: "user added", User: record})
	}

	if !changed {
		return results, nil
	}
	users.commit()
	presentMirrors, err := s.mirrorUsers(document, func(ws *userList) {
		addUsersToList(ws, added, "")
	})
	if err != nil {
		return nil, err
	}
	if err := s.persistAndApply(document, original, userDelta{added: added, mirrors: presentMirrors}); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *ConfigStore) Delete(name string) error {
	if err := validateUserName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	document, original, err := s.load()
	if err != nil {
		return err
	}
	users, err := findUserList(document, s.inboundTag)
	if err != nil {
		return err
	}
	filtered := make([]any, 0, len(users.values))
	found := false
	for _, raw := range users.values {
		user, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid user entry in Xray config")
		}
		if stringValue(user["email"]) == name {
			found = true
			continue
		}
		filtered = append(filtered, raw)
	}
	if !found {
		return ErrUserNotFound
	}
	users.values = filtered
	users.commit()
	presentMirrors, err := s.mirrorUsers(document, func(ws *userList) {
		removeUsersFromList(ws, []string{name})
	})
	if err != nil {
		return err
	}
	return s.persistAndApply(document, original, userDelta{removed: []string{name}, mirrors: presentMirrors})
}

// Rotate issues a fresh VLESS UUID for an existing config name, revoking the
// old one. The name is the stable identity the backend keys on; only the secret
// changes, so the caller updates the stored share URI and nothing else.
//
// This is the operation that makes returning a released config to the pool safe.
// Unlike a WireGuard peer, a VLESS credential keeps working for whoever still
// holds it, so handing an un-rotated config to the next user puts two devices on
// one identity.
//
// The user keeps its position in the client list: a rotation is a secret change,
// not a re-registration.
func (s *ConfigStore) Rotate(name string) (UserRecord, error) {
	if err := validateUserName(name); err != nil {
		return UserRecord{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	document, original, err := s.load()
	if err != nil {
		return UserRecord{}, err
	}
	users, err := findUserList(document, s.inboundTag)
	if err != nil {
		return UserRecord{}, err
	}

	id, err := randomUUID()
	if err != nil {
		return UserRecord{}, fmt.Errorf("generate UUID for %s: %w", name, err)
	}

	found := false
	for _, raw := range users.values {
		user, ok := raw.(map[string]any)
		if !ok {
			return UserRecord{}, fmt.Errorf("invalid user entry in Xray config")
		}
		if stringValue(user["email"]) != name {
			continue
		}
		user["id"] = id
		found = true
		break
	}
	if !found {
		return UserRecord{}, ErrUserNotFound
	}

	record := UserRecord{Name: name, UUID: id}
	users.commit()
	presentMirrors, err := s.mirrorUsers(document, func(ws *userList) {
		rotateUserInList(ws, name, id)
	})
	if err != nil {
		return UserRecord{}, err
	}
	// Removal must precede the addition, or the live apply would register the
	// new secret and then immediately revoke it by name. applyLive enforces
	// that order.
	if err := s.persistAndApply(document, original, userDelta{
		removed: []string{name},
		added:   []UserRecord{record},
		mirrors: presentMirrors,
	}); err != nil {
		return UserRecord{}, err
	}
	return record, nil
}

func (s *ConfigStore) DeleteAll() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	document, original, err := s.load()
	if err != nil {
		return 0, err
	}
	users, err := findUserList(document, s.inboundTag)
	if err != nil {
		return 0, err
	}
	deleted := len(users.values)
	if deleted == 0 {
		return 0, nil
	}
	removed := make([]string, 0, deleted)
	for _, raw := range users.values {
		if user, ok := raw.(map[string]any); ok {
			if email := stringValue(user["email"]); email != "" {
				removed = append(removed, email)
			}
		}
	}
	users.values = []any{}
	users.commit()
	presentMirrors, err := s.mirrorUsers(document, func(ws *userList) {
		ws.values = []any{}
	})
	if err != nil {
		return 0, err
	}
	if err := s.persistAndApply(document, original, userDelta{removed: removed, mirrors: presentMirrors}); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (s *ConfigStore) load() (map[string]any, []byte, error) {
	original, err := os.ReadFile(s.path)
	if err != nil {
		return nil, nil, fmt.Errorf("read Xray config: %w", err)
	}
	var document map[string]any
	if err := json.Unmarshal(original, &document); err != nil {
		return nil, nil, fmt.Errorf("parse Xray config: %w", err)
	}
	return document, original, nil
}

// persist writes the configuration and nothing else. Reconciliation uses it
// because it drives the live process itself, from the live process's own user
// list, rather than from a delta the file write could describe.
func (s *ConfigStore) persist(document map[string]any, original []byte) error {
	updated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Xray config: %w", err)
	}
	updated = append(updated, '\n')
	mode, uid, gid := s.fileOwnership()
	if err := writeAtomic(s.path, updated, mode, uid, gid, s.runtime.Validate); err != nil {
		// Leave the file exactly as it was found; a half-written ws-in is
		// worse than a stale one.
		_ = writeAtomic(s.path, original, mode, uid, gid, s.runtime.Validate)
		return err
	}
	return nil
}

// persistAndApply writes the new configuration, then brings the RUNNING Xray
// in step with it. The file is always written first: it is the source of truth
// across restarts, and a live-only change would silently vanish on the next
// reboot.
//
// With the live API available the running instance is updated per user and is
// never restarted, so other users' sessions survive. That is what makes
// per-release credential rotation affordable. Without it we fall back to the
// original restart, which drops every established session on the node.
func (s *ConfigStore) persistAndApply(document map[string]any, original []byte, delta userDelta) error {
	updated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Xray config: %w", err)
	}
	updated = append(updated, '\n')

	mode, uid, gid := s.fileOwnership()
	if err := writeAtomic(s.path, updated, mode, uid, gid, s.runtime.Validate); err != nil {
		return err
	}
	restore := func() error {
		return writeAtomic(s.path, original, mode, uid, gid, s.runtime.Validate)
	}

	if s.liveAPI != nil && s.liveAPI.Available() && !delta.empty() {
		if err := s.applyLive(delta); err != nil {
			// The file no longer matches the running instance. Put the file
			// back and restart so the two agree again, even though that costs
			// the node's sessions -- a silent mismatch is worse.
			rollbackErr := restore()
			if rollbackErr == nil {
				rollbackErr = s.runtime.Restart()
			}
			if rollbackErr != nil {
				return fmt.Errorf("apply Xray users: %v; rollback also failed: %w", err, rollbackErr)
			}
			return fmt.Errorf("apply Xray users: %v; original configuration restored", err)
		}
		return nil
	}

	if err := s.runtime.Restart(); err == nil {
		return nil
	} else {
		restartErr := err
		rollbackErr := restore()
		if rollbackErr == nil {
			rollbackErr = s.runtime.Restart()
		}
		if rollbackErr != nil {
			return fmt.Errorf("restart Xray: %v; rollback also failed: %w", restartErr, rollbackErr)
		}
		return fmt.Errorf("restart Xray: %v; original configuration restored", restartErr)
	}
}

// applyLive removes before it adds. A rotation is a remove plus an add of the
// SAME name; adding first would leave the new credential in place only to have
// the removal take it straight back out.
func (s *ConfigStore) applyLive(delta userDelta) error {
	if len(delta.removed) > 0 {
		if err := s.liveAPI.RemoveUsers(s.inboundTag, delta.removed); err != nil {
			return err
		}
		for _, mirror := range delta.mirrors {
			if err := s.liveAPI.RemoveUsers(mirror.Tag, delta.removed); err != nil {
				return err
			}
		}
	}
	if len(delta.added) > 0 {
		if err := s.liveAPI.AddUsers(s.inboundTag, s.vlessPort, s.flow, delta.added); err != nil {
			return err
		}
		for _, mirror := range delta.mirrors {
			if err := s.liveAPI.AddUsers(mirror.Tag, mirror.Port, "", delta.added); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *ConfigStore) fileOwnership() (os.FileMode, int, int) {
	mode := os.FileMode(0600)
	uid, gid := -1, -1
	if info, err := os.Stat(s.path); err == nil {
		mode = info.Mode().Perm()
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(stat.Uid), int(stat.Gid)
		}
	}
	return mode, uid, gid
}

func writeAtomic(path string, content []byte, mode os.FileMode, uid, gid int, validate func(string) error) error {
	directory := filepath.Dir(path)
	// Xray detects the configuration format from the filename, so the
	// candidate used by `xray run -test` must retain a .json suffix.
	temporary, err := os.CreateTemp(directory, ".vless-api-config-*.json")
	if err != nil {
		return fmt.Errorf("create temporary Xray config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary Xray config permissions: %w", err)
	}
	if uid >= 0 && gid >= 0 {
		if err := temporary.Chown(uid, gid); err != nil {
			temporary.Close()
			return fmt.Errorf("preserve Xray config ownership: %w", err)
		}
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary Xray config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary Xray config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary Xray config: %w", err)
	}
	if err := validate(temporaryPath); err != nil {
		return fmt.Errorf("validate Xray config: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace Xray config: %w", err)
	}
	return nil
}

type userList struct {
	settings map[string]any
	field    string
	values   []any
}

func (u *userList) commit() {
	u.settings[u.field] = u.values
}

func findUserList(document map[string]any, inboundTag string) (*userList, error) {
	inbounds, ok := document["inbounds"].([]any)
	if !ok {
		return nil, fmt.Errorf("Xray config has no inbounds array")
	}
	for _, rawInbound := range inbounds {
		inbound, ok := rawInbound.(map[string]any)
		if !ok || stringValue(inbound["tag"]) != inboundTag {
			continue
		}
		if stringValue(inbound["protocol"]) != "vless" {
			return nil, fmt.Errorf("inbound %q is not a VLESS inbound", inboundTag)
		}
		settings, ok := inbound["settings"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("inbound %q has no settings object", inboundTag)
		}
		field := "users"
		if _, found := settings["clients"]; found {
			field = "clients"
		}
		values, ok := settings[field].([]any)
		if !ok {
			if settings[field] == nil {
				values = []any{}
			} else {
				return nil, fmt.Errorf("inbound %q settings.%s is not an array", inboundTag, field)
			}
		}
		return &userList{settings: settings, field: field, values: values}, nil
	}
	return nil, fmt.Errorf("%w: VLESS inbound %q", errInboundNotFound, inboundTag)
}

// mirrorUsers applies mutate to every configured mirror inbound's user list
// (ws-in, grpc-in, xhttp-in, ...). A missing mirror inbound is expected on a
// node that has not been given it yet — skip rather than fail. Any other
// error (malformed config, wrong protocol) still bubbles up: that is a real
// misconfiguration, not an absence.
//
// It returns the subset of s.mirrors that were actually found in document, so
// the caller can carry that exact set into applyLive: a mirror absent from
// config.json today must stay untouched by the live API too, not just by the
// file write.
func (s *ConfigStore) mirrorUsers(document map[string]any, mutate func(*userList)) ([]mirrorInbound, error) {
	found := make([]mirrorInbound, 0, len(s.mirrors))
	for _, mirror := range s.mirrors {
		list, err := findUserList(document, mirror.Tag)
		if err != nil {
			if errors.Is(err, errInboundNotFound) {
				continue
			}
			return found, err
		}
		mutate(list)
		list.commit()
		found = append(found, mirror)
	}
	return found, nil
}

func addUsersToList(list *userList, added []UserRecord, flow string) {
	existing := make(map[string]struct{}, len(list.values))
	for _, raw := range list.values {
		if user, ok := raw.(map[string]any); ok {
			existing[stringValue(user["email"])] = struct{}{}
		}
	}
	for _, record := range added {
		if _, found := existing[record.Name]; found {
			continue
		}
		user := map[string]any{"id": record.UUID, "email": record.Name}
		if flow != "" {
			user["flow"] = flow
		}
		list.values = append(list.values, user)
		existing[record.Name] = struct{}{}
	}
}

func removeUsersFromList(list *userList, names []string) {
	if len(names) == 0 {
		return
	}
	remove := make(map[string]struct{}, len(names))
	for _, name := range names {
		remove[name] = struct{}{}
	}
	filtered := make([]any, 0, len(list.values))
	for _, raw := range list.values {
		if user, ok := raw.(map[string]any); ok {
			if _, found := remove[stringValue(user["email"])]; found {
				continue
			}
		}
		filtered = append(filtered, raw)
	}
	list.values = filtered
}

func rotateUserInList(list *userList, name, newID string) {
	for _, raw := range list.values {
		if user, ok := raw.(map[string]any); ok && stringValue(user["email"]) == name {
			user["id"] = newID
			return
		}
	}
	// Not present yet -- e.g. ws-in was added to this node after this user
	// was created. Add it fresh rather than silently leaving it missing.
	list.values = append(list.values, map[string]any{"id": newID, "email": name})
}

func decodeUserRecords(values []any) ([]UserRecord, error) {
	records := make([]UserRecord, 0, len(values))
	seenNames := make(map[string]struct{}, len(values))
	seenIDs := make(map[string]struct{}, len(values))
	for _, raw := range values {
		user, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid user entry in Xray config")
		}
		record := UserRecord{Name: stringValue(user["email"]), UUID: stringValue(user["id"])}
		if record.Name == "" || record.UUID == "" {
			return nil, fmt.Errorf("every managed Xray user must have email and id fields")
		}
		if _, found := seenNames[record.Name]; found {
			return nil, fmt.Errorf("duplicate managed Xray user name %q", record.Name)
		}
		if _, found := seenIDs[record.UUID]; found {
			return nil, fmt.Errorf("duplicate managed Xray UUID %q", record.UUID)
		}
		seenNames[record.Name] = struct{}{}
		seenIDs[record.UUID] = struct{}{}
		records = append(records, record)
	}
	return records, nil
}

func validateBulkNames(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("at least one name is required")
	}
	if len(names) > maxBulkUsers {
		return fmt.Errorf("at most %d names may be added at once", maxBulkUsers)
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if err := validateUserName(name); err != nil {
			return err
		}
		if _, found := seen[name]; found {
			return fmt.Errorf("duplicate name %q in request", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validateUserName(name string) error {
	if !userNamePattern.MatchString(name) {
		return fmt.Errorf("name must match %s", userNamePattern.String())
	}
	return nil
}

func randomUUID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16]), nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
