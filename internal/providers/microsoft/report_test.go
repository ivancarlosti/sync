package microsoft

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// newReportProvider builds a provider whose Graph answers are the given bodies.
func newReportProvider(bodies map[string]string, statuses map[string]int) (*Provider, *recordingTransport) {
	transport := &recordingTransport{bodies: bodies, statuses: statuses}
	provider := New()
	provider.httpClient.Transport = transport
	return provider, transport
}

// sectionsOf indexes the tables of a pass by section.
func sectionsOf(t *testing.T, tables []providers.ReportTable) map[providers.ReportSection]providers.ReportTable {
	t.Helper()
	bySection := make(map[providers.ReportSection]providers.ReportTable, len(tables))
	for _, table := range tables {
		bySection[table.Section] = table
	}
	return bySection
}

// cell reads one cell of a row by column key.
func cell(table providers.ReportTable, row providers.ReportRow, key string) string {
	for index, column := range table.Columns {
		if column.Key == key && index < len(row) {
			return row[index]
		}
	}
	return ""
}

// reportBodies is the tenant every collector test reports on: two users spread
// over two pages (one a guest with a licence), three groups (a Microsoft 365
// group, a distribution list and a security group), one licence, one domain, one
// site and the organisation record.
func reportBodies() map[string]string {
	return map[string]string{
		"/v1.0/organization": `{"value":[{"id":"tenant-1","displayName":"Contoso","countryLetterCode":"PT",` +
			`"preferredLanguage":"en","createdDateTime":"2020-01-01T00:00:00Z","onPremisesSyncEnabled":false,` +
			`"verifiedDomains":[{"name":"contoso.com","isDefault":true,"type":"Managed"}],` +
			`"technicalNotificationMails":["it@contoso.com"]}]}`,
		"/v1.0/users": `{"value":[` +
			`{"id":"u1","displayName":"Ada Lovelace","givenName":"Ada","surname":"Lovelace",` +
			`"userPrincipalName":"ada@contoso.com","mail":"ada@contoso.com","mailNickname":"ada",` +
			`"otherMails":["ada@example.com"],"proxyAddresses":["SMTP:ada@contoso.com"],` +
			`"userType":"Member","accountEnabled":true,"jobTitle":"CTO","department":"R&D","officeLocation":"Lisbon",` +
			`"createdDateTime":"2024-01-02T03:04:05Z","assignedLicenses":[{"skuId":"SKU-1"}],` +
			`"signInActivity":{"lastSuccessfulSignInDateTime":"2026-11-08T09:00:00Z"}},` +
			`{"id":"u2","displayName":"Guest User","userPrincipalName":"guest@contoso.com","userType":"Guest",` +
			`"accountEnabled":true,"assignedLicenses":[],"signInActivity":{"lastSuccessfulSignInDateTime":"2026-11-07T09:00:00Z"}}],` +
			`"@odata.nextLink":"https://graph.microsoft.com/v1.0/users?page=2"}`,
		"/v1.0/users?page=2":             `{"value":[{"id":"u3","displayName":"Grace Hopper","userPrincipalName":"grace@contoso.com","userType":"Member","accountEnabled":false,"signInActivity":{"lastSuccessfulSignInDateTime":"2026-11-06T09:00:00Z"}}]}`,
		"/v1.0/users/u1/mailboxSettings": `{"userPurpose":"user"}`,
		"/v1.0/users/u2/mailboxSettings": `{"userPurpose":"user"}`,
		"/v1.0/users/u3/mailboxSettings": `{"userPurpose":"shared"}`,
		"/tenant/oauth2/v2.0/token":      `{"access_token":"app-token"}`,
		"/v1.0/groups": `{"value":[` +
			`{"id":"g1","displayName":"Engineering","mail":"eng@contoso.com","mailEnabled":true,` +
			`"securityEnabled":false,"groupTypes":["Unified"],"visibility":"Private",` +
			`"createdDateTime":"2024-02-03T00:00:00Z"},` +
			`{"id":"g2","displayName":"All staff","mail":"all@contoso.com","mailEnabled":true,` +
			`"securityEnabled":false,"groupTypes":[]},` +
			`{"id":"g3","displayName":"Operators","mailEnabled":false,"securityEnabled":true,"groupTypes":[]}]}`,
		"/v1.0/subscribedSkus": `{"value":[{"skuId":"SKU-1","skuPartNumber":"ENTERPRISEPACK",` +
			`"capabilityStatus":"Enabled","appliesTo":"User","consumedUnits":5,"prepaidUnits":{"enabled":10},` +
			`"servicePlans":[{"servicePlanName":"EXCHANGE_S_ENTERPRISE"},{"servicePlanName":"SHAREPOINTENTERPRISE"}]}]}`,
		"/v1.0/domains": `{"value":[{"id":"contoso.com","isDefault":true,"isInitial":true,"isVerified":true,` +
			`"authenticationType":"Managed","supportedServices":["Email","OfficeCommunicationsOnline"]}]}`,
		"/v1.0/sites": `{"value":[{"id":"site-1","displayName":"Marketing","name":"marketing",` +
			`"webUrl":"https://contoso.sharepoint.com/sites/marketing","createdDateTime":"2023-05-06T00:00:00Z"}]}`,
		"/v1.0/teams": `{"value":[` +
			`{"id":"t1","displayName":"Engineering","description":"Ship it","visibility":"private",` +
			`"isArchived":false,"createdDateTime":"2024-04-05T00:00:00Z"},` +
			`{"id":"t2","displayName":"Field ops","visibility":"public","isArchived":true}]}`,
		"/v1.0/teams/t1/members": `{"value":[` +
			`{"id":"tm1","roles":["owner"],"displayName":"Ada Lovelace","email":"ada@contoso.com","userId":"u1"},` +
			`{"id":"tm2","roles":[],"displayName":"Grace Hopper","email":"grace@contoso.com","userId":"u3"}]}`,
		"/v1.0/teams/t2/members": `{"value":[` +
			`{"id":"tm3","roles":["owner"],"displayName":"Grace Hopper","userId":"u3"}]}`,
		"/v1.0/groups/g1/planner/plans": `{"value":[{"id":"plan-1","title":"Launch","owner":"g1",` +
			`"createdDateTime":"2024-03-04T00:00:00Z"}]}`,
	}
}

// TestCollectReportBuildsEverySection drives a whole pass: it pins the row count
// of every section, the two derived ones (guests and distribution lists) and the
// reuse of the shared collections, which is what keeps the call count constant
// instead of one call per user.
func TestCollectReportBuildsEverySection(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)

	var progress []providers.ReportProgress
	tables, err := provider.CollectReport(context.Background(),
		providers.Credentials{ClientID: "client-id", ClientSecret: "client-secret", TenantID: "tenant"},
		&providers.Tokens{AccessToken: "report-token"}, providers.ReportSections,
		func(event providers.ReportProgress) { progress = append(progress, event) })
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	bySection := sectionsOf(t, tables)

	users := bySection[providers.ReportSectionUsers]
	if len(users.Rows) != 3 {
		t.Fatalf("users rows = %d, want every account classified", len(users.Rows))
	}
	if users.Truncated || users.Failed() {
		t.Fatalf("users section = %+v, want a complete table", users)
	}
	if got := cell(users, users.Rows[0], "licenses"); got != "ENTERPRISEPACK" {
		t.Errorf("licenses cell = %q, want the part number of the sku", got)
	}
	if got := cell(users, users.Rows[0], "license_count"); got != "1" {
		t.Errorf("license_count cell = %q, want 1", got)
	}
	if got := cell(users, users.Rows[0], "last_login"); got != "2026-11-08T09:00:00Z" {
		t.Errorf("last_login cell = %q, want the Graph signInActivity value", got)
	}
	for key, want := range map[string]string{
		"given_name":      "Ada",
		"surname":         "Lovelace",
		"mail_nickname":   "ada",
		"other_mails":     "ada@example.com",
		"proxy_addresses": "SMTP:ada@contoso.com",
	} {
		if got := cell(users, users.Rows[0], key); got != want {
			t.Errorf("%s cell = %q, want %q", key, got, want)
		}
	}
	for index, want := range map[int]string{0: "User", 1: "Guest", 2: "Shared mailbox"} {
		if got := cell(users, users.Rows[index], "account_type"); got != want {
			t.Errorf("account_type of row %d = %q, want %q", index, got, want)
		}
	}
	if got := cell(users, users.Rows[2], "account_enabled"); got != "false" {
		t.Errorf("account_enabled cell = %q, want false", got)
	}

	sharedMailboxes := bySection[providers.ReportSectionSharedMailboxes]
	if len(sharedMailboxes.Rows) != 1 {
		t.Fatalf("shared mailboxes rows = %d, want the one shared mailbox", len(sharedMailboxes.Rows))
	}
	if got := cell(sharedMailboxes, sharedMailboxes.Rows[0], "user_principal_name"); got != "grace@contoso.com" {
		t.Errorf("shared mailbox principal = %q, want grace@contoso.com", got)
	}
	if got := cell(sharedMailboxes, sharedMailboxes.Rows[0], "account_type"); got != "Shared mailbox" {
		t.Errorf("shared mailbox type = %q, want Shared mailbox", got)
	}

	guests := bySection[providers.ReportSectionGuests]
	if len(guests.Rows) != 1 {
		t.Fatalf("guests rows = %d, want the one guest user", len(guests.Rows))
	}
	if got := cell(guests, guests.Rows[0], "display_name"); got != "Guest User" {
		t.Errorf("guest row = %q, want the guest account", got)
	}

	groups := bySection[providers.ReportSectionGroups]
	if len(groups.Rows) != 3 {
		t.Fatalf("groups rows = %d, want 3", len(groups.Rows))
	}
	if got := cell(groups, groups.Rows[0], "kind"); got != "microsoft365" {
		t.Errorf("kind cell = %q, want microsoft365", got)
	}

	lists := bySection[providers.ReportSectionDistributionLists]
	if len(lists.Rows) != 1 {
		t.Fatalf("distribution lists rows = %d, want the one mail-only group", len(lists.Rows))
	}
	if got := cell(lists, lists.Rows[0], "mail"); got != "all@contoso.com" {
		t.Errorf("distribution list row = %q, want the mail-only group", got)
	}

	licenses := bySection[providers.ReportSectionLicenses]
	if len(licenses.Rows) != 1 {
		t.Fatalf("licenses rows = %d, want 1", len(licenses.Rows))
	}
	if got := cell(licenses, licenses.Rows[0], "usage_percent"); got != "50.0" {
		t.Errorf("usage_percent cell = %q, want 50.0", got)
	}
	if got := cell(licenses, licenses.Rows[0], "available_units"); got != "5" {
		t.Errorf("available_units cell = %q, want 5", got)
	}

	domains := bySection[providers.ReportSectionDomains]
	if len(domains.Rows) != 1 || cell(domains, domains.Rows[0], "is_verified") != "true" {
		t.Fatalf("domains section = %+v, want the verified domain", domains)
	}

	sites := bySection[providers.ReportSectionSites]
	if len(sites.Rows) != 1 {
		t.Fatalf("sites rows = %d, want the tenant site", len(sites.Rows))
	}
	if got := cell(sites, sites.Rows[0], "web_url"); got != "https://contoso.sharepoint.com/sites/marketing" {
		t.Errorf("web_url cell = %q", got)
	}

	organization := bySection[providers.ReportSectionOrganization]
	if len(organization.Rows) != 1 {
		t.Fatalf("organization rows = %d, want 1", len(organization.Rows))
	}
	if got := cell(organization, organization.Rows[0], "default_domain"); got != "contoso.com" {
		t.Errorf("default_domain cell = %q, want contoso.com", got)
	}

	teams := bySection[providers.ReportSectionTeams]
	if len(teams.Rows) != 2 {
		t.Fatalf("teams rows = %d, want the two teams", len(teams.Rows))
	}
	if got := cell(teams, teams.Rows[0], "visibility"); got != "private" {
		t.Errorf("visibility cell = %q, want private", got)
	}
	if got := cell(teams, teams.Rows[1], "archived"); got != "true" {
		t.Errorf("archived cell = %q, want true", got)
	}

	// The team membership comes from the team endpoint, not from the group one:
	// the role is the conversation role of the member.
	members := bySection[providers.ReportSectionTeamMembers]
	if len(members.Rows) != 3 {
		t.Fatalf("team members rows = %d, want 3", len(members.Rows))
	}
	if got := cell(members, members.Rows[0], "role"); got != "owner" {
		t.Errorf("role cell = %q, want owner", got)
	}
	if got := cell(members, members.Rows[1], "role"); got != "member" {
		t.Errorf("role cell = %q, want member", got)
	}
	if got := cell(members, members.Rows[0], "team"); got != "Engineering" {
		t.Errorf("team cell = %q, want the team the member belongs to", got)
	}

	plans := bySection[providers.ReportSectionPlannerPlans]
	if len(plans.Rows) != 1 {
		t.Fatalf("planner plans rows = %d, want the plan of the one group that has one", len(plans.Rows))
	}
	if got := cell(plans, plans.Rows[0], "plan"); got != "Launch" {
		t.Errorf("plan cell = %q, want Launch", got)
	}
	if got := cell(plans, plans.Rows[0], "group"); got != "Engineering" {
		t.Errorf("group cell = %q, want the owning group", got)
	}

	if len(progress) == 0 {
		t.Error("CollectReport reported no progress; the polling UI would stay empty")
	}
	// Two users pages, then one call per collection: the guests, distribution
	// lists, team members and licences sections must reuse them instead of
	// re-reading, and the plans walk reuses the groups page. The plans walk only
	// opens the one Microsoft 365 group of the fixture (g2 is a distribution list
	// and g3 a security group, so neither can own a plan).
	if len(transport.urls) != 21 {
		t.Errorf("issued %d requests (%v), want 21 (the users pages, the app token, the three mailbox settings and the shared collections)", len(transport.urls), transport.urls)
	}
}

// TestCollectReportRetriesTeamMembersWithoutTheEmailProperty covers the tenant
// whose Graph resolves the membership `$select` against the base
// `microsoft.graph.conversationMember` type: the projection is refused with a 400
// ("Could not find a property named 'email'..."), the collection is read again as
// the plain listing, and the section keeps its rows and its mail column instead of
// failing. The refusal is remembered, so the second team is read plainly the first
// time instead of paying another doomed call.
func TestCollectReportRetriesTeamMembersWithoutTheEmailProperty(t *testing.T) {
	projected := strings.TrimPrefix(teamMembersTarget("t1", true), "https://graph.microsoft.com")
	if !strings.Contains(projected, "select=") {
		t.Fatalf("teamMembersTarget(true) = %q, want a projection", projected)
	}
	if plain := teamMembersTarget("t1", false); plain != "https://graph.microsoft.com/v1.0/teams/t1/members" {
		t.Fatalf("teamMembersTarget(false) = %q, want the plain listing", plain)
	}
	provider, transport := newReportProvider(
		map[string]string{
			"/v1.0/teams": `{"value":[{"id":"t1","displayName":"Engineering"},{"id":"t2","displayName":"Field ops"}]}`,
			// The refused page is scripted under its own URI as well: the mock keys
			// a scripted answer by the full request URI when it has one, which is
			// what lets the refusal apply to the projected listing and not to the
			// plain one that shares its path.
			projected: `{"value":[]}`,
			"/v1.0/teams/t1/members": `{"value":[{"id":"tm1","roles":["owner"],"displayName":"Ada Lovelace",` +
				`"email":"ada@contoso.com","userId":"u1"}]}`,
			"/v1.0/teams/t2/members": `{"value":[{"id":"tm2","roles":[],"displayName":"Grace Hopper",` +
				`"email":"grace@contoso.com","userId":"u3"}]}`,
		},
		map[string]int{projected: http.StatusBadRequest},
	)

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionTeamMembers}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	if len(tables) != 1 || tables[0].Failed() {
		t.Fatalf("tables = %+v, want the section even when the projection is refused", tables)
	}
	if len(tables[0].Rows) != 2 {
		t.Fatalf("team members rows = %d, want the membership of both teams", len(tables[0].Rows))
	}
	if got := cell(tables[0], tables[0].Rows[0], "mail"); got != "ada@contoso.com" {
		t.Errorf("mail cell = %q, want the address the plain listing carries", got)
	}
	if got := cell(tables[0], tables[0].Rows[0], "role"); got != "owner" {
		t.Errorf("role cell = %q, want owner", got)
	}
	projections := 0
	for _, target := range transport.urls {
		if strings.Contains(target, "select=") && strings.Contains(target, "/members") {
			projections++
		}
	}
	if projections != 1 {
		t.Errorf("issued %v, want the projection attempted once and the rest plain", transport.urls)
	}
	if len(transport.urls) != 4 {
		t.Errorf("issued %d requests (%v), want the teams page and one walk per team", len(transport.urls), transport.urls)
	}
}

// TestCollectReportSkipsThePlansOfGroupsThatRefuse pins the two filters of the
// plans walk: a group that is not a Microsoft 365 group is not asked for plans it
// cannot own, and a group Planner refuses (403: the signed-in user cannot see its
// plans; 404: it has none) costs that group only. Before the fix a single refusal
// emptied the whole section with "You do not have the required permissions".
func TestCollectReportSkipsThePlansOfGroupsThatRefuse(t *testing.T) {
	provider, transport := newReportProvider(
		map[string]string{
			"/v1.0/groups": `{"value":[` +
				`{"id":"g1","displayName":"Engineering","groupTypes":["Unified"]},` +
				`{"id":"g2","displayName":"Field ops","groupTypes":["Unified"]},` +
				`{"id":"g3","displayName":"Operators","mailEnabled":false,"securityEnabled":true,"groupTypes":[]}]}`,
			"/v1.0/groups/g2/planner/plans": `{"value":[{"id":"plan-2","title":"Field plan","owner":"g2"}]}`,
		},
		map[string]int{
			"/v1.0/groups/g1/planner/plans": http.StatusForbidden,
			"/v1.0/groups/g3/planner/plans": http.StatusNotFound,
		},
	)

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionPlannerPlans}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	if len(tables) != 1 || tables[0].Failed() {
		t.Fatalf("tables = %+v, want the plans of the group that answered", tables)
	}
	if len(tables[0].Rows) != 1 {
		t.Fatalf("planner plans rows = %d, want the plan of the group that answered", len(tables[0].Rows))
	}
	if got := cell(tables[0], tables[0].Rows[0], "plan"); got != "Field plan" {
		t.Errorf("plan cell = %q, want Field plan", got)
	}
	for _, target := range transport.urls {
		if strings.Contains(target, "/groups/g3/planner/plans") {
			t.Errorf("asked for the plans of a security group: %v", transport.urls)
		}
	}
}

// TestCollectReportFailsThePlansWhenEveryGroupRefuses pins the other side of the
// same trade: when no Microsoft 365 group answered at all, the refusal is the
// section's error instead of an empty table, because that is what a grant the
// account lacks looks like.
func TestCollectReportFailsThePlansWhenEveryGroupRefuses(t *testing.T) {
	provider, _ := newReportProvider(
		map[string]string{
			"/v1.0/groups": `{"value":[{"id":"g1","displayName":"Engineering","groupTypes":["Unified"]}]}`,
		},
		map[string]int{"/v1.0/groups/g1/planner/plans": http.StatusForbidden},
	)

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionPlannerPlans}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	if len(tables) != 1 || !tables[0].Failed() {
		t.Fatalf("tables = %+v, want a failed section when no group answered", tables)
	}
	if !strings.Contains(tables[0].Error, "403") {
		t.Errorf("section error = %q, want the refusal of every group", tables[0].Error)
	}
}

// TestCollectReportKeepsThePlansWhenAGroupHasNone pins the empty case of the same
// walk: a Microsoft 365 group without a plan answers 200 with an empty collection,
// which is a complete section and not a failure.
func TestCollectReportKeepsThePlansWhenAGroupHasNone(t *testing.T) {
	provider, _ := newReportProvider(
		map[string]string{
			"/v1.0/groups":                  `{"value":[{"id":"g1","displayName":"Engineering","groupTypes":["Unified"]}]}`,
			"/v1.0/groups/g1/planner/plans": `{"value":[]}`,
		}, nil)

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionPlannerPlans}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	if len(tables) != 1 || tables[0].Failed() {
		t.Fatalf("tables = %+v, want a complete empty section", tables)
	}
	if len(tables[0].Rows) != 0 {
		t.Errorf("planner plans rows = %d, want none", len(tables[0].Rows))
	}
}

// TestCollectReportRetriesWithoutTheLicenceProperty covers the tenant that
// refuses `assignedLicenses` in $select: the users section is read again without
// the property and the licence column stays empty instead of losing the section.
func TestCollectReportRetriesWithoutTheLicenceProperty(t *testing.T) {
	plain := strings.TrimPrefix(usersTarget(false, false), "https://graph.microsoft.com")
	transport := &recordingTransport{
		bodies:   map[string]string{plain: `{"value":[{"id":"u1","displayName":"Ada"}]}`},
		statuses: map[string]int{"/v1.0/users": http.StatusBadRequest},
	}
	provider := New()
	provider.httpClient.Transport = transport

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionUsers}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	if len(tables) != 1 || tables[0].Failed() {
		t.Fatalf("tables = %+v, want the users section even when the property is refused", tables)
	}
	if len(tables[0].Rows) != 1 {
		t.Fatalf("users rows = %d, want the row of the plain listing", len(tables[0].Rows))
	}
	if got := cell(tables[0], tables[0].Rows[0], "licenses"); got != "" {
		t.Errorf("licenses cell = %q, want empty without the property", got)
	}
	// Both optional properties are tried first; the tenant refuses every
	// projection that carries one, so the plain listing is the fourth attempt.
	// The test passes no application credentials, so the mailbox purpose read is
	// skipped and adds no request.
	if len(transport.urls) != 4 {
		t.Errorf("issued %v, want each optional projection refused and the plain listing", transport.urls)
	}
}

// TestCollectReportKeepsGoingWhenASectionFails pins the per-section contract: a
// refused endpoint (a personal account has no /sites) fails its own section and
// leaves the rest of the report intact.
func TestCollectReportKeepsGoingWhenASectionFails(t *testing.T) {
	provider, _ := newReportProvider(
		map[string]string{"/v1.0/domains": `{"value":[{"id":"contoso.com","isVerified":true}]}`},
		map[string]int{"/v1.0/sites": http.StatusBadRequest},
	)

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionSites, providers.ReportSectionDomains}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v, want a per-section failure", err)
	}
	bySection := sectionsOf(t, tables)
	if !bySection[providers.ReportSectionSites].Failed() {
		t.Error("the sites section did not report the refusal")
	}
	if len(bySection[providers.ReportSectionDomains].Rows) != 1 {
		t.Error("the domains section did not survive the failure of the sites one")
	}
}

// TestCollectReportRefusesAPageLinkThatLeavesGraph pins the token guard: a
// nextLink is followed with the access token, so one that leaves Graph must stop
// the section instead of being requested.
func TestCollectReportRefusesAPageLinkThatLeavesGraph(t *testing.T) {
	provider, transport := newReportProvider(map[string]string{
		"/v1.0/users": `{"value":[{"id":"u1"}],"@odata.nextLink":"https://evil.example.test/v1.0/users?page=2"}`,
	}, nil)

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionUsers}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	if len(tables) != 1 || !strings.Contains(tables[0].Error, "left Graph") {
		t.Fatalf("tables = %+v, want the section to refuse the page link", tables)
	}
	for _, target := range transport.urls {
		if strings.Contains(target, "evil.example.test") {
			t.Fatalf("followed %q, want the off-Graph link to be refused", target)
		}
	}
}

// TestCollectReportNeedsAToken pins the guard against an account whose token was
// never loaded: the pass is refused instead of reading the directory anonymously.
func TestCollectReportNeedsAToken(t *testing.T) {
	provider, _ := newReportProvider(nil, nil)
	if _, err := provider.CollectReport(context.Background(), providers.Credentials{}, nil, providers.ReportSections, nil); err == nil {
		t.Fatal("CollectReport without a token = nil, want an error")
	}
}

// TestReportSectionsAreTheHeldScopes pins the phase 1 promise: the sections the
// collector advertises are exactly the ones the permission table already pays
// for, so starting a report never asks the operator to reconnect an account.
//
// The vocabulary is provider neutral since the Google collector landed, so the
// comparison is against the Microsoft half of it: a section only a Workspace
// domain has (organisational units, administrative roles, shared drives) must not
// be offered to an Entra ID directory.
func TestReportSectionsAreTheHeldScopes(t *testing.T) {
	provider := New()
	sections := provider.ReportSections()
	want := providers.ReportSectionsFor(models.ProviderMicrosoft)
	if fmt.Sprint(sections) != fmt.Sprint(want) {
		t.Fatalf("ReportSections = %v, want %v", sections, want)
	}
	for _, section := range sections {
		if !section.Valid() {
			t.Errorf("ReportSections returned the unknown section %q", section)
		}
	}
	for _, section := range sections {
		if section == providers.ReportSectionOrgUnits || section == providers.ReportSectionSharedDrives ||
			section == providers.ReportSectionRoles || section == providers.ReportSectionRoleAssignments {
			t.Errorf("ReportSections offered the Google-only section %q", section)
		}
	}
}
