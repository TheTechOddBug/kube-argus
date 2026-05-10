package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ─── Audit Trail ────────────────────────────────────────────────────

type auditEntry struct {
	Time     time.Time `json:"time"`
	Actor    string    `json:"actor"`
	Role     string    `json:"role"`
	Action   string    `json:"action"`
	Resource string    `json:"resource,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	IP       string    `json:"ip"`
}

var auditTrail struct {
	mu      sync.Mutex
	entries []auditEntry
}

const auditMaxBytes = 900_000 // safety cap: ~900KB to stay under ConfigMap 1MB limit

var auditRetention time.Duration

// ─── Online Users (WebSocket presence) ──────────────────────────────

type onlineUser struct {
	Email    string    `json:"email"`
	Role     string    `json:"role"`
	LastSeen time.Time `json:"lastSeen"`
	IP       string    `json:"ip"`
}

// presenceConn tracks one WebSocket connection per user session.
// gorilla *websocket.Conn is not safe for concurrent writes, so wmu serialises
// every write (broadcasts, ping control frames, initial snapshot).
type presenceConn struct {
	email string
	role  string
	ip    string
	conn  *websocket.Conn
	wmu   sync.Mutex
}

func (pc *presenceConn) writeMessage(t int, data []byte) error {
	pc.wmu.Lock()
	defer pc.wmu.Unlock()
	return pc.conn.WriteMessage(t, data)
}

func (pc *presenceConn) writeControl(t int, data []byte, deadline time.Time) error {
	pc.wmu.Lock()
	defer pc.wmu.Unlock()
	return pc.conn.WriteControl(t, data, deadline)
}

var presence struct {
	mu    sync.Mutex
	conns map[*presenceConn]struct{}
}

func presenceAdd(pc *presenceConn) {
	presence.mu.Lock()
	if presence.conns == nil {
		presence.conns = make(map[*presenceConn]struct{})
	}
	presence.conns[pc] = struct{}{}
	presence.mu.Unlock()
	broadcastPresence()
}

func presenceRemove(pc *presenceConn) {
	presence.mu.Lock()
	delete(presence.conns, pc)
	presence.mu.Unlock()
	broadcastPresence()
}

// wsEnvelope tags broadcast messages so the client can route them.
type wsEnvelope struct {
	Type string `json:"type"` // "presence" | "audit"
	Data any    `json:"data"`
}

// snapshotAdminConns returns a slice of all currently-connected admin presence
// conns. Caller writes outside the lock to avoid blocking new connect/disconnect.
func snapshotAdminConns() []*presenceConn {
	presence.mu.Lock()
	defer presence.mu.Unlock()
	out := make([]*presenceConn, 0, len(presence.conns))
	for pc := range presence.conns {
		if pc.role == "admin" {
			out = append(out, pc)
		}
	}
	return out
}

// broadcastToAdmins serialises the envelope once and writes it to every admin conn.
func broadcastToAdmins(env wsEnvelope) {
	msg, err := json.Marshal(env)
	if err != nil {
		return
	}
	for _, pc := range snapshotAdminConns() {
		pc.writeMessage(websocket.TextMessage, msg)
	}
}

// broadcastPresence sends the current user list to all connected admin clients.
func broadcastPresence() {
	broadcastToAdmins(wsEnvelope{Type: "presence", Data: getOnlineUsers()})
}

// broadcastAuditEntry pushes a single new audit record to all admins.
func broadcastAuditEntry(e auditEntry) {
	broadcastToAdmins(wsEnvelope{Type: "audit", Data: e})
}

func getOnlineUsers() []onlineUser {
	// Deduplicate by email — keep most recent connection per user.
	seen := make(map[string]*onlineUser)
	presence.mu.Lock()
	for pc := range presence.conns {
		if existing, ok := seen[pc.email]; !ok || existing == nil {
			seen[pc.email] = &onlineUser{Email: pc.email, Role: pc.role, LastSeen: time.Now(), IP: pc.ip}
		}
	}
	presence.mu.Unlock()
	out := make([]onlineUser, 0, len(seen))
	for _, u := range seen {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// apiPresenceWS upgrades to WebSocket for real-time presence tracking.
// All authenticated users connect; only admins receive broadcast updates.
func apiPresenceWS(w http.ResponseWriter, r *http.Request) {
	sd, ok := r.Context().Value(userCtxKey).(*sessionData)
	if !ok || sd == nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("presence ws upgrade failed", "error", err)
		return
	}
	pc := &presenceConn{email: sd.Email, role: sd.Role, ip: clientIP(r), conn: conn}
	presenceAdd(pc)
	defer func() {
		presenceRemove(pc)
		conn.Close()
	}()

	// If admin, send initial user list immediately.
	if sd.Role == "admin" {
		if msg, err := json.Marshal(wsEnvelope{Type: "presence", Data: getOnlineUsers()}); err == nil {
			pc.writeMessage(websocket.TextMessage, msg)
		}
	}

	// Keep connection alive — read pongs, discard any client messages.
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})
	// Ping loop
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := pc.writeControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}()
	// Block on reads until disconnect
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

// trackUser is kept for backward compat (audit login records etc.)
// but presence is now driven by WebSocket connections.
func trackUser(_, _, _ string) {}

// auditDirty is set whenever a new entry is recorded; the persist ticker
// reads-and-clears it to coalesce many records into a single ConfigMap write.
var auditDirty atomic.Bool

func auditRecord(actor, role, action, resource, detail, ip string) {
	e := auditEntry{
		Time: time.Now(), Actor: actor, Role: role,
		Action: action, Resource: resource, Detail: detail, IP: ip,
	}
	auditTrail.mu.Lock()
	auditTrail.entries = append(auditTrail.entries, e)
	auditTrail.entries = auditTrimExpired(auditTrail.entries)
	auditTrail.mu.Unlock()
	slog.Info("audit", "actor", actor, "action", action, "resource", resource, "detail", detail, "ip", ip)
	auditDirty.Store(true)
	broadcastAuditEntry(e)
}

// auditTrimExpired removes entries older than the retention period.
func auditTrimExpired(entries []auditEntry) []auditEntry {
	cutoff := time.Now().Add(-auditRetention)
	n := 0
	for _, e := range entries {
		if !e.Time.Before(cutoff) {
			entries[n] = e
			n++
		}
	}
	return entries[:n]
}

// ─── Audit ConfigMap Persistence ─────────────────────────────────────

var (
	auditCMName    string
	auditPersistOn bool
)

func auditInitPersistence() {
	auditCMName = os.Getenv("AUDIT_CONFIGMAP_NAME")
	if auditCMName == "" {
		auditCMName = "kube-argus-audit"
	}

	days := 15
	if v := os.Getenv("AUDIT_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	auditRetention = time.Duration(days) * 24 * time.Hour

	auditPersistOn = true
	slog.Info("audit: persistence enabled", "configmap", jitCMNamespace+"/"+auditCMName, "retention_days", days)
	auditStartSync()
}

func auditRestore() {
	if !auditPersistOn {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cm, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Get(ctx, auditCMName, metav1.GetOptions{})
	if err != nil {
		if k8serr.IsNotFound(err) {
			return
		}
		slog.Error("audit: restore failed", "error", err)
		return
	}

	data, ok := cm.Data["audit.json"]
	if !ok || data == "" {
		return
	}

	var loaded []auditEntry
	if err := json.Unmarshal([]byte(data), &loaded); err != nil {
		slog.Error("audit: restore unmarshal failed", "error", err)
		return
	}

	loaded = auditTrimExpired(loaded)
	auditTrail.mu.Lock()
	auditTrail.entries = loaded
	auditTrail.mu.Unlock()
	slog.Info("audit: restored entries from configmap", "count", len(loaded))
}

func auditPersist() {
	if !auditPersistOn {
		return
	}

	auditTrail.mu.Lock()
	auditTrail.entries = auditTrimExpired(auditTrail.entries)
	snapshot := make([]auditEntry, len(auditTrail.entries))
	copy(snapshot, auditTrail.entries)
	auditTrail.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cm, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Get(ctx, auditCMName, metav1.GetOptions{})
	if k8serr.IsNotFound(err) {
		raw, err := json.Marshal(snapshot)
		if err != nil {
			slog.Error("audit: persist marshal failed", "error", err)
			return
		}
		newCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      auditCMName,
				Namespace: jitCMNamespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kube-argus"},
			},
			Data: map[string]string{"audit.json": string(raw)},
		}
		if _, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Create(ctx, newCM, metav1.CreateOptions{}); err != nil {
			slog.Error("audit: persist create failed", "error", err)
			auditDirty.Store(true)
		}
		return
	}
	if err != nil {
		slog.Error("audit: persist get failed", "error", err)
		auditDirty.Store(true)
		return
	}

	// Read-merge-write: merge local entries with what other pods have written.
	if existing := cm.Data["audit.json"]; existing != "" {
		var remote []auditEntry
		if err := json.Unmarshal([]byte(existing), &remote); err == nil {
			snapshot = auditDedup(snapshot, remote)
			snapshot = auditTrimExpired(snapshot)
		}
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		slog.Error("audit: persist marshal failed", "error", err)
		return
	}

	// Drop oldest entries until we fit under the ConfigMap size limit.
	for len(raw) > auditMaxBytes && len(snapshot) > 0 {
		snapshot = snapshot[:len(snapshot)-1]
		raw, _ = json.Marshal(snapshot)
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["audit.json"] = string(raw)

	if _, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		// Re-mark dirty so the next ticker cycle retries (with no goroutine storm).
		auditDirty.Store(true)
		if k8serr.IsConflict(err) {
			slog.Warn("audit: persist conflict, will retry next cycle")
			return
		}
		slog.Error("audit: persist update failed", "error", err)
	}
}

// auditSync pulls entries from the ConfigMap that other pods may have written
// and merges them into local in-memory state. Runs periodically in background.
func auditSync() {
	if !auditPersistOn {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cm, err := clientset.CoreV1().ConfigMaps(jitCMNamespace).Get(ctx, auditCMName, metav1.GetOptions{})
	if err != nil {
		return
	}
	data, ok := cm.Data["audit.json"]
	if !ok || data == "" {
		return
	}
	var remote []auditEntry
	if err := json.Unmarshal([]byte(data), &remote); err != nil {
		return
	}

	auditTrail.mu.Lock()
	merged := auditDedup(auditTrail.entries, remote)
	auditTrail.entries = auditTrimExpired(merged)
	auditTrail.mu.Unlock()
}

func auditStartSync() {
	// Read-merge from peers every 10s.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			auditSync()
		}
	}()
	// Write our local changes every 5s, but only when something is dirty.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			if auditDirty.CompareAndSwap(true, false) {
				auditPersist()
			}
		}
	}()
}

// auditDedup merges two slices of audit entries, deduplicating by (time, actor, action, resource).
func auditDedup(a, b []auditEntry) []auditEntry {
	type key struct {
		t        int64
		actor    string
		action   string
		resource string
	}
	seen := make(map[key]struct{}, len(a)+len(b))
	out := make([]auditEntry, 0, len(a)+len(b))
	for _, list := range [2][]auditEntry{a, b} {
		for _, e := range list {
			k := key{e.Time.UnixMilli(), e.Actor, e.Action, e.Resource}
			if _, exists := seen[k]; !exists {
				seen[k] = struct{}{}
				out = append(out, e)
			}
		}
	}
	return out
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.SplitN(fwd, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	host, _, _ := strings.Cut(r.RemoteAddr, ":")
	return host
}

func apiAudit(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	action := r.URL.Query().Get("action")
	auditTrail.mu.Lock()
	src := make([]auditEntry, len(auditTrail.entries))
	copy(src, auditTrail.entries)
	auditTrail.mu.Unlock()
	sort.Slice(src, func(i, j int) bool { return src[i].Time.After(src[j].Time) })
	if action != "" {
		filtered := make([]auditEntry, 0)
		for _, e := range src {
			if e.Action == action {
				filtered = append(filtered, e)
			}
		}
		src = filtered
	}
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	if len(src) > limit {
		src = src[:limit]
	}
	j(w, src)
}
