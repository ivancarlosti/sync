package google

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
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
	// batch, when set, answers a POST to the batch endpoint: it receives the
	// sub-request lines of the document and answers one part per sub-request,
	// which is what lets a test script the per-user licence lookup without a
	// multipart body spelled out inline.
	batch func(subRequests []string) []batchAnswer
	calls map[string]int
}

// RoundTrip implements http.RoundTripper.
func (t *reportTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.urls = append(t.urls, request.URL.String())
	if t.calls == nil {
		t.calls = map[string]int{}
	}
	path := request.URL.Path
	if t.batch != nil && path == pathLicensingBatch && request.Method == http.MethodPost {
		return t.batchAnswer(request)
	}
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

// batchAnswer renders the parts of a batch document: one `application/http` part
// per scripted answer, carrying the raw HTTP response Google would put there.
func (t *reportTransport) batchAnswer(request *http.Request) (*http.Response, error) {
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for index, answer := range t.batch(subRequestLines(string(payload))) {
		part, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Type": {"application/http"},
			"Content-ID":   {fmt.Sprintf("<response-item%d>", index+1)},
		})
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(part, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\n\r\n%s",
			answer.Status, http.StatusText(answer.Status), answer.Body)
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	body := buffer.String()
	header := http.Header{}
	header.Set("Content-Type", "multipart/mixed; boundary="+writer.Boundary())
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        fmt.Sprintf("%d %s", http.StatusOK, http.StatusText(http.StatusOK)),
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

// subRequestLines answers the sub-requests of a batch document: the request line of
// every `application/http` part, which is what a scripted answer is keyed by.
func subRequestLines(payload string) []string {
	lines := make([]string, 0, len(reportLicenseSkus))
	for _, line := range strings.Split(payload, "\r\n") {
		if strings.HasPrefix(line, "GET ") {
			lines = append(lines, line)
		}
	}
	return lines
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
	// pathDrivePermissions is the membership listing of one shared drive. Drive
	// exposes it through the *file* resource: a shared drive id is a file id, and
	// the `drives` resource has no `permissions` method (hence not
	// /drive/v3/drives/{id}/permissions, which answers 404).
	pathDrivePermissions = "/drive/v3/files/0AJxyz/permissions"
	// pathAnalyticsAccounts is the Analytics Admin listing of the accounts the
	// connection can reach; pathAnalyticsProperties is the listing of the
	// properties under one of them, which the API scopes with a `parent:` filter
	// instead of a path segment.
	pathAnalyticsAccounts   = "/v1beta/accounts"
	pathAnalyticsProperties = "/v1beta/properties"
	// pathChannels is the YouTube Data listing of the channels of the grant.
	pathChannels = "/youtube/v3/channels"
	// pathUserInfo is the OpenID userinfo endpoint, which is what names the
	// account behind the token for the analytics and youtube sections.
	pathUserInfo = "/oauth2/v3/userinfo"
	// pathLicensingBatch is the batch endpoint every per-user licence lookup is
	// sent to.
	pathLicensingBatch = "/batch"
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
// two licence assignments, one shared drive, the two permissions of it, one
// Analytics account with one property, one YouTube channel and the identity behind
// the token (which the analytics and youtube sections name).
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
		pathDrivePermissions: {`{"permissions":[` +
			`{"id":"p1","type":"user","role":"organizer","emailAddress":"ada@contoso.com",` +
			`"displayName":"Ada Lovelace","domain":"contoso.com"},` +
			`{"id":"p2","type":"group","role":"fileOrganizer","emailAddress":"eng@contoso.com",` +
			`"displayName":"Engineering","domain":"contoso.com"}` +
			`]}`},
		pathAnalyticsAccounts: {`{"accounts":[` +
			`{"name":"accounts/123","displayName":"Contoso Analytics",` +
			`"createTime":"2019-03-01T10:00:00Z"}` +
			`]}`},
		pathAnalyticsProperties: {`{"properties":[` +
			`{"name":"properties/456","parent":"accounts/123","displayName":"contoso.com",` +
			`"propertyType":"PROPERTY_TYPE_ORDINARY","industryCategory":"TECHNOLOGY",` +
			`"timeZone":"Europe/Lisbon","currencyCode":"EUR",` +
			`"createTime":"2020-06-01T09:30:00Z","serviceLevel":"STANDARD"}` +
			`]}`},
		pathChannels: {`{"items":[` +
			`{"id":"UC01contoso","snippet":{"title":"Contoso",` +
			`"customUrl":"@contoso","country":"PT","publishedAt":"2018-01-05T12:00:00Z"},` +
			`"statistics":{"viewCount":"1200345","subscriberCount":"2400",` +
			`"hiddenSubscriberCount":false,"videoCount":"17"}}` +
			`]}`},
		pathUserInfo: {`{"sub":"1","email":"admin@contoso.com","hd":"contoso.com"}`},
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

	// The membership section walks the permissions of every shared drive, which
	// is the section that used to fail on a `drives/{id}/permissions` endpoint
	// the Drive API does not have: a pass that carries the section must produce
	// its rows, not an error.
	driveMembers := bySection[providers.ReportSectionSharedDriveMembers]
	if driveMembers.Failed() {
		t.Fatalf("shared_drive_members section failed: %s", driveMembers.Error)
	}
	if len(driveMembers.Rows) != 2 {
		t.Fatalf("shared_drive_members rows = %d, want the 2 permissions of the drive", len(driveMembers.Rows))
	}
	if got := reportCell(driveMembers, driveMembers.Rows[0], "drive"); got != "Engineering" {
		t.Errorf("member drive = %q, want the drive the permission belongs to", got)
	}
	if got := reportCell(driveMembers, driveMembers.Rows[0], "role"); got != "organizer" {
		t.Errorf("member role = %q, want organizer", got)
	}

	analytics := bySection[providers.ReportSectionAnalytics]
	if analytics.Failed() {
		t.Fatalf("analytics section failed: %s", analytics.Error)
	}
	if len(analytics.Rows) != 1 {
		t.Fatalf("analytics rows = %d, want the property of the one account", len(analytics.Rows))
	}
	if got := reportCell(analytics, analytics.Rows[0], "account"); got != "Contoso Analytics" {
		t.Errorf("analytics account = %q, want the account that holds the property", got)
	}
	if got := reportCell(analytics, analytics.Rows[0], "created"); got != "2020-06-01T09:30:00Z" {
		t.Errorf("analytics created = %q, want the creation date of the property", got)
	}

	channels := bySection[providers.ReportSectionYouTube]
	if channels.Failed() {
		t.Fatalf("youtube section failed: %s", channels.Error)
	}
	if len(channels.Rows) != 1 {
		t.Fatalf("youtube rows = %d, want the channel of the connection", len(channels.Rows))
	}
	if got := reportCell(channels, channels.Rows[0], "handle"); got != "@contoso" {
		t.Errorf("channel handle = %q, want the custom url", got)
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
	// The catalogue is scoped to the id of the domain, which is the value the
	// License Manager API documents (an Admin console id), not to the alias.
	if !containsURL(transport.urls, "customerId=C01abc") {
		t.Error("the licence listing was not scoped to the customer record")
	}
}

// TestCollectReportReadsThePermissionsOfASharedDrive pins the endpoint of the
// membership section: Drive exposes the permissions of a shared drive through
// files/{fileId}/permissions — a shared drive id *is* a file id — and not through
// /drives/{driveId}/permissions, which the API does not implement and answers 404,
// which is why this section used to come back unusable on every domain.
// useDomainAdminAccess is what lets the administrator behind the account read the
// members of a drive it does not belong to; supportsAllDrives (from listURL) is
// what makes the call look at a shared drive instead of My Drive.
func TestCollectReportReadsThePermissionsOfASharedDrive(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)
	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionSharedDriveMembers}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	table := tables[0]
	if table.Failed() {
		t.Fatalf("shared_drive_members section = %q, want the permissions of the drive", table.Error)
	}
	if len(table.Rows) != 2 {
		t.Fatalf("shared_drive_members rows = %d, want the 2 permissions of the drive", len(table.Rows))
	}
	if count := requestsTo(transport.urls, pathDrivePermissions); count != 1 {
		t.Fatalf("permission calls = %d, want 1 on %s: %v", count, pathDrivePermissions, transport.urls)
	}
	if count := requestsTo(transport.urls, "/drive/v3/drives/0AJxyz/permissions"); count != 0 {
		t.Errorf("the pass asked %d times for the non-existent drives/{id}/permissions listing", count)
	}
	for _, fragment := range []string{"useDomainAdminAccess=true", "supportsAllDrives=true"} {
		if !containsURL(transport.urls, fragment) {
			t.Errorf("the permission listing did not carry %s", fragment)
		}
	}
	if got := reportCell(table, table.Rows[1], "member"); got != "Engineering" {
		t.Errorf("member = %q, want the group behind the second permission", got)
	}
	if got := reportCell(table, table.Rows[1], "type"); got != "group" {
		t.Errorf("type = %q, want group", got)
	}
}

// TestAnalyticsSectionReadsThePropertiesOfEveryAccount pins the shape of the
// analytics section: one page of accounts, then one property listing per account
// scoped with `filter=parent:accounts/{id}`, both read with the largest page the
// v1beta Admin API accepts. The account name travels in the query, so it is the
// value the filter escapes, and the row keeps both halves of the answer: the
// account it belongs to and the settings Analytics records for the property.
func TestAnalyticsSectionReadsThePropertiesOfEveryAccount(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)
	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionAnalytics}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	table := tables[0]
	if table.Failed() {
		t.Fatalf("analytics section = %q, want the properties of the account", table.Error)
	}
	if table.Truncated {
		t.Error("analytics section is truncated, want the single property of the domain")
	}
	if len(table.Rows) != 1 {
		t.Fatalf("analytics rows = %d, want 1", len(table.Rows))
	}
	row := table.Rows[0]
	for key, want := range map[string]string{
		"account_email": "admin@contoso.com",
		"account":       "Contoso Analytics",
		"property":      "properties/456",
		"property_name": "contoso.com",
		"property_type": "PROPERTY_TYPE_ORDINARY",
		"industry":      "TECHNOLOGY",
		"time_zone":     "Europe/Lisbon",
		"currency":      "EUR",
		"created":       "2020-06-01T09:30:00Z",
	} {
		if got := reportCell(table, row, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// The connection, not the property, is the subject of the section: the email
	// of the account the report reads as must be the first column, so the reader
	// knows whose Analytics the rows describe.
	if table.Columns[0].Key != "account_email" {
		t.Errorf("first analytics column = %q, want the account email", table.Columns[0].Key)
	}
	if table.Columns[0].Title != "Account" {
		t.Errorf("first analytics title = %q, want Account", table.Columns[0].Title)
	}
	if count := requestsTo(transport.urls, pathAnalyticsAccounts); count != 1 {
		t.Errorf("account calls = %d, want 1", count)
	}
	if count := requestsTo(transport.urls, pathAnalyticsProperties); count != 1 {
		t.Errorf("property calls = %d, want 1 per account", count)
	}
	for _, fragment := range []string{
		"pageSize=200",
		"filter=parent%3Aaccounts%2F123",
	} {
		if !containsURL(transport.urls, fragment) {
			t.Errorf("the analytics pass did not carry %q: %v", fragment, transport.urls)
		}
	}
}

// TestAnalyticsSectionIsEmptyWhenTheConnectionReachesNoAccount pins the empty
// case: a Workspace domain with analytics.readonly granted and no Analytics
// account answers an empty page, which is a usable section of zero rows and not
// the failure a refused API produces.
func TestAnalyticsSectionIsEmptyWhenTheConnectionReachesNoAccount(t *testing.T) {
	provider, _ := newReportProvider(map[string][]string{pathAnalyticsAccounts: {`{}`}}, nil)
	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionAnalytics}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	table := tables[0]
	if table.Failed() {
		t.Fatalf("analytics section = %q, want an empty section", table.Error)
	}
	if len(table.Rows) != 0 {
		t.Fatalf("analytics rows = %d, want none", len(table.Rows))
	}
	if len(table.Columns) == 0 {
		t.Error("the empty analytics section carries no header")
	}
}

// TestAnalyticsSectionFailsAloneWhenTheAdminAPIIsRefused pins the isolation of the
// two additions: the Analytics Admin API and the YouTube Data API are enabled per
// project, so a domain that runs one of them must keep the other. The refusal is
// labelled with the API that answered it, because "reconnect the account" is only
// the right advice for one of the two.
func TestAnalyticsSectionFailsAloneWhenTheAdminAPIIsRefused(t *testing.T) {
	provider, _ := newReportProvider(reportBodies(),
		map[string]int{pathAnalyticsAccounts: http.StatusForbidden})

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionAnalytics, providers.ReportSectionYouTube}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	analytics := tables[0]
	if !analytics.Failed() {
		t.Fatal("analytics section succeeded, want the refused API reported")
	}
	if !strings.Contains(analytics.Error, "analytics admin api") {
		t.Errorf("analytics error = %q, want the API that refused it named", analytics.Error)
	}
	if youtube := tables[1]; youtube.Failed() || len(youtube.Rows) != 1 {
		t.Errorf("youtube section = %+v, want the channel despite the analytics refusal", youtube)
	}
}

// TestYouTubeSectionListsTheChannelsOfTheConnection pins the youtube section: a
// single `channels.list` with `mine=true` (the Data API has no domain-wide listing
// without a content owner), read with the largest page it accepts and its counters
// rendered as the API quotes them.
func TestYouTubeSectionListsTheChannelsOfTheConnection(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)
	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionYouTube}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	table := tables[0]
	if table.Failed() {
		t.Fatalf("youtube section = %q, want the channel of the connection", table.Error)
	}
	if len(table.Rows) != 1 {
		t.Fatalf("youtube rows = %d, want 1", len(table.Rows))
	}
	row := table.Rows[0]
	for key, want := range map[string]string{
		"account_email": "admin@contoso.com",
		"channel":       "Contoso",
		"channel_id":    "UC01contoso",
		"handle":        "@contoso",
		"country":       "PT",
		"created":       "2018-01-05T12:00:00Z",
		"subscribers":   "2400",
		"videos":        "17",
		"views":         "1200345",
	} {
		if got := reportCell(table, row, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// `mine=true` lists the channels of the grant, so the account behind the grant
	// is what the rows belong to and leads the header.
	if table.Columns[0].Key != "account_email" {
		t.Errorf("first youtube column = %q, want the account email", table.Columns[0].Key)
	}
	if count := requestsTo(transport.urls, pathChannels); count != 1 {
		t.Errorf("channel calls = %d, want 1", count)
	}
	for _, fragment := range []string{"mine=true", "maxResults=50", "part=snippet%2Cstatistics"} {
		if !containsURL(transport.urls, fragment) {
			t.Errorf("the youtube pass did not carry %q: %v", fragment, transport.urls)
		}
	}
}

// TestYouTubeSectionSaysWhenTheSubscriberCountIsHidden pins the one field a
// channel may withhold: the API answers `hiddenSubscriberCount` and leaves the
// counter out, so the cell says so instead of showing an empty count next to a
// visible one.
func TestYouTubeSectionSaysWhenTheSubscriberCountIsHidden(t *testing.T) {
	bodies := map[string][]string{pathChannels: {
		`{"items":[{"id":"UC02contoso","snippet":{"title":"Contoso Labs"},` +
			`"statistics":{"viewCount":"10","hiddenSubscriberCount":true,"videoCount":"2"}}]}`,
	}}
	provider, _ := newReportProvider(bodies, nil)
	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionYouTube}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	table := tables[0]
	if table.Failed() || len(table.Rows) != 1 {
		t.Fatalf("youtube section = %+v, want the one channel", table)
	}
	if got := reportCell(table, table.Rows[0], "subscribers"); got != "hidden" {
		t.Errorf("subscribers = %q, want the hidden marker", got)
	}
	// The pass carries no identity: the account column is empty and the section is
	// still usable, because a refused userinfo read says nothing about the channels.
	if got := reportCell(table, table.Rows[0], "account_email"); got != "" {
		t.Errorf("account_email = %q, want an empty cell when the identity is unreadable", got)
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
	// A section of the Admin SDK must not be reported as a Drive failure: the
	// operator reads this message to know which API (and therefore which scope)
	// refused the call.
	if !strings.Contains(groups.Error, "directory api") {
		t.Errorf("groups error = %q, want the API that refused the call", groups.Error)
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

// TestLicensesFallBackToTheCustomerAlias pins the other half of the scoping: a
// grant that cannot read the customer record (one made before
// admin.directory.customer.readonly was requested) still asks the catalogue, with
// the `my_customer` alias, instead of losing the section.
func TestLicensesFallBackToTheCustomerAlias(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(),
		map[string]int{pathCustomer: http.StatusForbidden})

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionLicenses}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	licenses := tables[0]
	if licenses.Failed() || len(licenses.Rows) != 2 {
		t.Fatalf("licenses section = %+v, want the 2 assignments", licenses)
	}
	if !containsURL(transport.urls, "customerId=my_customer") {
		t.Error("the licence listing did not fall back to the customer alias")
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
	// A 403 is a decision of the domain, not of the product: the section says so
	// once instead of failing ten more times on the rest of the catalogue.
	if !strings.Contains(licenses.Error, "licence catalogue") {
		t.Errorf("licenses error = %q, want the refused catalogue explained", licenses.Error)
	}
	if !strings.Contains(licenses.Error, "403") {
		t.Errorf("licenses error = %q, want the reason the API gave", licenses.Error)
	}
	if count := requestsTo(transport.urls, "/apps/licensing/"); count != 1 {
		t.Errorf("licence calls = %d, want 1 (the walk stops at the refusal)", count)
	}
	// The refusal is only half the answer: the per-user read takes no customer id,
	// so it is tried (once — the batch answers nothing this transport can read, so
	// the walk stops there) and its failure is part of the report.
	if count := requestsTo(transport.urls, pathLicensingBatch); count != 1 {
		t.Errorf("per-user lookups = %d, want 1 (the walk stops at the first failed batch)", count)
	}
	if !strings.Contains(licenses.Error, "per-user") {
		t.Errorf("licenses error = %q, want the per-user fallback reported too", licenses.Error)
	}
}

// TestLicensesProbeEachUserWhenTheDomainRefusesTheCatalogue pins the fallback of a
// domain-wide catalogue the API refuses: the per-user read takes no customer id, so
// the section asks every user of the directory for every SKU of the catalogue in one
// batch and reports what it finds. It is the walk GAM calls `Info_User_Licenses`, and
// the only way a resold tenant — whose reviewer is not an administrator of the domain
// — gets a licence inventory at all.
func TestLicensesProbeEachUserWhenTheDomainRefusesTheCatalogue(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)
	transport.statusFor = func(request *http.Request) int {
		if strings.HasPrefix(request.URL.Path, "/apps/licensing/") {
			return http.StatusForbidden
		}
		return http.StatusOK
	}
	batches, parts := 0, 0
	transport.batch = func(subRequests []string) []batchAnswer {
		batches++
		parts = len(subRequests)
		answers := make([]batchAnswer, len(subRequests))
		for index, line := range subRequests {
			answers[index] = batchAnswer{Status: http.StatusNotFound,
				Body: []byte(`{"error":{"code":404,"message":"Not Found"}}`)}
			if strings.Contains(line, "/sku/1010020028") && strings.Contains(line, "/user/Ada@Contoso.com") {
				answers[index] = batchAnswer{Status: http.StatusOK, Body: []byte(
					`{"productId":"Google-Apps","skuId":"1010020028",` +
						`"skuName":"Google Workspace Business Standard","userId":"Ada@Contoso.com"}`)}
			}
		}
		return answers
	}

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionUsers, providers.ReportSectionLicenses}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	bySection := reportSections(t, tables)
	// The users section keeps the licence column of the fallback: the refusal of the
	// catalogue is not a refusal of the assignments behind it.
	users := bySection[providers.ReportSectionUsers]
	if got := reportCell(users, users.Rows[0], "licenses"); got != "Google Workspace Business Standard" {
		t.Errorf("licenses cell = %q, want the SKU the per-user read answered", got)
	}
	licenses := bySection[providers.ReportSectionLicenses]
	if licenses.Failed() {
		t.Fatalf("licenses section = %q, want the assignments of the per-user read", licenses.Error)
	}
	if licenses.Truncated {
		t.Error("licenses section is truncated, want the single assignment of the directory")
	}
	if len(licenses.Rows) != 1 {
		t.Fatalf("licenses rows = %d, want 1", len(licenses.Rows))
	}
	row := licenses.Rows[0]
	for key, want := range map[string]string{
		"user":     "Ada@Contoso.com",
		"product":  "Google-Apps",
		"sku_id":   "1010020028",
		"sku_name": "Google Workspace Business Standard",
	} {
		if got := reportCell(licenses, row, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// One batch per user, each carrying the whole catalogue: the section replaces a
	// refused listing, so it must not cost one call per user and SKU.
	if batches != 3 {
		t.Errorf("licence batches = %d, want 1 per user of the directory", batches)
	}
	if parts != len(reportLicenseSkus) {
		t.Errorf("sub-requests of a batch = %d, want the %d SKUs of the catalogue",
			parts, len(reportLicenseSkus))
	}
}

// TestLicensesKeepReadingThePartsAfterARefusedOne pins the defect a resold tenant
// reported: the licences of the first user of the directory and nobody else's.
//
// A part that fails is an answer about *one SKU* — a subscription the tenant never
// had, a product Google does not answer `get` for — and the assignments of the same
// user arrive in the other parts of the same answer. The script below refuses the
// first part of the batch, and one part in the middle of the catalogue that falls
// *after* the first assignment of the user the batch is about: reading the answer to
// the end, and asking every other user as well, is the difference between one user's
// licences in the report and the directory's.
func TestLicensesKeepReadingThePartsAfterARefusedOne(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)
	transport.statusFor = func(request *http.Request) int {
		if strings.HasPrefix(request.URL.Path, "/apps/licensing/") {
			return http.StatusForbidden
		}
		return http.StatusOK
	}
	// The domain of the report: a resold tenant where one user holds a paid edition
	// and a free one, and the rest hold the paid edition alone.
	held := map[string]string{
		"/sku/1010020028/user/Ada@Contoso.com": `{"productId":"Google-Apps","skuId":"1010020028",` +
			`"skuName":"Google Workspace Business Standard","userId":"Ada@Contoso.com"}`,
		"/sku/1010010001/user/Ada@Contoso.com": `{"productId":"101001","skuId":"1010010001",` +
			`"skuName":"Cloud Identity Free","userId":"Ada@Contoso.com"}`,
		"/sku/1010020028/user/grace@contoso.com": `{"productId":"Google-Apps","skuId":"1010020028",` +
			`"skuName":"Google Workspace Business Standard","userId":"grace@contoso.com"}`,
		"/sku/1010020028/user/linus@contoso.com": `{"productId":"Google-Apps","skuId":"1010020028",` +
			`"skuName":"Google Workspace Business Standard","userId":"linus@contoso.com"}`,
	}
	batches := 0
	transport.batch = func(subRequests []string) []batchAnswer {
		batches++
		answers := make([]batchAnswer, len(subRequests))
		for index, line := range subRequests {
			answers[index] = batchAnswer{Status: http.StatusNotFound,
				Body: []byte(`{"error":{"code":404,"message":"Not Found"}}`)}
			for key, body := range held {
				if strings.Contains(line, key) {
					answers[index] = batchAnswer{Status: http.StatusOK, Body: []byte(body)}
				}
			}
			if strings.Contains(line, "/sku/1010020020/") || strings.Contains(line, "/sku/1010050001/") {
				// The 403 a subscription the tenant never had is answered with: a
				// failure of that part, in a batch the endpoint accepted.
				answers[index] = batchAnswer{Status: http.StatusForbidden, Body: []byte(
					`{"error":{"code":403,"message":"Subscription not found.",` +
						`"errors":[{"reason":"subscriptionNotFound","message":"Subscription not found."}]}}`)}
			}
		}
		return answers
	}

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionUsers, providers.ReportSectionLicenses}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	bySection := reportSections(t, tables)
	licenses := bySection[providers.ReportSectionLicenses]
	if licenses.Failed() {
		t.Fatalf("licenses section = %q, want the assignments the per-user read answered", licenses.Error)
	}
	// Every user of the directory, including the two a walk that stopped at the first
	// failed part would never have asked about.
	if batches != 3 {
		t.Errorf("licence batches = %d, want 1 per user of the directory", batches)
	}
	got := make([]string, 0, len(licenses.Rows))
	for _, row := range licenses.Rows {
		got = append(got, reportCell(licenses, row, "user")+" "+reportCell(licenses, row, "sku_name"))
	}
	// The catalogue is walked product by product and the rows are sorted by product,
	// address and SKU (licensesTable), so the free edition of the first user comes
	// first — what matters here is that all four assignments are there.
	want := []string{
		"Ada@Contoso.com Cloud Identity Free",
		"Ada@Contoso.com Google Workspace Business Standard",
		"grace@contoso.com Google Workspace Business Standard",
		"linus@contoso.com Google Workspace Business Standard",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("licenses rows = %v, want %v", got, want)
	}
	// The column the report is read for: the assignments of every user, not just the
	// ones that happened to arrive before the first failed part.
	users := bySection[providers.ReportSectionUsers]
	for index, want := range map[int]string{
		0: "Google Workspace Business Standard; Cloud Identity Free",
		1: "Google Workspace Business Standard",
		2: "Google Workspace Business Standard",
	} {
		if cell := reportCell(users, users.Rows[index], "licenses"); cell != want {
			t.Errorf("licenses cell of row %d = %q, want %q", index, cell, want)
		}
	}
}

// TestLicensesKeepWalkingThroughAThrottledBatch pins the one failure the per-user walk
// asks again for: a batch whose parts were all refused because the caller is over a
// quota is not the domain refusing the read, so the next user is still worth asking —
// where the same batch's parts refused for any other reason end the walk.
func TestLicensesKeepWalkingThroughAThrottledBatch(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)
	transport.statusFor = func(request *http.Request) int {
		if strings.HasPrefix(request.URL.Path, "/apps/licensing/") {
			return http.StatusForbidden
		}
		return http.StatusOK
	}
	batches := 0
	transport.batch = func(subRequests []string) []batchAnswer {
		batches++
		answers := make([]batchAnswer, len(subRequests))
		for index := range subRequests {
			answers[index] = batchAnswer{Status: http.StatusForbidden, Body: []byte(
				`{"error":{"code":403,"message":"Quota exceeded for quota metric 'Queries'",` +
					`"errors":[{"reason":"rateLimitExceeded","message":"Rate Limit Exceeded"}]}}`)}
		}
		return answers
	}

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionLicenses}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	licenses := tables[0]
	for _, fragment := range []string{"licence catalogue", "Rate Limit Exceeded"} {
		if !strings.Contains(licenses.Error, fragment) {
			t.Errorf("licenses error = %q, want %q", licenses.Error, fragment)
		}
	}
	if batches != 3 {
		t.Errorf("licence batches = %d, want 1 per user (a quota excess is asked again)", batches)
	}
}

// TestLicensesReportTheDomainWhenThePerUserLookupIsRefusedToo pins the other end of
// the fallback: a domain that refuses the per-user read as well is the answer, so the
// walk stops at the first batch and the section says why both halves are missing
// instead of asking the same 403 once per user.
func TestLicensesReportTheDomainWhenThePerUserLookupIsRefusedToo(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)
	transport.statusFor = func(request *http.Request) int {
		if strings.HasPrefix(request.URL.Path, "/apps/licensing/") {
			return http.StatusForbidden
		}
		return http.StatusOK
	}
	transport.batch = func(subRequests []string) []batchAnswer {
		answers := make([]batchAnswer, len(subRequests))
		for index := range subRequests {
			answers[index] = batchAnswer{Status: http.StatusForbidden, Body: []byte(
				`{"error":{"code":403,"message":"Unauthorized operation for the given domain"}}`)}
		}
		return answers
	}

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionLicenses}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	licenses := tables[0]
	if !licenses.Failed() {
		t.Fatalf("licenses section = %+v, want the refusal reported", licenses)
	}
	for _, fragment := range []string{"licence catalogue", "Unauthorized operation for the given domain"} {
		if !strings.Contains(licenses.Error, fragment) {
			t.Errorf("licenses error = %q, want %q", licenses.Error, fragment)
		}
	}
	if count := requestsTo(transport.urls, pathLicensingBatch); count != 1 {
		t.Errorf("licence batches = %d, want 1 (the walk stops at the refused batch)", count)
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

// TestReportReadsTheRolesWithThePageSizeTheAPIAccepts pins the 400 the roles
// section answered for a while: roles.list caps `maxResults` at 100 and refuses
// 200 with `Invalid value '200'. Values must be within range [1, 100]`, while
// roleassignments.list accepts 200. The two collections used to share one page
// constant, so the catalogue could not be read at all.
func TestReportReadsTheRolesWithThePageSizeTheAPIAccepts(t *testing.T) {
	provider, transport := newReportProvider(reportBodies(), nil)

	tables, err := provider.CollectReport(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "report-token"},
		[]providers.ReportSection{providers.ReportSectionRoles, providers.ReportSectionRoleAssignments}, nil)
	if err != nil {
		t.Fatalf("CollectReport = %v", err)
	}
	bySection := reportSections(t, tables)
	roles := bySection[providers.ReportSectionRoles]
	if roles.Failed() || len(roles.Rows) != 2 {
		t.Fatalf("roles section = %+v, want the two roles of the catalogue", roles)
	}
	if count := requestsTo(transport.urls, pathRoles+"?maxResults=100"); count != 1 {
		t.Errorf("roles calls with maxResults=100 = %d, want 1 (the endpoint caps the page at 100)", count)
	}
	if count := requestsTo(transport.urls, pathRoleAssignments+"?maxResults=200"); count != 1 {
		t.Errorf("role assignment calls with maxResults=200 = %d, want 1", count)
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
