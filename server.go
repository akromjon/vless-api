package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type APIServer struct {
	settings AppSettings
	store    *ConfigStore
	runtime  XrayRuntime
}

type APIResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

type ClientResponse struct {
	Name             string `json:"name"`
	UUID             string `json:"uuid"`
	Protocol         string `json:"protocol"`
	Address          string `json:"address"`
	Port             int    `json:"port"`
	ServerName       string `json:"server_name"`
	RealityPublicKey string `json:"reality_public_key"`
	ShortID          string `json:"short_id"`
	Flow             string `json:"flow"`
	Config           string `json:"config"`
}

func NewAPIServer(settings AppSettings, store *ConfigStore, runtime XrayRuntime) *APIServer {
	return &APIServer{settings: settings, store: store, runtime: runtime}
}

func (s *APIServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/users", s.require(http.MethodGet, s.listUsers))
	mux.HandleFunc("/api/users/add", s.require(http.MethodPost, s.addUser))
	mux.HandleFunc("/api/users/add-bulk", s.require(http.MethodPost, s.addUsersBulk))
	mux.HandleFunc("/api/users/delete", s.require(http.MethodPost, s.deleteUser))
	mux.HandleFunc("/api/users/delete-all", s.require(http.MethodPost, s.deleteAllUsers))
	mux.HandleFunc("/api/health", s.require(http.MethodGet, s.health))
	mux.HandleFunc("/api/status", s.require(http.MethodGet, s.status))
	mux.HandleFunc("/api/stats", s.require(http.MethodGet, s.stats))
	mux.HandleFunc("/api/start", s.require(http.MethodPost, s.start))
	mux.HandleFunc("/api/stop", s.require(http.MethodPost, s.stop))
	mux.HandleFunc("/api/restart", s.require(http.MethodPost, s.restart))
	return s.authenticate(mux)
}

func (s *APIServer) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !s.settings.TokenMatches(request.Header.Get("key")) {
			http.NotFound(writer, request)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *APIServer) require(method string, handler http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != method {
			writer.Header().Set("Allow", method)
			writeJSON(writer, http.StatusMethodNotAllowed, APIResponse{Success: false, Message: "method not allowed"})
			return
		}
		handler(writer, request)
	}
}

func (s *APIServer) listUsers(writer http.ResponseWriter, _ *http.Request) {
	records, err := s.store.List()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	clients := make([]ClientResponse, 0, len(records))
	for _, record := range records {
		clients = append(clients, s.clientResponse(record))
	}
	writeJSON(writer, http.StatusOK, APIResponse{Success: true, Data: clients})
}

func (s *APIServer) addUser(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeBody(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	record, err := s.store.Add(body.Name)
	if errors.Is(err, ErrUserExists) {
		writeError(writer, http.StatusConflict, err)
		return
	}
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "name must match") {
			status = http.StatusBadRequest
		}
		writeError(writer, status, err)
		return
	}
	writeJSON(writer, http.StatusOK, APIResponse{Success: true, Message: "user added", Data: s.clientResponse(record)})
}

func (s *APIServer) addUsersBulk(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Names []string `json:"names"`
	}
	if err := decodeBody(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	results, err := s.store.AddBulk(body.Names)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "name") || strings.Contains(err.Error(), "at least") || strings.Contains(err.Error(), "at most") {
			status = http.StatusBadRequest
		}
		writeError(writer, status, err)
		return
	}
	created := 0
	responseResults := make([]map[string]any, 0, len(results))
	for _, result := range results {
		item := map[string]any{"name": result.Name, "success": result.Success, "message": result.Message}
		if result.Success {
			created++
			item["client"] = s.clientResponse(result.User)
		}
		responseResults = append(responseResults, item)
	}
	writeJSON(writer, http.StatusOK, APIResponse{
		Success: created > 0,
		Message: fmt.Sprintf("created %d of %d users", created, len(results)),
		Data: map[string]any{
			"created": created,
			"failed":  len(results) - created,
			"results": responseResults,
		},
	})
}

func (s *APIServer) deleteUser(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeBody(request, &body); err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	err := s.store.Delete(body.Name)
	if errors.Is(err, ErrUserNotFound) {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, APIResponse{Success: true, Message: "user deleted"})
}

func (s *APIServer) deleteAllUsers(writer http.ResponseWriter, _ *http.Request) {
	deleted, err := s.store.DeleteAll()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, APIResponse{Success: true, Message: "all users deleted", Data: map[string]int{"deleted": deleted}})
}

func (s *APIServer) health(writer http.ResponseWriter, _ *http.Request) {
	records, configErr := s.store.List()
	running, runtimeErr := s.runtime.IsActive()
	listening := false
	if running {
		connection, err := net.DialTimeout("tcp", s.settings.VLESSListenAddress(), time.Second)
		if err == nil {
			listening = true
			_ = connection.Close()
		}
	}
	healthy := configErr == nil && runtimeErr == nil && running && listening
	data := map[string]any{
		"healthy":      healthy,
		"running":      running,
		"listening":    listening,
		"config_valid": configErr == nil,
		"protocol":     "vless-reality",
		"transport":    "tcp",
		"port":         s.settings.VLESSPort,
		"users":        len(records),
		"version":      s.runtime.Version(),
	}
	if configErr != nil {
		data["config_error"] = configErr.Error()
	}
	if runtimeErr != nil {
		data["runtime_error"] = runtimeErr.Error()
	}
	writeJSON(writer, http.StatusOK, APIResponse{Success: true, Data: data})
}

func (s *APIServer) status(writer http.ResponseWriter, _ *http.Request) {
	records, err := s.store.List()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	running, err := s.runtime.IsActive()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, APIResponse{Success: true, Data: map[string]any{
		"running":     running,
		"protocol":    "vless-reality",
		"transport":   "tcp",
		"address":     s.settings.PublicAddress,
		"port":        s.settings.VLESSPort,
		"server_name": s.settings.ServerName,
		"users":       len(records),
	}})
}

func (s *APIServer) stats(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusNotImplemented, APIResponse{
		Success: false,
		Message: "per-user Xray traffic statistics are not enabled in the initial durable-config implementation",
	})
}

func (s *APIServer) start(writer http.ResponseWriter, _ *http.Request) {
	s.serviceAction(writer, "started", s.runtime.Start)
}

func (s *APIServer) stop(writer http.ResponseWriter, _ *http.Request) {
	s.serviceAction(writer, "stopped", s.runtime.Stop)
}

func (s *APIServer) restart(writer http.ResponseWriter, _ *http.Request) {
	s.serviceAction(writer, "restarted", s.runtime.Restart)
}

func (s *APIServer) serviceAction(writer http.ResponseWriter, action string, operation func() error) {
	if err := operation(); err != nil {
		writeError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, APIResponse{Success: true, Message: "Xray " + action})
}

func (s *APIServer) clientResponse(record UserRecord) ClientResponse {
	return ClientResponse{
		Name:             record.Name,
		UUID:             record.UUID,
		Protocol:         "vless-reality",
		Address:          s.settings.PublicAddress,
		Port:             s.settings.VLESSPort,
		ServerName:       s.settings.ServerName,
		RealityPublicKey: s.settings.RealityPublicKey,
		ShortID:          s.settings.ShortID,
		Flow:             s.settings.Flow,
		Config:           s.settings.BuildShareURI(record),
	}
}

func decodeBody(request *http.Request, destination any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("invalid JSON body: multiple values")
	}
	return nil
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, APIResponse{Success: false, Message: err.Error()})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
