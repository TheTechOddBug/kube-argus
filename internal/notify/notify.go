// Package notify dispatches JIT lifecycle events to Slack and a generic
// webhook. Wire-up (clientset, namespace, RBAC, audit) is injected by main so
// this package stays free of cross-package imports.
package notify

import (
	"net/http"

	"k8s.io/client-go/kubernetes"
)

// JITPayload is the shape of a JIT request as serialized to webhook bodies and
// rendered into Slack messages. It mirrors the in-memory jit request struct
// the rest of the server uses; main converts between the two.
type JITPayload struct {
	ID         string  `json:"id"`
	Email      string  `json:"email"`
	Namespace  string  `json:"namespace"`
	Pod        string  `json:"pod"`
	OwnerKind  string  `json:"ownerKind"`
	OwnerName  string  `json:"ownerName"`
	Reason     string  `json:"reason"`
	Duration   string  `json:"duration"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"createdAt"`
	ApprovedBy string  `json:"approvedBy,omitempty"`
	ExpiresAt  *string `json:"expiresAt,omitempty"`
}

// Wire-up state shared by slack.go + webhook.go.
var (
	clientset      kubernetes.Interface
	jitCMNamespace string
)

// Cluster identifies the cluster in Slack/webhook payloads. main sets it.
var Cluster = func() string { return "" }

// ClientIP returns the originating IP for an HTTP request. Set by main.
var ClientIP = func(r *http.Request) string { return "" }

// RequireAdmin enforces admin RBAC for the Settings endpoints. Set by main.
var RequireAdmin = func(w http.ResponseWriter, r *http.Request) bool { return true }

// AuditRecord persists an audit trail entry. Set by main.
var AuditRecord = func(actor, role, action, target, detail, ip string) {}

// CurrentUser returns the authenticated user (email, role) for audit context.
// Returns ok=false when there's no session. Set by main.
var CurrentUser = func(r *http.Request) (email, role string, ok bool) { return "", "", false }

// JITApprove/JITDeny/JITLookup are wired by main so Slack's interactive
// buttons can mutate the JIT store without notify importing the jit package.
var (
	JITApprove = func(id, actor, source string) error { return nil }
	JITDeny    = func(id, actor, source string) error { return nil }
	JITLookup  = func(id string) *JITPayload { return nil }
)

// Init wires in the kube clientset and the namespace used to store the
// configmap-backed settings for slack + webhook.
func Init(cs kubernetes.Interface, namespace string) {
	clientset = cs
	jitCMNamespace = namespace
}

// JIT fans an event out to Slack + webhook. `event` is one of
// jit.requested, jit.approved, jit.denied, jit.revoked, jit.expired.
func JIT(event string, p *JITPayload, actor string) {
	if p == nil {
		return
	}
	switch event {
	case "jit.requested":
		slackNotifyJITRequested(p)
	case "jit.approved":
		slackNotifyJITResult(p, "approve", actor)
	case "jit.denied":
		slackNotifyJITResult(p, "deny", actor)
	case "jit.revoked":
		slackNotifyJITResult(p, "revoke", actor)
	case "jit.expired":
		// no Slack notification on auto-expire (matches prior behavior).
	}
	webhookNotifyJIT(event, p, actor)
}
