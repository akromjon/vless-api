package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// XrayLiveAPI applies user changes to the RUNNING Xray instance and reads
// per-user counters, both over Xray's gRPC API (HandlerService / StatsService).
//
// Why this exists: every mutation used to go through persistAndRestart, and a
// restart drops every established TCP session on the node. That is acceptable
// for a one-off bulk provision, but it makes per-release credential rotation
// impossible — rotating one config would disconnect every other user on the
// box. HandlerService adds and removes users in place, with no restart.
//
// Runtime changes made this way are NOT persisted by Xray, so the caller must
// keep writing config.json as well. The file remains the source of truth across
// restarts; this interface only keeps the live process in step with it.
type XrayLiveAPI interface {
	AddUsers(inboundTag string, port int, flow string, users []UserRecord) error
	RemoveUsers(inboundTag string, names []string) error
	InboundUsers(inboundTag string) ([]string, error)
	UserTraffic(reset bool) ([]UserTraffic, error)
	Probe(inboundTag string) error
	Available() bool
}

// UserTraffic is one config's byte counters. Uplink/Downlink are cumulative
// since the last reset, so a caller polling with reset=true reads deltas.
type UserTraffic struct {
	Name     string `json:"name"`
	Uplink   int64  `json:"uplink"`
	Downlink int64  `json:"downlink"`
}

// CommandLiveAPI drives the API through the `xray api` subcommands rather than
// linking Xray's protobufs into this binary. The CLI ships with every node we
// deploy, so this keeps the dependency surface at zero and the failure mode
// legible (a non-zero exit with the CLI's own error text).
type CommandLiveAPI struct {
	Runtime    CommandRuntime
	APIAddress string
}

func (c CommandLiveAPI) Available() bool {
	return strings.TrimSpace(c.APIAddress) != ""
}

// Probe checks that an Xray API is actually answering before the store is
// allowed to depend on it.
//
// A configured address is not proof of anything: a node whose config.json has
// no api/stats blocks still inherits the default address, and every live apply
// against it would fail, roll the file back, and restart Xray -- turning a
// routine add into an outage. Callers must treat a probe failure as "no live
// API" and fall back to the restart path.
func (c CommandLiveAPI) Probe(inboundTag string) error {
	if !c.Available() {
		return fmt.Errorf("Xray API address is not configured")
	}
	if _, err := c.run("inboundusercount", "-tag="+inboundTag); err != nil {
		return err
	}
	return nil
}

// AddUsers registers users on the live inbound. The payload mirrors an Xray
// config fragment; `decryption` is required or the VLESS inbound settings fail
// to build and the CLI reports "Added 0 user(s)" WITHOUT a non-zero exit.
func (c CommandLiveAPI) AddUsers(inboundTag string, port int, flow string, users []UserRecord) error {
	if len(users) == 0 {
		return nil
	}
	clients := make([]map[string]any, 0, len(users))
	for _, user := range users {
		client := map[string]any{"id": user.UUID, "email": user.Name}
		if flow != "" {
			client["flow"] = flow
		}
		clients = append(clients, client)
	}
	payload := map[string]any{"inbounds": []any{map[string]any{
		"tag":      inboundTag,
		"port":     port,
		"protocol": "vless",
		"settings": map[string]any{"decryption": "none", "clients": clients},
	}}}

	path, cleanup, err := writeTempJSON(payload)
	if err != nil {
		return err
	}
	defer cleanup()

	output, err := c.run("adu", path)
	if err != nil {
		return err
	}
	// The CLI exits 0 even when it adds nothing, so the count is the only
	// signal that the payload was understood.
	if added := parseAffected(output); added != len(users) {
		return fmt.Errorf("xray api adu applied %d of %d users: %s", added, len(users), strings.TrimSpace(output))
	}
	return nil
}

// RemoveUsers revokes users on the live inbound. Removing a name that is not
// present is treated as success: the caller's intent is that the credential no
// longer works, and a config.json rewrite may already have removed it.
func (c CommandLiveAPI) RemoveUsers(inboundTag string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	args := append([]string{"-tag=" + inboundTag}, names...)
	if _, err := c.run("rmu", args...); err != nil {
		return err
	}
	return nil
}

// InboundUsers lists the names the RUNNING Xray currently accepts on an
// inbound. The file and the live process can disagree -- a live apply that
// never reached disk, or a restart that reloaded a file the live process had
// moved past -- and `adu` aborts the whole batch on the first duplicate, so
// reconciliation needs the live set, not the file's.
func (c CommandLiveAPI) InboundUsers(inboundTag string) ([]string, error) {
	output, err := c.run("inbounduser", "-tag="+inboundTag)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return []string{}, nil
	}
	var document struct {
		Users []struct {
			Email string `json:"email"`
		} `json:"users"`
	}
	if err := json.Unmarshal([]byte(trimmed), &document); err != nil {
		return nil, fmt.Errorf("parse Xray inbounduser output: %w", err)
	}
	names := make([]string, 0, len(document.Users))
	for _, user := range document.Users {
		if user.Email != "" {
			names = append(names, user.Email)
		}
	}
	return names, nil
}

// UserTraffic reads the per-user counters. Names come back inside stat keys
// shaped `user>>><email>>>>traffic>>>uplink`.
func (c CommandLiveAPI) UserTraffic(reset bool) ([]UserTraffic, error) {
	args := []string{"-pattern=user>>>"}
	if reset {
		args = append(args, "-reset")
	}
	output, err := c.run("statsquery", args...)
	if err != nil {
		return nil, err
	}
	return parseUserTraffic(output)
}

// run invokes `xray api <subcommand> --server=<addr> [args...]`.
//
// Argument order matters: the CLI parses the subcommand first and rejects
// anything before it with "unknown command", so --server cannot lead.
func (c CommandLiveAPI) run(subcommand string, args ...string) (string, error) {
	if !c.Available() {
		return "", fmt.Errorf("Xray API address is not configured")
	}
	full := append([]string{"api", subcommand, "--server=" + c.APIAddress}, args...)
	return c.Runtime.run(c.Runtime.XrayBinary, full...)
}

func writeTempJSON(payload any) (string, func(), error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("encode Xray API payload: %w", err)
	}
	file, err := os.CreateTemp("", "vless-api-xray-*.json")
	if err != nil {
		return "", nil, fmt.Errorf("create Xray API payload: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	if _, err := file.Write(encoded); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write Xray API payload: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close Xray API payload: %w", err)
	}
	return file.Name(), cleanup, nil
}

// parseAffected reads the trailing count out of "Added N user(s) in total."
func parseAffected(output string) int {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		if !strings.EqualFold(fields[0], "added") && !strings.EqualFold(fields[0], "removed") {
			continue
		}
		count := 0
		if _, err := fmt.Sscanf(fields[1], "%d", &count); err == nil {
			return count
		}
	}
	return -1
}

// parseUserTraffic turns the statsquery JSON document into per-name totals.
// A stat only appears once it is non-zero, so a user with no traffic since the
// last reset is simply absent rather than reported as zero.
func parseUserTraffic(output string) ([]UserTraffic, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return []UserTraffic{}, nil
	}
	var document struct {
		Stat []struct {
			Name  string `json:"name"`
			Value int64  `json:"value"`
		} `json:"stat"`
	}
	if err := json.Unmarshal([]byte(trimmed), &document); err != nil {
		return nil, fmt.Errorf("parse Xray statsquery output: %w", err)
	}

	byName := map[string]*UserTraffic{}
	for _, stat := range document.Stat {
		parts := strings.Split(stat.Name, ">>>")
		if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
			continue
		}
		entry, ok := byName[parts[1]]
		if !ok {
			entry = &UserTraffic{Name: parts[1]}
			byName[parts[1]] = entry
		}
		switch parts[3] {
		case "uplink":
			entry.Uplink = stat.Value
		case "downlink":
			entry.Downlink = stat.Value
		}
	}

	results := make([]UserTraffic, 0, len(byName))
	for _, entry := range byName {
		results = append(results, *entry)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results, nil
}
