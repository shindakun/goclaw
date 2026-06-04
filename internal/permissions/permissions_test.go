package permissions

import "testing"

// TestCheck exhaustively covers the v0 access gate: unknown senders gate on the
// policy; known senders gate on the wiring's sender scope. A bug here is a
// security hole, so every meaningful combination is asserted.
func TestCheck(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want Decision
	}{
		// Unknown sender: decided purely by policy, scope/role irrelevant.
		{"unknown + public allows", Request{KnownUser: false, Policy: PolicyPublic}, Allow},
		{"unknown + strict denies", Request{KnownUser: false, Policy: PolicyStrict}, Deny},
		{"unknown + request_approval", Request{KnownUser: false, Policy: PolicyRequestApproval}, NeedsApproval},
		{"unknown + empty policy denies (fail closed)", Request{KnownUser: false, Policy: ""}, Deny},
		{"unknown + unrecognized policy denies", Request{KnownUser: false, Policy: "nonsense"}, Deny},

		// Known sender, scope=owner: only the owner role passes.
		{"known owner-scope owner allows", Request{KnownUser: true, Role: RoleOwner, Scope: ScopeOwner}, Allow},
		{"known owner-scope admin denies", Request{KnownUser: true, Role: RoleAdmin, Scope: ScopeOwner}, Deny},
		{"known owner-scope member denies", Request{KnownUser: true, Role: RoleMember, Scope: ScopeOwner}, Deny},

		// Known sender, scope=known/all: any known user passes regardless of role.
		{"known known-scope member allows", Request{KnownUser: true, Role: RoleMember, Scope: ScopeKnown}, Allow},
		{"known all-scope member allows", Request{KnownUser: true, Role: RoleMember, Scope: ScopeAll}, Allow},
		{"known all-scope owner allows", Request{KnownUser: true, Role: RoleOwner, Scope: ScopeAll}, Allow},

		// Known sender, unrecognized/empty scope: fail closed.
		{"known empty scope denies (fail closed)", Request{KnownUser: true, Role: RoleOwner, Scope: ""}, Deny},
		{"known unrecognized scope denies", Request{KnownUser: true, Role: RoleOwner, Scope: "nonsense"}, Deny},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Check(c.req); got != c.want {
				t.Fatalf("Check(%+v) = %v, want %v", c.req, got, c.want)
			}
		})
	}
}

// The zero-value Request (unknown user, empty policy) must fail closed.
func TestCheck_ZeroValueDenies(t *testing.T) {
	if got := Check(Request{}); got != Deny {
		t.Fatalf("zero Request = %v, want Deny", got)
	}
}
