// Package permissions holds the access-control logic: roles, the unknown-sender
// policy, per-wiring sender scope, and (eventually) the approval-card flows.
// This is pure logic over DB rows (brief §3.4, §9) and has no I/O of its own.
package permissions

// Role is a user's role within the host.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// UnknownSenderPolicy governs what happens when a sender isn't a known user.
type UnknownSenderPolicy string

const (
	// PolicyPublic accepts anyone.
	PolicyPublic UnknownSenderPolicy = "public"
	// PolicyStrict rejects unknown senders.
	PolicyStrict UnknownSenderPolicy = "strict"
	// PolicyRequestApproval routes unknown senders through an approval flow.
	PolicyRequestApproval UnknownSenderPolicy = "request_approval"
)

// SenderScope narrows which senders in a wired conversation are allowed.
type SenderScope string

const (
	ScopeAll   SenderScope = "all"
	ScopeKnown SenderScope = "known"
	ScopeOwner SenderScope = "owner"
)

// Decision is the outcome of an access check.
type Decision int

const (
	// Deny drops the message.
	Deny Decision = iota
	// Allow forwards the message into inbound.db.
	Allow
	// NeedsApproval triggers the approval-card flow.
	NeedsApproval
)

// Request carries the resolved facts an access check needs.
type Request struct {
	KnownUser bool
	Role      Role
	Scope     SenderScope
	Policy    UnknownSenderPolicy
}

// Check decides whether a message may proceed. This is the v0 gate; the
// approval-card flow (NeedsApproval) is wired but not yet implemented.
func Check(r Request) Decision {
	if !r.KnownUser {
		switch r.Policy {
		case PolicyPublic:
			return Allow
		case PolicyRequestApproval:
			return NeedsApproval
		default: // PolicyStrict
			return Deny
		}
	}
	switch r.Scope {
	case ScopeOwner:
		if r.Role == RoleOwner {
			return Allow
		}
		return Deny
	case ScopeKnown, ScopeAll:
		return Allow
	default:
		return Deny
	}
}
