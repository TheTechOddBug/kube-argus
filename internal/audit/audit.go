// Package audit records actor/action events to in-memory + ConfigMap-backed
// storage and broadcasts them to connected admin WebSocket presence clients.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"kube-argus/internal/auth"
	"kube-argus/internal/httpx"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ─── Audit Trail ────────────────────────────────────────────────────

// Entry is a single audit-trail record.
type Entry struct {
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
	entries []Entry
}

const auditMaxBytes = 900_000 // safety cap: ~900KB to stay under ConfigMap 1MB limit

// Retention controls how long entries are kept; set by Init.
var Retention time.Duration

// ─── Online Users (WebSocket presence) ──────────────────────────────

// OnlineUser is a presence record returned by GetOnlineUsers.
type OnlineUser struct {
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
	broadcastToAdmins(wsEnvelope{Type: "presence", Data: GetOnlineUsers()})
}

// broadcastAuditEntry pushes a single new audit record to all admins.
func broadcastAuditEntry(e Entry) {
	broadcastToAdmins(wsEnvelope{Type: "audit", Data: e})
}

// GetOnlineUsers returns the deduplicated list of currently-connected users.
func GetOnlineUsers() []OnlineUser {
	seen := make(map[string]*OnlineUser)
	presence.mu.Lock()
	for pc := range presence.conns {
		if existing, ok := seen[pc.email]; !ok || existing == nil {
			seen[pc.email] = &OnlineUser{Email: pc.email, Role: pc.role, LastSeen: time.Now(), IP: pc.ip}
		}
	}
	presence.mu.Unlock()
	out := make([]OnlineUser, 0, len(seen))
	for _, u := range seen {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// PresenceHandler upgrades to WebSocket for real-time presence tracking.
// All authenticated users connect; only admins receive broadcast updates.
func PresenceHandler(w http.ResponseWriter, r *http.Request) {
	sd := auth.Session(r)
	if sd == nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	conn, err := httpx.WSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("presence ws upgrade failed", "error", err)
		return
	}
	pc := &presenceConn{email: sd.Email, role: sd.Role, ip: auth.ClientIP(r), conn: conn}
	presenceAdd(pc)
	defer func() {
		presenceRemove(pc)
		conn.Close()
	}()

	if sd.Role == "admin" {
		if msg, err := json.Marshal(wsEnvelope{Type: "presence", Data: GetOnlineUsers()}); err == nil {
			pc.writeMessage(websocket.TextMessage, msg)
		}
	}

	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := pc.writeControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}()
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

// TrackUser is the no-op called from the auth middleware. Presence is driven
// by WebSocket connections now; retained for backward compat (audit login
// records flow through Record, not TrackUser).
func TrackUser(_, _, _ string) {}

// auditDirty is set whenever a new entry is recorded; the persist ticker
// reads-and-clears it to coalesce many records into a single ConfigMap write.
var auditDirty atomic.Bool

// Record appends a new audit entry, trims expired entries, and notifies any
// connected admin presence WebSockets.
func Record(actor, role, action, resource, detail, ip string) {
	e := Entry{
		Time: time.Now(), Actor: actor, Role: role,
		Action: action, Resource: resource, Detail: detail, IP: ip,
	}
	auditTrail.mu.Lock()
	auditTrail.entries = append(auditTrail.entries, e)
	auditTrail.entries = TrimExpired(auditTrail.entries)
	auditTrail.mu.Unlock()
	slog.Info("audit", "actor", actor, "action", action, "resource", resource, "detail", detail, "ip", ip)
	auditDirty.Store(true)
	broadcastAuditEntry(e)
}

// TrimExpired removes entries older than the retention period.
// If Retention is zero (Init hasn't run yet), returns entries unchanged —
// otherwise cutoff=time.Now() would silently drop every entry.
func TrimExpired(entries []Entry) []Entry {
	if Retention == 0 {
		return entries
	}
	cutoff := time.Now().Add(-Retention)
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
	clientset      kubernetes.Interface
	auditCMName    string
	auditNamespace string
	auditPersistOn bool
)

// InitPersistence wires the kube client and namespace and starts the
// background sync/persist loops.
func InitPersistence(cs kubernetes.Interface, namespace string) {
	clientset = cs
	auditNamespace = namespace

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
	Retention = time.Duration(days) * 24 * time.Hour

	auditPersistOn = true
	slog.Info("audit: persistence enabled", "configmap", auditNamespace+"/"+auditCMName, "retention_days", days)
	startSync()
}

// Restore loads any persisted entries from the ConfigMap into memory.
func Restore() {
	if !auditPersistOn || clientset == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cm, err := clientset.CoreV1().ConfigMaps(auditNamespace).Get(ctx, auditCMName, metav1.GetOptions{})
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

	var loaded []Entry
	if err := json.Unmarshal([]byte(data), &loaded); err != nil {
		slog.Error("audit: restore unmarshal failed", "error", err)
		return
	}

	loaded = TrimExpired(loaded)
	auditTrail.mu.Lock()
	auditTrail.entries = loaded
	auditTrail.mu.Unlock()
	slog.Info("audit: restored entries from configmap", "count", len(loaded))
}

func persist() {
	if !auditPersistOn || clientset == nil {
		return
	}

	auditTrail.mu.Lock()
	auditTrail.entries = TrimExpired(auditTrail.entries)
	snapshot := make([]Entry, len(auditTrail.entries))
	copy(snapshot, auditTrail.entries)
	auditTrail.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cm, err := clientset.CoreV1().ConfigMaps(auditNamespace).Get(ctx, auditCMName, metav1.GetOptions{})
	if k8serr.IsNotFound(err) {
		raw, err := json.Marshal(snapshot)
		if err != nil {
			slog.Error("audit: persist marshal failed", "error", err)
			return
		}
		newCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      auditCMName,
				Namespace: auditNamespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kube-argus"},
			},
			Data: map[string]string{"audit.json": string(raw)},
		}
		if _, err := clientset.CoreV1().ConfigMaps(auditNamespace).Create(ctx, newCM, metav1.CreateOptions{}); err != nil {
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

	if existing := cm.Data["audit.json"]; existing != "" {
		var remote []Entry
		if err := json.Unmarshal([]byte(existing), &remote); err == nil {
			snapshot = Dedup(snapshot, remote)
			snapshot = TrimExpired(snapshot)
		}
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		slog.Error("audit: persist marshal failed", "error", err)
		return
	}

	for len(raw) > auditMaxBytes && len(snapshot) > 0 {
		snapshot = snapshot[:len(snapshot)-1]
		raw, _ = json.Marshal(snapshot)
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["audit.json"] = string(raw)

	if _, err := clientset.CoreV1().ConfigMaps(auditNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		auditDirty.Store(true)
		if k8serr.IsConflict(err) {
			slog.Warn("audit: persist conflict, will retry next cycle")
			return
		}
		slog.Error("audit: persist update failed", "error", err)
	}
}

// syncFromCM pulls entries from the ConfigMap that other pods may have written
// and merges them into local in-memory state.
func syncFromCM() {
	if !auditPersistOn || clientset == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cm, err := clientset.CoreV1().ConfigMaps(auditNamespace).Get(ctx, auditCMName, metav1.GetOptions{})
	if err != nil {
		return
	}
	data, ok := cm.Data["audit.json"]
	if !ok || data == "" {
		return
	}
	var remote []Entry
	if err := json.Unmarshal([]byte(data), &remote); err != nil {
		return
	}

	auditTrail.mu.Lock()
	merged := Dedup(auditTrail.entries, remote)
	auditTrail.entries = TrimExpired(merged)
	auditTrail.mu.Unlock()
}

func startSync() {
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			syncFromCM()
		}
	}()
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			if auditDirty.CompareAndSwap(true, false) {
				persist()
			}
		}
	}()
}

// Dedup merges two slices of audit entries, deduplicating by (time, actor, action, resource).
func Dedup(a, b []Entry) []Entry {
	type key struct {
		t        int64
		actor    string
		action   string
		resource string
	}
	seen := make(map[key]struct{}, len(a)+len(b))
	out := make([]Entry, 0, len(a)+len(b))
	for _, list := range [2][]Entry{a, b} {
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

// Handler serves GET /api/audit (admin only) with optional action= filter.
func Handler(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireAdmin(w, r) {
		return
	}
	action := r.URL.Query().Get("action")
	auditTrail.mu.Lock()
	src := make([]Entry, len(auditTrail.entries))
	copy(src, auditTrail.entries)
	auditTrail.mu.Unlock()
	sort.Slice(src, func(i, j int) bool { return src[i].Time.After(src[j].Time) })
	if action != "" {
		filtered := make([]Entry, 0)
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
	httpx.JSON(w, src)
}
