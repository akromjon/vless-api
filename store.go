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
	ErrUserExists   = errors.New("user already exists")
	ErrUserNotFound = errors.New("user not found")
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
	mu         sync.Mutex
}

func NewConfigStore(path, inboundTag, flow string, runtime XrayRuntime) *ConfigStore {
	return &ConfigStore{path: path, inboundTag: inboundTag, flow: flow, runtime: runtime}
}

func (s *ConfigStore) Path() string {
	return s.path
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
		results = append(results, BulkUserResult{Name: name, Success: true, Message: "user added", User: record})
	}

	if !changed {
		return results, nil
	}
	users.commit()
	if err := s.persistAndRestart(document, original); err != nil {
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
	return s.persistAndRestart(document, original)
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
	users.values = []any{}
	users.commit()
	if err := s.persistAndRestart(document, original); err != nil {
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

func (s *ConfigStore) persistAndRestart(document map[string]any, original []byte) error {
	updated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Xray config: %w", err)
	}
	updated = append(updated, '\n')

	mode := os.FileMode(0600)
	uid, gid := -1, -1
	if info, err := os.Stat(s.path); err == nil {
		mode = info.Mode().Perm()
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(stat.Uid), int(stat.Gid)
		}
	}
	if err := writeAtomic(s.path, updated, mode, uid, gid, s.runtime.Validate); err != nil {
		return err
	}
	if err := s.runtime.Restart(); err == nil {
		return nil
	} else {
		restartErr := err
		rollbackErr := writeAtomic(s.path, original, mode, uid, gid, s.runtime.Validate)
		if rollbackErr == nil {
			rollbackErr = s.runtime.Restart()
		}
		if rollbackErr != nil {
			return fmt.Errorf("restart Xray: %v; rollback also failed: %w", restartErr, rollbackErr)
		}
		return fmt.Errorf("restart Xray: %v; original configuration restored", restartErr)
	}
}

func writeAtomic(path string, content []byte, mode os.FileMode, uid, gid int, validate func(string) error) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".vless-api-config-*")
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
	return nil, fmt.Errorf("VLESS inbound %q not found", inboundTag)
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
