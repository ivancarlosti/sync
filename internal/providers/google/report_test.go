package google

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// reportTransport answers the Directory and License Manager endpoints with canned
// pages while recording the URLs, so the projection, the pagination and the
// per-section isolation of a pass can be asserted without reaching Google.
//
// Bodies are keyed by request path and hold the pages of that path in order: the
// second request to the same path answers the second entry, which is how a walk
// over `nextPageToken` is scripted without spelling out the query string.
type reportTransport struct {
	urls     []string
	bodies   map[string][]string
	statuses map[string]int
	// statusFor, when set, decides the status of a request the statuses map does
	// not name. It is what lets a test refuse a whole API (every product of the
	// licence catalogue) with one line.
	statusFor func(*http.Request) int
	calls     map[string]int
}

// RoundTrip implements http.RoundTripper.
func (t *reportTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.urls = append(t.urls, request.URL.String())
	if t.calls == nil {
		t.calls = map[string]int{}
	}
	path := request.URL.Path
	status := http.StatusOK
	if t.statusFor != nil {
		status = t.statusFor(request)
	}
	if code, ok := t.statuses[path]; ok {
		status = code
	}
	body := ""
	if pages := t.bodies[path]; len(pages) > 0 {
		index := t.calls[path]
		if index >= len(pages) {
			index = len(pages) - 1
		}
		body = pages[index]
	}
	t.calls[path]++
	if body == "" {
		body = "{}"
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

// forbiddenBody is the error document the Directory API answers a grant that does
// not cover the call with. The reason matters: a `forbidden` 403 is a decision
// (the section fails), unlike a `rateLimitExceeded` one (which is retried).
const forbiddenBody = `{"error":{"code":403,"message":"Not Authorized to access this resource/api",` +
	`"errors":[{"reason":"forbidden","message":"Not Authorized to access this resource/api"}]}}`

// reportPaths are the endpoints one full pass of a Workspace domain reads. They
// are spelled out so a test can assert the calls instead of the rows.
const (
	pathCustomer        = "/admin/directory/v1/customers/my_customer"
	pathUsers           = "/admin/directory/v1/users"
	pathGroups          = "/admin/directory/v1/groups"
	pathDomains         = "/admin/directory/v1/customer/my_customer/domains"
	pathOrgUnits        = "/admin/directory/v1/customer/my_customer/orgunits"
	pathRoles           = "/admin/directory/v1/customer/my_customer/roles"
	pathRoleAssignments = "/admin/directory/v1/customer/my_customer/roleassignments"
	pathDrives          = "/drive/v3/drives"
)

// licensePath is the assignment listing of one product of the catalogue.
func licensePath(product string) string {
	return "/apps/licensing/v1/product/" + product + "/users"
}

// membersPath is the membership listing of one group.
func membersPath(key string) string {
	return "/admin/directory/v1/groups/" + key + "/members"
}

// reportBodies is the Workspace domain every collector test reports on: three
// users spread over two pages (one an administrator with a licence, one
// suspended), two groups with three memberships between them, two domains, one
// organisational unit next to the root, two administrative roles, one assignment,
// two licence assignments and one shared drive.
func reportBodies() map[string][]string {
	return map[string][]string{
		pathCustomer: {`{"id":"C01abc","customerId":"C01abc","customerDomain":"contoso.com",` +
			`"alternateEmail":"it@contoso.com","phoneNumber":"+351 210000000","language":"en",` +
			`"postalAddress":{"organizationName":"Contoso","locality":"Lisbon","countryCode":"PT"},` +
			`"primaryAdmin":{"primaryEmail":"admin@contoso.com"}}`},
		pathUsers: {
			`{"users":[` +
				`{"id":"u1","primaryEmail":"Ada@Contoso.com","name":{"fullName":"Ada Lovelace"},` +
				`"aliases":["ada.lovelace@contoso.com"],"isAdmin":true,"isEnforcedIn2Sv":true,` +
				`"isEnrolledIn2Sv":true,"isMailboxSetup":true,"suspended":false,"archived":false,` +
				`"lastLoginTime":"2026-11-01T10:00:00.000Z","creationTime":"2020-01-02T03:04:05.000Z",` +
				`"orgUnitPath":"/Engineering","customerId":"C01abc","changePasswordAtNextLogin":false},` +
				`{"id":"u2","primaryEmail":"grace@contoso.com","name":{"fullName":"Grace Hopper"},` +
				`"suspended":true,"archived":false,"customerId":"C01abc","orgUnitPath":"/"}` +
				`],"nextPageToken":"page-2"}`,
			`{"users":[` +
				`{"id":"u3","primaryEmail":"linus@contoso.com","name":{"fullName":"Linus Torvalds"},` +
				`"suspended":false,"archived":false,"customerId":"C01abc","orgUnitPath":"/"}` +
				`]}`,
		},
		pathGroups: {`{"groups":[` +
			`{"id":"g1","email":"eng@contoso.com","name":"Engineering","description":"Builders",` +
			`"adminCreated":true,"directMembersCount":"2","aliases":["engineering@contoso.com"]},` +
			`{"id":"g2","email":"ops@contoso.com","name":"Operations","directMembersCount":"1"}` +
			`]}`},
		membersPath("eng@contoso.com"): {`{"members":[` +
			`{"id":"m1","email":"ada@contoso.com","role":"OWNER","type":"USER","status":"ACTIVE"},` +
			`{"id":"m2","email":"linus@contoso.com","role":"MEMBER","type":"USER","status":"ACTIVE"}` +
			`]}`},
		membersPath("ops@contoso.com"): {`{"members":[` +
			`{"id":"m3","email":"grace@contoso.com","role":"MEMBER","type":"USER","status":"SUSPENDED"}` +
			`]}`},
		pathDomains: {`{"domains":[` +
			`{"domainName":"contoso.com","isPrimary":true,"verified":true,` +
			`"domainAliases":[{"domainAliasName":"contoso.net","verified":true}]},` +
			`{"domainName":"contoso.io","isPrimary":false,"verified":false}` +
			`]}`},
		pathOrgUnits: {`{"organizationUnits":[` +
			`{"orgUnitId":"id:root","orgUnitPath":"/","name":"Contoso",` +
			`"parentOrgUnitPath":"/","blockInheritance":false},` +
			`{"orgUnitId":"id:eng","orgUnitPath":"/Engineering","name":"Engineering",` +
			`"parentOrgUnitPath":"/","description":"Builders","blockInheritance":true}` +
			`]}`},
		pathRoles: {`{"items":[` +
			`{"roleId":"1","roleName":"_SEED_ADMIN_ROLE","isSuperAdminRole":true,"isSystemRole":true},` +
			`{"roleId":"2","roleName":"_GROUPS_ADMIN_ROLE","roleDescription":"Groups administrator",` +
			`"isSystemRole":true}` +
			`]}`},
		pathRoleAssignments: {`{"items":[` +
			`{"roleAssignmentId":"ra1","roleId":"1","assignedTo":"admin@contoso.com",` +
			`"assigneeType":"USER","scopeType":"CUSTOMER"}` +
			`]}`},
		licensePath("Google-Apps"): {`{"items":[` +
			`{"productId":"Google-Apps","skuId":"Google-Apps-For-Business",` +
			`"skuName":"Google Workspace Business Standard","userId":"ada@contoso.com"}` +
			`]}`},
		licensePath("Google-Vault"): {`{"items":[` +
			`{"productId":"Google-Vault","skuId":"Google-Vault","skuName":"Google Vault",` +
			`"userId":"grace@contoso.com"}` +
			`]}`},
		pathDrives: {`{"drives":[{"id":"0AJxyz","name":"Engineering"}]}`},
	}
}

// newReportProvider builds a provider whose API answers are the given pages.
func newReportProvider(bodies map[string][]string, statuses map[string]int) (*Provider, *reportTransport) {
	transport := &reportTransport{bodies: bodies, statuses: statuses}
	return &Provider{httpClient: &http.Client{Transport: transport}}, transport
}

// reportSections indexes the tables of a pass by section.
func reportSections(t *testing.T, tables []providers.ReportTable) map[providers.ReportSection]providers.ReportTable {
	t.Helper()
	bySection := make(map[providers.ReportSection]providers.ReportTable, len(tables))
	for _, table := range tables {
		bySection[table.Section] = table
	}
	return bySection
}

// reportCell reads one cell of a row by column key.
func reportCell(table providers.ReportTable, row providers.ReportRow, key string) string {
	for index, column := range table.Columns {
		if column.Key == key && index < len(row) {
			return row[index]
		}
	}
	return ""
}

// TestCollectReportBuildsEverySection drives a whole pass over a Workspace
// domain: it pins the row count of every section, the two that are derived
// (suspended users from the users page, the membership of the groups the groups
// section already listed) and the reuse of the collections more than one section
// reads, which is what keeps the call count constant instead of one call per
// user.
func TestCollectReportBuildsEverySection(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)
	sections := providers.ReportSectionsFor(models.ProviderGoogle)

	var progress []providers.ReportProgress
	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"}, sections,
		func(event providers.ReportProgress) { progress = append(progress, event) })
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	if len(tables) != len(sections) {
		t.Fatalf("CollectReport answered %d tables, want %d", len(tables), len(sections))
	}
	if len(progress) == 0 {
		t.Fatal("the pass reported no progress")
	}
	bySection := reportSections(t, tables)

	org := bySection[providers.ReportSectionOrganization]
	if len(org.Rows) != 1 || org.Failed() {
		t.Fatalf("organization section = %+v, want the customer record", org)
	}
	if got := reportCell(org, org.Rows[0], "customer_id"); got != "C01abc" {
		t.Errorf("customer_id = %q, want the tenant id", got)
	}
	if got := reportCell(org, org.Rows[0], "primary_domain"); got != "contoso.com" {
		t.Errorf("primary_domain = %q, want the primary domain", got)
	}
	if got := reportCell(org, org.Rows[0], "source"); got != "customer" {
		t.Errorf("source = %q, want the customer record", got)
	}
	if got := reportCell(org, org.Rows[0], "domains"); got != "contoso.com; contoso.io" {
		t.Errorf("domains = %q, want both domains", got)
	}

	users := bySection[providers.ReportSectionUsers]
	if len(users.Rows) != 3 {
		t.Fatalf("users rows = %d, want 3 (both pages)", len(users.Rows))
	}
	if users.Truncated || users.Failed() {
		t.Fatalf("users section = %+v, want a complete table", users)
	}
	if got := reportCell(users, users.Rows[0], "full_name"); got != "Ada Lovelace" {
		t.Errorf("full_name = %q, want the display name", got)
	}
	if got := reportCell(users, users.Rows[0], "licenses"); got != "Google Workspace Business Standard" {
		t.Errorf("licenses = %q, want the readable sku name", got)
	}
	if got := reportCell(users, users.Rows[0], "license_count"); got != "1" {
		t.Errorf("license_count = %q, want 1", got)
	}
	if got := reportCell(users, users.Rows[1], "suspended"); got != "true" {
		t.Errorf("suspended cell = %q, want true for the blocked account", got)
	}
	if got := reportCell(users, users.Rows[0], "org_unit"); got != "/Engineering" {
		t.Errorf("org_unit = %q, want the organisational unit path", got)
	}

	suspended := bySection[providers.ReportSectionSuspendedUsers]
	if len(suspended.Rows) != 1 {
		t.Fatalf("suspended_users rows = %d, want the one blocked account", len(suspended.Rows))
	}
	if got := reportCell(suspended, suspended.Rows[0], "primary_email"); got != "grace@contoso.com" {
		t.Errorf("suspended row = %q, want the blocked account", got)
	}

	groups := bySection[providers.ReportSectionGroups]
	if len(groups.Rows) != 2 {
		t.Fatalf("groups rows = %d, want 2", len(groups.Rows))
	}
	if got := reportCell(groups, groups.Rows[0], "direct_members"); got != "2" {
		t.Errorf("direct_members = %q, want the count the API reports", got)
	}

	members := bySection[providers.ReportSectionGroupMembers]
	if len(members.Rows) != 3 {
		t.Fatalf("group_members rows = %d, want the 3 memberships of both groups", len(members.Rows))
	}
	if got := reportCell(members, members.Rows[0], "group"); got != "eng@contoso.com" {
		t.Errorf("member group = %q, want the group the row belongs to", got)
	}
	if got := reportCell(members, members.Rows[0], "role"); got != "OWNER" {
		t.Errorf("member role = %q, want OWNER", got)
	}

	domains := bySection[providers.ReportSectionDomains]
	if len(domains.Rows) != 2 {
		t.Fatalf("domains rows = %d, want 2", len(domains.Rows))
	}
	if got := reportCell(domains, domains.Rows[0], "aliases"); got != "contoso.net" {
		t.Errorf("aliases = %q, want the domain alias", got)
	}
	if got := reportCell(domains, domains.Rows[1], "verified"); got != "false" {
		t.Errorf("verified = %q, want false for the unverified domain", got)
	}

	units := bySection[providers.ReportSectionOrgUnits]
	if len(units.Rows) != 2 {
		t.Fatalf("org_units rows = %d, want 2", len(units.Rows))
	}
	if got := reportCell(units, units.Rows[1], "block_inheritance"); got != "true" {
		t.Errorf("block_inheritance = %q, want true", got)
	}

	roles := bySection[providers.ReportSectionRoles]
	if len(roles.Rows) != 2 {
		t.Fatalf("roles rows = %d, want 2", len(roles.Rows))
	}
	if got := reportCell(roles, roles.Rows[0], "super_admin"); got != "true" {
		t.Errorf("super_admin = %q, want true for the seed admin role", got)
	}

	assignments := bySection[providers.ReportSectionRoleAssignments]
	if len(assignments.Rows) != 1 {
		t.Fatalf("role_assignments rows = %d, want 1", len(assignments.Rows))
	}
	if got := reportCell(assignments, assignments.Rows[0], "assigned_to"); got != "admin@contoso.com" {
		t.Errorf("assigned_to = %q, want the administrator", got)
	}

	licenses := bySection[providers.ReportSectionLicenses]
	if len(licenses.Rows) != 2 {
		t.Fatalf("licenses rows = %d, want the 2 assignments", len(licenses.Rows))
	}
	if got := reportCell(licenses, licenses.Rows[0], "product"); got != "Google-Apps" {
		t.Errorf("first licence product = %q, want the rows sorted by product", got)
	}
	if got := reportCell(licenses, licenses.Rows[1], "user"); got != "grace@contoso.com" {
		t.Errorf("second licence user = %q, want the vault assignment", got)
	}

	drives := bySection[providers.ReportSectionSharedDrives]
	if len(drives.Rows) != 1 || drives.Failed() {
		t.Fatalf("shared_drives section = %+v, want the one shared drive", drives)
	}
	if got := reportCell(drives, drives.Rows[0], "name"); got != "Engineering" {
		t.Errorf("shared drive name = %q, want the drive of the domain", got)
	}

	// The users collection is read once (two pages) with the projection the
	// columns need, and the licence catalogue is walked once per product: the
	// per-user loop of the GAM flow must not come back.
	if count := requestsTo(transport.urls, pathUsers); count != 2 {
		t.Errorf("users calls = %d, want 2 (one per page)", count)
	}
	if !containsURL(transport.urls, "projection=full") {
		t.Error("the users listing did not ask for the full projection")
	}
	if count := requestsTo(transport.urls, licensePath("Google-Apps")); count != 1 {
		t.Errorf("licence calls of Google-Apps = %d, want 1", count)
	}
	if !containsURL(transport.urls, "customerId=my_customer") {
		t.Error("the licence listing was not scoped to the customer")
	}
}

// requestsTo counts the requests a pass sent to one path.
func requestsTo(urls []string, path string) int {
	count := 0
	for _, raw := range urls {
		if strings.Contains(raw, path) {
			count++
		}
	}
	return count
}

// containsURL reports whether one of the requests carried a fragment.
func containsURL(urls []string, fragment string) bool {
	for _, raw := range urls {
		if strings.Contains(raw, fragment) {
			return true
		}
	}
	return false
}

// TestCollectReportDerivesTheOrganizationWhenTheCustomerIsRefused pins the
// fallback: a grant made before admin.directory.customer.readonly was requested
// answers 403 on the customer record, and the section is then derived from the
// domain list instead of failing.
func TestCollectReportDerivesTheOrganizationWhenTheCustomerIsRefused(t *testing.T) {
	bodies := reportBodies()
	bodies[pathCustomer] = []string{forbiddenBody}
	provider, _ := newReportProvider(bodies, map[string]int{pathCustomer: http.StatusForbidden})

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionOrganization}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	org := tables[0]
	if org.Failed() {
		t.Fatalf("organization section = %q, want a derived row", org.Error)
	}
	if len(org.Rows) != 1 {
		t.Fatalf("organization rows = %d, want the derived row", len(org.Rows))
	}
	if got := reportCell(org, org.Rows[0], "source"); got != "domains" {
		t.Errorf("source = %q, want the derived marker", got)
	}
	if got := reportCell(org, org.Rows[0], "primary_domain"); got != "contoso.com" {
		t.Errorf("primary_domain = %q, want the primary domain of the listing", got)
	}
	if got := reportCell(org, org.Rows[0], "customer_id"); got != "" {
		t.Errorf("customer_id = %q, want an empty cell (the record was refused)", got)
	}
}

// TestCollectReportIsolatesAFailedSection pins the per-section contract twice
// over: a refused endpoint fails only its own table, and the sections that share
// the collections it reads still produce theirs.
func TestCollectReportIsolatesAFailedSection(t *testing.T) {
	bodies := reportBodies()
	bodies[pathGroups] = []string{forbiddenBody}
	provider, _ := newReportProvider(bodies, map[string]int{pathGroups: http.StatusForbidden})

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{
			providers.ReportSectionGroups,
			providers.ReportSectionGroupMembers,
			providers.ReportSectionUsers,
		}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	bySection := reportSections(t, tables)
	groups := bySection[providers.ReportSectionGroups]
	if !groups.Failed() {
		t.Fatalf("groups section = %+v, want the refusal reported", groups)
	}
	if !strings.Contains(groups.Error, "403") {
		t.Errorf("groups error = %q, want the API status", groups.Error)
	}
	members := bySection[providers.ReportSectionGroupMembers]
	if !members.Failed() {
		t.Fatalf("group_members section = %+v, want the same refusal", members)
	}
	users := bySection[providers.ReportSectionUsers]
	if users.Failed() || len(users.Rows) != 3 {
		t.Fatalf("users section = %+v, want the 3 users of a healthy collection", users)
	}
}

// TestWalkRefusesAPageTokenThatLeavesTheAPI pins the pagination guard: the token
// an answer carries joins the next request, so one that is not a token (here one
// carrying a query delimiter) stops the walk instead of steering it.
func TestWalkRefusesAPageTokenThatLeavesTheAPI(t *testing.T) {
	bodies := reportBodies()
	bodies[pathUsers] = []string{
		`{"users":[{"id":"u1","primaryEmail":"ada@contoso.com"}],"nextPageToken":"page-2?alt=json"}`,
	}
	provider, transport := newReportProvider(bodies, nil)

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionUsers}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	users := tables[0]
	if !users.Failed() {
		t.Fatalf("users section = %+v, want the page token refused", users)
	}
	if !strings.Contains(users.Error, "page token") {
		t.Errorf("users error = %q, want the refused page token", users.Error)
	}
	if count := requestsTo(transport.urls, pathUsers); count != 1 {
		t.Errorf("users calls = %d, want 1 (the walk must not follow the token)", count)
	}
}

// TestLicensesSkipAProductTheAPIRetired pins the tolerance of the catalogue
// walk: a product id Google no longer knows answers 400, and the assignments of
// the products that do exist must still be reported.
func TestLicensesSkipAProductTheAPIRetired(t *testing.T) {
	provider, _ := newReportProvider(reportBodies(),
		map[string]int{licensePath("Google-Apps"): http.StatusBadRequest})

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionLicenses}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	licenses := tables[0]
	if licenses.Failed() {
		t.Fatalf("licenses section = %q, want the retired product skipped", licenses.Error)
	}
	if len(licenses.Rows) != 1 {
		t.Fatalf("licenses rows = %d, want the vault assignment", len(licenses.Rows))
	}
	if got := reportCell(licenses, licenses.Rows[0], "product"); got != "Google-Vault" {
		t.Errorf("licences product = %q, want the product that answered", got)
	}
}

// TestCollectReportKeepsTheUsersWhenLicencesAreRefused pins the trade of the
// licence column: a grant without apps.licensing leaves it empty and fails only
// the licences section, because the users of the domain are the point of the
// users table.
func TestCollectReportKeepsTheUsersWhenLicencesAreRefused(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)
	transport.statusFor = func(request *http.Request) int {
		if strings.HasPrefix(request.URL.Path, "/apps/licensing/") {
			return http.StatusForbidden
		}
		return http.StatusOK
	}

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionUsers, providers.ReportSectionLicenses}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	bySection := reportSections(t, tables)
	users := bySection[providers.ReportSectionUsers]
	if users.Failed() || len(users.Rows) != 3 {
		t.Fatalf("users section = %+v, want the users without the licence column", users)
	}
	if got := reportCell(users, users.Rows[0], "licenses"); got != "" {
		t.Errorf("licenses cell = %q, want it empty when the catalogue is refused", got)
	}
	licenses := bySection[providers.ReportSectionLicenses]
	if !licenses.Failed() {
		t.Fatalf("licenses section = %+v, want the refusal reported", licenses)
	}
}

// TestReportSectionsAreTheGoogleDirectory pins the sections a Workspace domain
// offers: the Microsoft-only ones are absent, and every advertised section is
// part of the shared vocabulary.
func TestReportSectionsAreTheGoogleDirectory(t *testing.T) {
	sections := New().ReportSections()
	want := providers.ReportSectionsFor(models.ProviderGoogle)
	if fmt.Sprint(sections) != fmt.Sprint(want) {
		t.Fatalf("ReportSections = %v, want %v", sections, want)
	}
	for _, section := range sections {
		if !section.Valid() {
			t.Errorf("ReportSections returned the unknown section %q", section)
		}
	}
	for _, section := range []providers.ReportSection{
		providers.ReportSectionSites,
		providers.ReportSectionGuests,
		providers.ReportSectionDistributionLists,
	} {
		if containsSection(sections, section) {
			t.Errorf("ReportSections offered the Microsoft-only section %q", section)
		}
	}
}

// TestCollectReportNeedsAToken pins the guard against an account whose token was
// never loaded: the pass is refused instead of reading the directory anonymously.
func TestCollectReportNeedsAToken(t *testing.T) {
	provider, _ := newReportProvider(nil, nil)
	if _, err := provider.CollectReport(context.Background(), providers.Credentials{}, nil,
		providers.ReportSectionsFor(models.ProviderGoogle), nil); err == nil {
		t.Fatal("CollectReport without a token = nil, want an error")
	}
}

// containsSection reports whether a section list holds a section.
func containsSection(sections []providers.ReportSection, want providers.ReportSection) bool {
	for _, section := range sections {
		if section == want {
			return true
		}
	}
	return false
}
