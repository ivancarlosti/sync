package providers

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ivancarlosti/sync/internal/models"
)

// testPermissions is a small permission table used by the matching tests.
func testPermissions() []Permission {
	return []Permission{
		{Capability: CapabilityFiles, Title: "files", Scope: "files.read"},
		{Capability: CapabilityFiles, Title: "files", Scope: "files.write"},
		{Capability: CapabilityUsers, Title: "users", Scope: "https://graph.microsoft.com/User.Read.All", AdminConsent: true},
	}
}

// TestCapabilitiesOfReportsOnlyCompleteGrants pins the rule the Accounts screen
// depends on: a capability is reported when every scope it declares was granted,
// and never when one is missing.
func TestCapabilitiesOfReportsOnlyCompleteGrants(t *testing.T) {
	table := testPermissions()

	cases := []struct {
		name    string
		granted []string
		want    []Capability
	}{
		{
			name:    "nothing granted",
			granted: nil,
			want:    []Capability{},
		},
		{
			name:    "one of two file scopes",
			granted: []string{"files.read"},
			want:    []Capability{},
		},
		{
			name:    "the whole file pair",
			granted: []string{"files.write", "files.read"},
			want:    []Capability{CapabilityFiles},
		},
		{
			name:    "graph form is recognised",
			granted: []string{"User.Read.All"},
			want:    []Capability{CapabilityUsers},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := CapabilitiesOf(table, testCase.granted)
			if fmt.Sprint(got) != fmt.Sprint(testCase.want) {
				t.Fatalf("CapabilitiesOf() = %v, want %v", got, testCase.want)
			}
			missing := MissingCapabilities(table, testCase.granted)
			if len(got)+len(missing) != len(ProviderCapabilities) {
				t.Fatalf("capabilities %v plus missing %v do not cover the catalogue", got, missing)
			}
			for _, capability := range got {
				for _, absent := range missing {
					if capability == absent {
						t.Fatalf("capability %q is both granted and missing", capability)
					}
				}
			}
		})
	}
}

// TestHasCapabilityNormalisesScopeForms documents the prefix tolerance that lets
// one table describe Microsoft Graph scopes in either of the two forms the
// identity platform returns.
func TestHasCapabilityNormalisesScopeForms(t *testing.T) {
	table := testPermissions()
	granted := []string{"HTTPS://GRAPH.MICROSOFT.COM/user.read.all", " files.read ", "files.write"}
	if !HasCapability(table, granted, CapabilityUsers) {
		t.Fatal("a fully qualified Graph grant was not recognised")
	}
	if !HasCapability(table, granted, CapabilityFiles) {
		t.Fatal("a grant with surrounding whitespace was not recognised")
	}
	if HasCapability(table, granted, CapabilityLicenses) {
		t.Fatal("a capability the table does not declare was reported")
	}
}

// TestScopesOfKeepsRequestOrder pins that the authorization request keeps the
// order of the table, which is what makes the guide readable.
func TestScopesOfKeepsRequestOrder(t *testing.T) {
	got := ScopesOf(testPermissions())
	want := []string{"files.read", "files.write", "https://graph.microsoft.com/User.Read.All"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ScopesOf() = %v, want %v", got, want)
	}
}

// TestInformationalPermissionsDoNotGateACapability pins the rule that keeps a
// Microsoft account from being reported as missing permissions it holds: the
// OpenID Connect scopes are requested for the protocol, so a grant that does not
// carry them still satisfies the capability they are declared under.
func TestInformationalPermissionsDoNotGateACapability(t *testing.T) {
	table := []Permission{
		{Capability: CapabilityFiles, Title: "files", Scope: "files.read"},
		{Capability: CapabilityFiles, Title: "files", Scope: "offline_access", Informational: true},
	}
	if !HasCapability(table, []string{"files.read"}, CapabilityFiles) {
		t.Fatal("a grant without the informational scope was reported as incomplete")
	}
	if len(MissingCapabilities(table, []string{"files.read"})) != len(ProviderCapabilities)-1 {
		t.Fatal("a table that declares one capability was reported as missing another one")
	}
	for _, capability := range MissingCapabilities(table, []string{"files.read"}) {
		if capability == CapabilityFiles {
			t.Fatal("an informational scope was reported as a missing capability")
		}
	}
	if HasCapability(table, []string{"offline_access"}, CapabilityFiles) {
		t.Fatal("the proof-bearing scope of the capability was not required")
	}
}

// TestSplitScopesAcceptsTheEncodedGraphValue pins the tolerance for the
// percent-encoded `scope` value of a Microsoft token response: the example
// Microsoft documents and the space separated form have to yield the same list,
// otherwise an Entra grant is stored as one unreadable scope.
func TestSplitScopesAcceptsTheEncodedGraphValue(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "the documented example",
			raw:  "https%3A%2F%2Fgraph.microsoft.com%2Fmail.read",
			want: []string{"https://graph.microsoft.com/mail.read"},
		},
		{
			name: "a whole encoded grant",
			raw:  "https%3A%2F%2Fgraph.microsoft.com%2Fmail.read%20openid",
			want: []string{"https://graph.microsoft.com/mail.read", "openid"},
		},
		{
			name: "the space separated form",
			raw:  "openid files.read",
			want: []string{"openid", "files.read"},
		},
		{
			name: "an empty grant",
			raw:  "",
			want: nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := SplitScopes(testCase.raw)
			if fmt.Sprint(got) != fmt.Sprint(testCase.want) {
				t.Fatalf("SplitScopes(%q) = %v, want %v", testCase.raw, got, testCase.want)
			}
		})
	}
	if !containsScope(SplitScopes("https%3A%2F%2Fgraph.microsoft.com%2FMail.Read"), "https://graph.microsoft.com/mail.read") {
		t.Fatal("a decoded Graph scope was not matched against the table")
	}
}

// TestIsConsentRequired pins which authorization-server refusals are reported as
// a missing consent (HTTP 412, `code: consent_required`) and which stay generic.
func TestIsConsentRequired(t *testing.T) {
	consent := []string{"access_denied", "consent_required", "admin_consent_required", "invalid_scope", "unauthorized_client", "INTERACTION_REQUIRED"}
	for _, code := range consent {
		err := fmt.Errorf("wrapped: %w", NewAuthError(models.ProviderMicrosoft, code, "denied by policy"))
		if !IsConsentRequired(err) {
			t.Fatalf("%q was not treated as a consent problem", code)
		}
	}
	generic := []error{
		NewAuthError(models.ProviderGoogle, "invalid_grant", "code expired"),
		errors.New("network is unreachable"),
		nil,
	}
	for _, err := range generic {
		if IsConsentRequired(err) {
			t.Fatalf("%v was treated as a consent problem", err)
		}
	}
}

// TestApplicablePermissionsSplitsByAccountKind pins the rule that keeps a
// personal account from showing a permission it can never hold as missing: the
// table is split per permission, because one capability can be declared by both
// grantable and non-grantable scopes.
func TestApplicablePermissionsSplitsByAccountKind(t *testing.T) {
	table := []Permission{
		{Capability: CapabilityFiles, Title: "files", Scope: "files.read"},
		{Capability: CapabilityFiles, Title: "files", Scope: "files.write", Kinds: []AccountKind{AccountKindWork}},
		{Capability: CapabilityUsers, Title: "users", Scope: "users.read", Kinds: []AccountKind{AccountKindWork}},
	}

	applicable, notApplicable := ApplicablePermissions(table, AccountKindPersonal)
	if len(applicable) != 1 || applicable[0].Scope != "files.read" {
		t.Fatalf("applicable = %+v, want the one scope every kind can hold", applicable)
	}
	if len(notApplicable) != 2 || notApplicable[0].Scope != "files.write" {
		t.Fatalf("notApplicable = %+v, want the two work-only scopes", notApplicable)
	}

	// files keeps being judged by its applicable half: it is granted as soon as
	// its only grantable scope is. users — declared by the non applicable half
	// alone — is out of reach instead of missing, and the capabilities this small
	// table never mentions are reported missing exactly as they were before kinds
	// existed.
	report := ReportCapabilities(applicable, notApplicable, []string{"files.read"})
	if fmt.Sprint(report.Granted) != fmt.Sprint([]Capability{CapabilityFiles}) {
		t.Fatalf("granted = %v, want files", report.Granted)
	}
	if fmt.Sprint(report.NotApplicable) != fmt.Sprint([]Capability{CapabilityUsers}) {
		t.Fatalf("notApplicable = %v, want users alone", report.NotApplicable)
	}
	undeclared := []Capability{
		CapabilityGroups, CapabilityMembers, CapabilityDomains,
		CapabilityOrgUnits, CapabilityRoles, CapabilityLicenses,
	}
	if fmt.Sprint(report.Missing) != fmt.Sprint(undeclared) {
		t.Fatalf("missing = %v, want %v", report.Missing, undeclared)
	}

	// Without that scope files is missing — not out of reach.
	report = ReportCapabilities(applicable, notApplicable, nil)
	wantMissing := append([]Capability{CapabilityFiles}, undeclared...)
	if fmt.Sprint(report.Missing) != fmt.Sprint(wantMissing) {
		t.Fatalf("missing = %v, want %v", report.Missing, wantMissing)
	}
}

// TestApplicablePermissionsKeepsUnknownKindsWhole guards the fallback of a
// provider that cannot classify an account: the whole table stays applicable and
// nothing is reported as not applicable, which is exactly how accounts behaved
// before kinds existed.
func TestApplicablePermissionsKeepsUnknownKindsWhole(t *testing.T) {
	table := testPermissions()
	applicable, notApplicable := ApplicablePermissions(table, AccountKindUnknown)
	if fmt.Sprint(applicable) != fmt.Sprint(table) {
		t.Fatalf("applicable = %+v, want the whole table", applicable)
	}
	if len(notApplicable) != 0 {
		t.Fatalf("notApplicable = %+v, want none", notApplicable)
	}
	// With no non applicable half the report is the one the screen always had:
	// granted plus missing, and nothing greyed out.
	for _, granted := range [][]string{nil, {"files.read"}, {"files.read", "files.write"}} {
		report := ReportCapabilities(applicable, notApplicable, granted)
		if len(report.NotApplicable) != 0 {
			t.Fatalf("granted %v: notApplicable = %v, want none", granted, report.NotApplicable)
		}
		if fmt.Sprint(report.Granted) != fmt.Sprint(CapabilitiesOf(table, granted)) ||
			fmt.Sprint(report.Missing) != fmt.Sprint(MissingCapabilities(table, granted)) {
			t.Fatalf("granted %v: report = %+v, want the plain capability lists", granted, report)
		}
	}
	if !AppliesToKind(Permission{Scope: "users.read", Kinds: []AccountKind{AccountKindWork}}, AccountKindUnknown) {
		t.Fatal("a work-only scope does not apply to the unknown kind")
	}
}

// TestAuthErrorNeverLeaksAToken guards the shape of the message shown as
// technical detail: code and description only.
func TestAuthErrorNeverLeaksAToken(t *testing.T) {
	err := NewAuthError(models.ProviderGoogle, "", "")
	if err.Error() != "google: the authorization server refused the request (unknown_error)" {
		t.Fatalf("unexpected message: %q", err.Error())
	}
}
