package microsoft

import (
	"context"
	"net/http"
	"strings"
	"testing"

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
			`{"id":"u1","displayName":"Ada Lovelace","userPrincipalName":"ada@contoso.com","mail":"ada@contoso.com",` +
			`"userType":"Member","accountEnabled":true,"jobTitle":"CTO","department":"R&D","officeLocation":"Lisbon",` +
			`"createdDateTime":"2024-01-02T03:04:05Z","assignedLicenses":[{"skuId":"SKU-1"}]},` +
			`{"id":"u2","displayName":"Guest User","userPrincipalName":"guest@contoso.com","userType":"Guest",` +
			`"accountEnabled":true,"assignedLicenses":[]}],` +
			`"@odata.nextLink":"https://graph.microsoft.com/v1.0/users?page=2"}`,
		"/v1.0/users?page=2": `{"value":[{"id":"u3","displayName":"Grace Hopper","userType":"Member","accountEnabled":false}]}`,
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
	}
}

// TestCollectReportBuildsEverySection drives a whole pass: it pins the row count
// of every section, the two derived ones (guests and distribution lists) and the
// reuse of the shared collections, which is what keeps the call count constant
// instead of one call per user.
func TestCollectReportBuildsEverySection(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)

	var progress []providers.ReportProgress
	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"}, providers.ReportSections,
		func(event providers.ReportProgress) { progress = append(progress, event) })
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	bySection := sectionsOf(t, tables)

	users := bySection[providers.ReportSectionUsers]
	if len(users.Rows) != 3 {
		t.Fatalf("users rows = %d, want 3 (both pages)", len(users.Rows))
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
	if got := cell(users, users.Rows[2], "account_enabled"); got != "false" {
		t.Errorf("account_enabled cell = %q, want false", got)
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

	if len(progress) == 0 {
		t.Error("CollectReport reported no progress; the polling UI would stay empty")
	}
	// Two users pages, then one call per collection: the guests, distribution
	// lists and licences sections must reuse them instead of re-reading.
	if len(transport.urls) != 7 {
		t.Errorf("issued %d requests (%v), want 7", len(transport.urls), transport.urls)
	}
}

// TestCollectReportRetriesWithoutTheLicenceProperty covers the tenant that
// refuses `assignedLicenses` in $select: the users section is read again without
// the property and the licence column stays empty instead of losing the section.
func TestCollectReportRetriesWithoutTheLicenceProperty(t *testing.T) {
	plain := strings.TrimPrefix(usersTarget(false), "https://graph.microsoft.com")
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
	if len(transport.urls) != 2 {
		t.Errorf("issued %v, want the refused listing then the plain one", transport.urls)
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
func TestReportSectionsAreTheHeldScopes(t *testing.T) {
	provider := New()
	sections := provider.ReportSections()
	if len(sections) != len(providers.ReportSections) {
		t.Fatalf("ReportSections = %v, want the whole vocabulary", sections)
	}
	for _, section := range sections {
		if !section.Valid() {
			t.Errorf("ReportSections returned the unknown section %q", section)
		}
	}
}
