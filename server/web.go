package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"time"

	"monitor/internal/transport"
	"monitor/protocol"
)

//go:embed web/*
var webAssets embed.FS

func (h *hub) webRoutes(user, password string) http.Handler {
	mux := http.NewServeMux()
	files, _ := fs.Sub(webAssets, "web")
	mux.Handle("GET /", http.FileServer(http.FS(files)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/hosts", h.listHosts)
	mux.HandleFunc("GET /api/hosts/{id}", h.hostDetails)
	mux.HandleFunc("GET /api/hosts/{id}/history", h.hostHistory)
	mux.HandleFunc("POST /api/hosts/{id}/query/{kind}", h.liveQuery)
	wantUser, wantPass := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if r.URL.Path != "/healthz" {
			u, p, ok := r.BasicAuth()
			hashedUser, hashedPass := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(p))
			match := subtle.ConstantTimeCompare(wantUser[:], hashedUser[:]) & subtle.ConstantTimeCompare(wantPass[:], hashedPass[:])
			if !ok || match != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="Monitor", charset="UTF-8"`)
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

type hostSummary struct {
	ID         string   `json:"host_id"`
	Name       string   `json:"hostname"`
	OS         string   `json:"os"`
	Arch       string   `json:"architecture"`
	BootedAt   string   `json:"booted_at"`
	LastSeenMS int64    `json:"last_seen_ms"`
	Online     bool     `json:"online"`
	Collection string   `json:"collection_status"`
	CPU        *float64 `json:"cpu_percent"`
	Memory     *float64 `json:"memory_percent"`
	Root       *float64 `json:"root_percent"`
}

func (h *hub) listHosts(w http.ResponseWriter, r *http.Request) {
	records, err := h.store.hosts(r.Context(), "")
	if err != nil {
		storageError(w, err)
		return
	}
	summaries := make([]hostSummary, 0, len(records))
	now := time.Now()
	for _, record := range records {
		h.state(&record, now)
		summaries = append(summaries, summarize(record))
	}
	writeJSON(w, http.StatusOK, summaries)
}

func summarize(record hostRecord) hostSummary {
	result := hostSummary{ID: record.ID, Name: record.ID, LastSeenMS: record.LastSeenMS,
		Online: record.Online, Collection: record.Collecting}
	var inventory struct {
		System map[string]string `json:"system"`
	}
	var metric struct {
		BootedAt string            `json:"booted_at"`
		Memory   map[string]uint64 `json:"memory_bytes"`
		Windows  []struct {
			CPU map[string]map[string]float64 `json:"cpu_percent"`
		} `json:"windows"`
	}
	var filesystems struct {
		Mounts []struct {
			Target   string            `json:"target"`
			Capacity map[string]uint64 `json:"capacity"`
		} `json:"mounts"`
	}
	_ = json.Unmarshal(record.Inventory, &inventory)
	_ = json.Unmarshal(record.Metrics, &metric)
	_ = json.Unmarshal(record.Filesystems, &filesystems)
	if name := inventory.System["hostname"]; name != "" {
		result.Name = name
	}
	result.OS = inventory.System["pretty_name"]
	result.Arch = inventory.System["architecture"]
	result.BootedAt = metric.BootedAt
	if result.BootedAt == "" {
		result.BootedAt = inventory.System["booted_at"]
	}
	if len(metric.Windows) > 0 {
		if busy, ok := metric.Windows[0].CPU["cpu"]["busy"]; ok {
			result.CPU = &busy
		}
	}
	result.Memory = utilization(metric.Memory, "used", "total")
	for _, mount := range filesystems.Mounts {
		if mount.Target == "/" {
			result.Root = utilization(mount.Capacity, "used_bytes", "total_bytes")
			break
		}
	}
	return result
}

func (h *hub) hostDetails(w http.ResponseWriter, r *http.Request) {
	records, err := h.store.hosts(r.Context(), r.PathValue("id"))
	if err != nil {
		storageError(w, err)
		return
	}
	if len(records) == 0 {
		writeError(w, http.StatusNotFound, "unknown host")
		return
	}
	h.state(&records[0], time.Now())
	writeJSON(w, http.StatusOK, records[0])
}

func (h *hub) hostHistory(w http.ResponseWriter, r *http.Request) {
	duration := time.Hour
	if value := r.URL.Query().Get("range"); value != "" {
		var err error
		duration, err = time.ParseDuration(value)
		if err != nil || duration <= 0 || duration > 7*24*time.Hour {
			writeError(w, http.StatusBadRequest, "range must be between 0 and 168h")
			return
		}
	}
	since := time.Now().Add(-duration)
	if value := r.URL.Query().Get("since_ms"); value != "" {
		ms, err := strconv.ParseInt(value, 10, 64)
		if err != nil || ms < 0 {
			writeError(w, http.StatusBadRequest, "since_ms must be a nonnegative Unix timestamp in milliseconds")
			return
		}
		if cursor := time.UnixMilli(ms); cursor.After(since) {
			since = cursor
		}
	}
	points, err := h.store.history(r.Context(), r.PathValue("id"), since)
	if err != nil {
		storageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, points)
}

func (h *hub) liveQuery(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "application/json required")
		return
	}
	id := r.PathValue("id")
	if !transport.ValidHostID(id) {
		writeError(w, http.StatusBadRequest, "invalid host ID")
		return
	}
	kind := protocol.Query_KIND_LATEST
	switch r.PathValue("kind") {
	case "inventory":
		kind = protocol.Query_KIND_INVENTORY
	case "latest":
	default:
		writeError(w, http.StatusBadRequest, "unsupported query")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	snap, err := h.query(ctx, id, kind)
	switch {
	case errors.Is(err, errOffline):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errBusy):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "agent query timed out")
	case err != nil:
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusOK, struct {
			Metrics     json.RawMessage `json:"metrics,omitempty"`
			Filesystems json.RawMessage `json:"filesystems,omitempty"`
			Inventory   json.RawMessage `json:"inventory,omitempty"`
		}{snap.MetricsJson, snap.FilesystemsJson, snap.InventoryJson})
	}
}

func storageError(w http.ResponseWriter, err error) {
	log.Printf("read storage: %v", err)
	writeError(w, http.StatusInternalServerError, "storage unavailable")
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}
