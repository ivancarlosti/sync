package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// Bounds of one collected section. They exist so a huge or misbehaving domain
// cannot exhaust the database, the memory of the process or the patience of the
// operator: a section that hits the row cap is stored truncated (and says so),
// never silently cut.
const (
	// reportRowLimit caps the rows one section may hold.
	reportRowLimit = 20000
	// reportPageLimit caps the pages one walk may read, so a nextPageToken
	// chain that never ends cannot loop forever.
	reportPageLimit = 500
	// reportUserPageSize is the largest page users.list accepts.
	reportUserPageSize = 500
	// reportGroupPageSize is the largest page groups.list accepts.
	reportGroupPageSize = 200
	// reportMemberPageSize is the largest page members.list accepts.
	reportMemberPageSize = 200
	// reportLicensePageSize is the largest page the Enterprise License Manager
	// API accepts.
	reportLicensePageSize = 1000
	// reportItemPageSize is the page the role catalogue and the role
	// assignments are read with (the API caps both at 200).
	reportItemPageSize = 200
	// reportDrivePageSize is the page the permissions of a shared drive are read
	// with (the Drive API caps it at 100).
	reportDrivePageSize = 100
)

const (
	// adminBase is the Admin SDK Directory API root. It is served by the
	// admin.googleapis.com API the Workspace connection already enables.
	adminBase = "https://admin.googleapis.com/admin/directory/v1"
	// licensingBase is the Enterprise License Manager API root, which is what
	// answers the licence assignments of a user.
	licensingBase = "https://licensing.googleapis.com/apps/licensing/v1"
	// customerKey is the customer every call of a report is scoped to: the API
	// resolves the alias to the customer of the authorised administrator, which
	// is what makes a report follow the token instead of a stored id.
	customerKey = "my_customer"
)

// reportLicenseProducts is the catalogue of Enterprise License Manager product
// ids the licences section walks.
//
// The API only lists the assignments of a product it is asked about, so the
// catalogue has to be carried here (it is the list GAM carries for the same
// reason). An id Google retired answers 400 and is skipped, and a product with
// no assignment answers an empty page: a stale entry therefore costs one cheap
// call, never the section.
var reportLicenseProducts = []string{
	"Google-Apps",
	"Google-Apps-Unlimited",
	"Google-Apps-For-Business",
	"Google-Apps-For-Postini",
	"Google-Apps-Lite",
	"Google-Apps-For-Nonprofits",
	"Google-Drive-storage",
	"Google-Vault",
	"Google-Vault-Former-Employee",
	"Google-Coordinate",
	"Google-CustomerIQ",
}

var (
	// pageTokenPattern is the only shape of page token this provider follows.
	// The token is opaque and joins the query of the next request, so it is
	// narrowed here (and escaped by withParam) before it reaches an URL: an
	// answer can never steer the following request to another endpoint or
	// another host. The length bound is generous next to the ~200 characters a
	// real token carries.
	pageTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_./+:=-]{1,1000}$`)
	// groupKeyPattern is the shape of the group key the membership section
	// interpolates into a request path: a group's primary address (which is what
	// a Workspace group is keyed by) or its id. It excludes every character that
	// could leave the segment — a separator, a query or a fragment delimiter,
	// whitespace and the percent-encoded form of any of them — while keeping the
	// `@`, `+`, `'` and `%` an address itself may carry.
	groupKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._%+@'-]{1,255}$`)
	// licenseProductPattern is the shape of a product id of the catalogue
	// above; it is narrowed for the same reason as the group key.
	licenseProductPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// Compile time guard: the registry hands this provider to the report service as
// an interface, so a renamed method must fail the build.
var _ providers.ReportSource = (*Provider)(nil)

// ReportSections lists the sections this provider can collect.
//
// Every one of them is served by a scope Sync already requests (see
// permissions.go), so a directory report needs no consent beyond the connection
// itself: the users, groups, domains, organisational units, roles and licences
// come from the same grant the capability badges of the accounts screen are
// derived from, and the shared drives come from the Drive scope the transfers
// already use.
//
// The collections a GAM style export can reach that Sync cannot are the ones
// that need a per-mailbox identity (delegates), a YouTube content owner or a
// Chrome management API: they are absent because the scope is absent, so the API
// offers "reconnect the account" instead of failing a section nobody can fix
// (see docs/reports.md).
func (p *Provider) ReportSections() []providers.ReportSection {
	return providers.ReportSectionsFor(models.ProviderGoogle)
}

// reportCustomer is GET /customers/{customerKey}: the domain record itself,
// which is the `organization` section.
type reportCustomer struct {
	ID             string `json:"id"`
	CustomerID     string `json:"customerId"`
	CustomerDomain string `json:"customerDomain"`
	AlternateEmail string `json:"alternateEmail"`
	PhoneNumber    string `json:"phoneNumber"`
	Language       string `json:"language"`
	PostalAddress  struct {
		ContactName      string `json:"contactName"`
		OrganizationName string `json:"organizationName"`
		AddressLine1     string `json:"addressLine1"`
		AddressLine2     string `json:"addressLine2"`
		AddressLine3     string `json:"addressLine3"`
		Locality         string `json:"locality"`
		Region           string `json:"region"`
		PostalCode       string `json:"postalCode"`
		CountryCode      string `json:"countryCode"`
	} `json:"postalAddress"`
	PrimaryAdmin struct {
		PrimaryEmail string `json:"primaryEmail"`
	} `json:"primaryAdmin"`
}

// reportUser is one user record, trimmed to the columns of the users section.
type reportUser struct {
	ID                        string     `json:"id"`
	PrimaryEmail              string     `json:"primaryEmail"`
	Name                      reportName `json:"name"`
	Aliases                   []string   `json:"aliases"`
	IsAdmin                   bool       `json:"isAdmin"`
	IsDelegatedAdmin          bool       `json:"isDelegatedAdmin"`
	IsEnforcedIn2Sv           bool       `json:"isEnforcedIn2Sv"`
	IsEnrolledIn2Sv           bool       `json:"isEnrolledIn2Sv"`
	IsMailboxSetup            bool       `json:"isMailboxSetup"`
	Suspended                 bool       `json:"suspended"`
	Archived                  bool       `json:"archived"`
	ChangePasswordAtNextLogin bool       `json:"changePasswordAtNextLogin"`
	LastLoginTime             string     `json:"lastLoginTime"`
	CreationTime              string     `json:"creationTime"`
	OrgUnitPath               string     `json:"orgUnitPath"`
	CustomerID                string     `json:"customerId"`
}

// reportName is the `name` object of a user record.
type reportName struct {
	FullName   string `json:"fullName"`
	GivenName  string `json:"givenName"`
	FamilyName string `json:"familyName"`
}

// reportGroup is one group record, trimmed to the columns of the groups
// section. AdminCreated stays optional because a group created by a directory
// sync answers neither flag.
type reportGroup struct {
	ID                 string   `json:"id"`
	Email              string   `json:"email"`
	Name               string   `json:"name"`
	Aliases            []string `json:"aliases"`
	Description        string   `json:"description"`
	AdminCreated       *bool    `json:"adminCreated"`
	DirectMembersCount string   `json:"directMembersCount"`
}

// reportDrivePermission is one permission of a shared drive, trimmed to the
// columns of the shared drive members section. The Drive API returns a permission
// per grantee, which is why a person who reaches a drive through a group appears
// as the group (`type: group`) and not as themselves: the drive records the grant
// it was given, and expanding a group is the directory's job (see the group
// members section).
type reportDrivePermission struct {
	ID           string `json:"id"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
	Role         string `json:"role"`
	Type         string `json:"type"`
	Domain       string `json:"domain"`
}

// reportMember is one membership of a group.
type reportMember struct {
	ID     string `json:"id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// reportDomain is one domain of the customer, with the aliases that point at it.
type reportDomain struct {
	DomainName    string `json:"domainName"`
	IsPrimary     bool   `json:"isPrimary"`
	Verified      bool   `json:"verified"`
	DomainAliases []struct {
		DomainAliasName string `json:"domainAliasName"`
		Verified        bool   `json:"verified"`
	} `json:"domainAliases"`
}

// reportOrgUnit is one node of the organisational unit tree.
type reportOrgUnit struct {
	OrgUnitID         string `json:"orgUnitId"`
	OrgUnitPath       string `json:"orgUnitPath"`
	Name              string `json:"name"`
	ParentOrgUnitPath string `json:"parentOrgUnitPath"`
	Description       string `json:"description"`
	BlockInheritance  bool   `json:"blockInheritance"`
}

// reportRole is one administrative role of the catalogue.
type reportRole struct {
	RoleID           string `json:"roleId"`
	RoleName         string `json:"roleName"`
	RoleDescription  string `json:"roleDescription"`
	IsSystemRole     bool   `json:"isSystemRole"`
	IsSuperAdminRole bool   `json:"isSuperAdminRole"`
}

// reportRoleAssignment is one administrator holding one role over one scope.
type reportRoleAssignment struct {
	RoleAssignmentID string `json:"roleAssignmentId"`
	RoleID           string `json:"roleId"`
	AssignedTo       string `json:"assignedTo"`
	AssigneeType     string `json:"assigneeType"`
	ScopeType        string `json:"scopeType"`
	OrgUnitID        string `json:"orgUnitId"`
}

// reportLicense is one licence of one user, as the License Manager API lists it.
type reportLicense struct {
	ProductID string `json:"productId"`
	SkuID     string `json:"skuId"`
	SkuName   string `json:"skuName"`
	UserID    string `json:"userId"`
}

// CollectReport builds the requested sections of one directory report.
//
// The sections are read in the canonical order, and the collections that feed
// more than one of them (the users page, the groups page and the licence
// catalogue) are read once and reused: suspended_users is the users page
// filtered, group_members walks the groups the groups section already listed,
// and the `licenses` column of the users section comes from the catalogue the
// licenses section reads. The per-user loop of the GAM flow — one `users.get`
// per account to ask for its licences — therefore collapses into one walk per
// product.
//
// A section that cannot be collected is returned with its Error set instead of
// failing the pass: a grant made before a feature existed refuses that endpoint
// with a 403, and the users of the same domain are still worth reporting.
func (p *Provider) CollectReport(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, sections []providers.ReportSection, progress func(providers.ReportProgress)) ([]providers.ReportTable, error) {
	if tokens == nil || strings.TrimSpace(tokens.AccessToken) == "" {
		return nil, fmt.Errorf("google: a directory report needs an access token")
	}
	if progress == nil {
		progress = func(providers.ReportProgress) {}
	}
	data := &reportData{
		provider: p,
		client:   p.client(tokens),
		tokens:   tokens,
		ctx:      ctx,
		progress: progress,
	}
	tables := make([]providers.ReportTable, 0, len(sections))
	for _, section := range sections {
		if err := ctx.Err(); err != nil {
			return tables, err
		}
		tables = append(tables, data.table(section))
	}
	return tables, nil
}

// reportData is one collection pass: the client, the report progress callback and
// the cache of the collections more than one section reads.
type reportData struct {
	provider *Provider
	client   *http.Client
	tokens   *providers.Tokens
	ctx      context.Context
	progress func(providers.ReportProgress)

	customer       reportCustomer
	customerLoaded bool
	customerErr    error

	users       []reportUser
	usersLoaded bool
	usersCut    bool
	usersErr    error

	groups       []reportGroup
	groupsLoaded bool
	groupsCut    bool
	groupsErr    error

	domains       []reportDomain
	domainsLoaded bool
	domainsCut    bool
	domainsErr    error

	// licenses is the assignment of every product, indexed by the lower-cased
	// address of the user that holds it.
	licenses       map[string][]reportLicense
	licensesLoaded bool
	licensesErr    error
	licenseRows    int
	licenseCut     bool

	// drives is the Drive listing (My Drive plus the shared drives), read once:
	// the shared drives section lists it and the membership section opens it.
	drives       []providers.Drive
	drivesLoaded bool
	drivesErr    error
}

// table collects one section and normalises the empty shapes the API and the UI
// expect (an empty array, never a null).
func (d *reportData) table(section providers.ReportSection) providers.ReportTable {
	var out providers.ReportTable
	switch section {
	case providers.ReportSectionOrganization:
		out = d.organizationTable()
	case providers.ReportSectionUsers:
		out = d.usersTable(false)
	case providers.ReportSectionSuspendedUsers:
		out = d.usersTable(true)
	case providers.ReportSectionGroups:
		out = d.groupsTable()
	case providers.ReportSectionGroupMembers:
		out = d.groupMembersTable()
	case providers.ReportSectionDomains:
		out = d.domainsTable()
	case providers.ReportSectionOrgUnits:
		out = d.orgUnitsTable()
	case providers.ReportSectionRoles:
		out = d.rolesTable()
	case providers.ReportSectionRoleAssignments:
		out = d.roleAssignmentsTable()
	case providers.ReportSectionLicenses:
		out = d.licensesTable()
	case providers.ReportSectionSharedDrives:
		out = d.sharedDrivesTable()
	case providers.ReportSectionSharedDriveMembers:
		out = d.sharedDriveMembersTable()
	default:
		out = providers.ReportTable{Error: fmt.Sprintf("google: report section %q is not collected for this provider", string(section))}
	}
	out.Section = section
	if out.Columns == nil {
		out.Columns = []providers.ReportColumn{}
	}
	if out.Rows == nil {
		out.Rows = []providers.ReportRow{}
	}
	d.progress(providers.ReportProgress{Section: section, Rows: len(out.Rows)})
	return out
}

// walkRequest is one paged read of the Directory API: the collection to read, the
// URL of its first page and the row budget of the section it feeds.
type walkRequest struct {
	section    providers.ReportSection
	target     string
	collection string
	// base is how many rows the section already holds, so the progress the run
	// row mirrors counts the whole section instead of this walk.
	base int
	// budget caps the rows this walk may add (the rest of the section budget).
	budget int
}

// walk reads one collection page by page, handing every element to visit, which
// answers whether the element was accepted into the section. It answers how many
// rows were accepted, whether the row or page cap cut the listing, and the first
// error that stopped it.
//
// Google paginates with an opaque `nextPageToken` query parameter instead of
// Graph's next link, so the token is validated (pageTokenPattern) and escaped
// (withParam) before it joins the URL: a page can never steer the following
// request to another endpoint or another host.
func (d *reportData) walk(request walkRequest, visit func(json.RawMessage) bool) (int, bool, error) {
	rows := 0
	token := ""
	for page := 0; ; page++ {
		if err := d.ctx.Err(); err != nil {
			return rows, false, err
		}
		if page >= reportPageLimit {
			return rows, true, nil
		}
		target := request.target
		if token != "" {
			if !pageTokenPattern.MatchString(token) {
				return rows, false, invalidIdentifierError("page token", token)
			}
			target = withParam(target, "pageToken", token)
		}
		var answer map[string]json.RawMessage
		if err := d.provider.doJSON(d.ctx, d.client, http.MethodGet, target, nil, &answer); err != nil {
			return rows, false, err
		}
		elements, err := elementsOf(answer, request.collection)
		if err != nil {
			return rows, false, fmt.Errorf("google: cannot read the %s of a report page: %w", request.collection, err)
		}
		for _, element := range elements {
			if rows >= request.budget || request.base+rows >= reportRowLimit {
				return rows, true, nil
			}
			if visit(element) {
				rows++
			}
		}
		d.progress(providers.ReportProgress{Section: request.section, Rows: request.base + rows})
		token, err = pageTokenOf(answer)
		if err != nil {
			return rows, false, fmt.Errorf("google: cannot read the page token of a report page: %w", err)
		}
		if token == "" {
			return rows, false, nil
		}
	}
}

// elementsOf answers the elements of the collection one page carries, which the
// API names after the resource (`users`, `groups`, `members`, `items`, `domains`,
// `organizationUnits`) instead of a shared key.
func elementsOf(page map[string]json.RawMessage, collection string) ([]json.RawMessage, error) {
	raw, ok := page[collection]
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		return nil, err
	}
	return elements, nil
}

// pageTokenOf answers the token of the next page, or an empty string when the
// collection was read whole.
func pageTokenOf(page map[string]json.RawMessage) (string, error) {
	raw, ok := page["nextPageToken"]
	if !ok || len(raw) == 0 {
		return "", nil
	}
	var token string
	if err := json.Unmarshal(raw, &token); err != nil {
		return "", err
	}
	return strings.TrimSpace(token), nil
}

// usersTarget builds the first page of the users collection. `projection=full`
// is what makes the last login, the two-step flags and the alias list come back
// (the default basic projection omits them), and `orderBy=email` keeps the
// pagination stable between two pages of the same walk.
func usersTarget() string {
	target := adminBase + "/users"
	target = withParam(target, "customer", customerKey)
	target = withParam(target, "maxResults", strconv.Itoa(reportUserPageSize))
	target = withParam(target, "projection", "full")
	target = withParam(target, "orderBy", "email")
	return target
}

// groupsTarget builds the first page of the groups collection.
func groupsTarget() string {
	target := adminBase + "/groups"
	target = withParam(target, "customer", customerKey)
	target = withParam(target, "maxResults", strconv.Itoa(reportGroupPageSize))
	return target
}

// membersTarget builds the first page of the members collection of one group.
// The key is interpolated into the path, so it is narrowed here, in the function
// that builds the URL (see groupKeyPattern).
func membersTarget(groupKey string) (string, error) {
	if !groupKeyPattern.MatchString(groupKey) {
		return "", invalidIdentifierError("group key", groupKey)
	}
	target := adminBase + "/groups/" + url.PathEscape(groupKey) + "/members"
	target = withParam(target, "maxResults", strconv.Itoa(reportMemberPageSize))
	return target, nil
}

// customerTarget builds the customer record URL and the collections that hang off
// it (domains, organisational units, roles, role assignments), which the API
// addresses under `customer/{customerKey}` rather than under the plural
// collection.
func customerTarget(resource string, values url.Values) string {
	target := adminBase + "/customer/" + customerKey + "/" + resource
	for key, list := range values {
		for _, value := range list {
			target = withParam(target, key, value)
		}
	}
	return target
}

// loadCustomer reads the domain record once. The organization section is the
// only reader, but the call is cached so a section requested twice in one pass
// (a manual run overlapping a scheduled one) is paid for once.
func (d *reportData) loadCustomer() {
	if d.customerLoaded {
		return
	}
	d.customerLoaded = true
	var customer reportCustomer
	if err := d.provider.doJSON(d.ctx, d.client, http.MethodGet, adminBase+"/customers/"+customerKey, nil, &customer); err != nil {
		d.customerErr = err
		return
	}
	d.customer = customer
}

// loadUsers reads the users collection once; the suspended users section filters
// it instead of listing the directory a second time with `query=isSuspended=true`.
func (d *reportData) loadUsers() {
	if d.usersLoaded {
		return
	}
	d.usersLoaded = true
	users := make([]reportUser, 0, reportUserPageSize)
	_, cut, err := d.walk(walkRequest{
		section:    providers.ReportSectionUsers,
		target:     usersTarget(),
		collection: "users",
		budget:     reportRowLimit,
	}, func(raw json.RawMessage) bool {
		var user reportUser
		if err := json.Unmarshal(raw, &user); err != nil {
			return false
		}
		users = append(users, user)
		return true
	})
	d.users, d.usersCut, d.usersErr = users, cut, err
}

// loadGroups reads the groups collection once; the membership section walks the
// groups it lists, and the groups section renders them.
func (d *reportData) loadGroups() {
	if d.groupsLoaded {
		return
	}
	d.groupsLoaded = true
	groups := make([]reportGroup, 0, reportGroupPageSize)
	_, cut, err := d.walk(walkRequest{
		section:    providers.ReportSectionGroups,
		target:     groupsTarget(),
		collection: "groups",
		budget:     reportRowLimit,
	}, func(raw json.RawMessage) bool {
		var group reportGroup
		if err := json.Unmarshal(raw, &group); err != nil {
			return false
		}
		groups = append(groups, group)
		return true
	})
	d.groups, d.groupsCut, d.groupsErr = groups, cut, err
}

// loadDomains reads the domain list once: the domains section renders it and the
// organization section falls back to it when the customer record is refused.
func (d *reportData) loadDomains() {
	if d.domainsLoaded {
		return
	}
	d.domainsLoaded = true
	domains := make([]reportDomain, 0, 8)
	_, cut, err := d.walk(walkRequest{
		section:    providers.ReportSectionDomains,
		target:     customerTarget("domains", nil),
		collection: "domains",
		budget:     reportRowLimit,
	}, func(raw json.RawMessage) bool {
		var domain reportDomain
		if err := json.Unmarshal(raw, &domain); err != nil {
			return false
		}
		domains = append(domains, domain)
		return true
	})
	d.domains, d.domainsCut, d.domainsErr = domains, cut, err
}

// loadLicenses reads the licence catalogue once: the users section turns it into
// the `licenses` column and the licenses section lists it.
//
// One product is one walk. A product the API does not know (an id Google
// retired) answers 400 and is skipped, because a stale catalogue entry must never
// cost a section; only when *every* product refuses does the section fail, which
// is the shape of a grant that predates the apps.licensing scope.
func (d *reportData) loadLicenses() {
	if d.licensesLoaded {
		return
	}
	d.licensesLoaded = true
	d.licenses = make(map[string][]reportLicense, 256)
	answered := 0
	var firstErr error
	for _, product := range reportLicenseProducts {
		if err := d.ctx.Err(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		if d.licenseRows >= reportRowLimit {
			d.licenseCut = true
			break
		}
		count, err := d.scanLicenses(product)
		if err != nil {
			if apiStatus(err) == http.StatusBadRequest {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		answered++
		d.licenseRows += count
	}
	if answered == 0 {
		d.licensesErr = firstErr
	}
}

// scanLicenses reads one product of the catalogue, adding every assignment to the
// per-user index.
func (d *reportData) scanLicenses(product string) (int, error) {
	if !licenseProductPattern.MatchString(product) {
		return 0, invalidIdentifierError("license product", product)
	}
	target := licensingBase + "/product/" + url.PathEscape(product) + "/users"
	target = withParam(target, "customerId", customerKey)
	target = withParam(target, "maxResults", strconv.Itoa(reportLicensePageSize))
	count, _, err := d.walk(walkRequest{
		section:    providers.ReportSectionLicenses,
		target:     target,
		collection: "items",
		base:       d.licenseRows,
		budget:     reportRowLimit - d.licenseRows,
	}, func(raw json.RawMessage) bool {
		var license reportLicense
		if err := json.Unmarshal(raw, &license); err != nil {
			return false
		}
		address := strings.ToLower(textCell(license.UserID))
		if address == "" {
			return false
		}
		d.licenses[address] = append(d.licenses[address], license)
		return true
	})
	return count, err
}

// apiStatus answers the HTTP status of an API failure, or 0 when err did not come
// from the API.
func apiStatus(err error) int {
	var apiErr *apiError
	if !asAPIError(err, &apiErr) {
		return 0
	}
	return apiErr.Status
}

// licensesOf answers the assignments of one user and the names to display for
// them, preferring the readable sku name over the opaque id.
func (d *reportData) licensesOf(user reportUser) (int, []string) {
	assignments := d.licenses[strings.ToLower(textCell(user.PrimaryEmail))]
	names := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		name := textCell(assignment.SkuName)
		if name == "" {
			name = textCell(assignment.SkuID)
		}
		if name == "" {
			name = textCell(assignment.ProductID)
		}
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return len(assignments), names
}

// organizationTable builds the organization section: the domain record itself,
// which is a single document rather than a collection.
//
// The record comes from GET /customers/{customerKey} — the one call of a report
// that needs a scope of its own (admin.directory.customer.readonly, see
// permissions.go). A grant made before that scope was requested answers 403, and
// the section is then derived from the domain list instead of failing: the
// operator still gets the primary domain of the tenant, and the `source` column
// says where the row came from.
func (d *reportData) organizationTable() providers.ReportTable {
	table := providers.ReportTable{Columns: organizationColumns()}
	d.loadCustomer()
	d.loadDomains()
	if d.customerErr == nil {
		table.Rows = append(table.Rows, d.customerRow())
		return table
	}
	if d.domainsErr != nil {
		table.Error = d.customerErr.Error()
		return table
	}
	domains, primary := d.domainSummary()
	table.Rows = append(table.Rows, providers.ReportRow{
		"", primary, "", "", "", "", "",
		strings.Join(domains, "; "),
		"domains",
	})
	return table
}

// customerRow renders the domain record of GET /customers/{customerKey}.
func (d *reportData) customerRow() providers.ReportRow {
	customer := d.customer
	domains, primary := d.domainSummary()
	if primary == "" {
		primary = textCell(customer.CustomerDomain)
	}
	id := textCell(customer.CustomerID)
	if id == "" {
		id = textCell(customer.ID)
	}
	return providers.ReportRow{
		id,
		primary,
		textCell(customer.PrimaryAdmin.PrimaryEmail),
		textCell(customer.AlternateEmail),
		textCell(customer.PhoneNumber),
		textCell(customer.Language),
		postalAddress(customer),
		strings.Join(domains, "; "),
		"customer",
	}
}

// domainSummary answers the domains of the customer and its primary one, which
// both the organization and the domains sections display.
func (d *reportData) domainSummary() ([]string, string) {
	names := make([]string, 0, len(d.domains))
	primary := ""
	for _, domain := range d.domains {
		name := textCell(domain.DomainName)
		if name == "" {
			continue
		}
		names = append(names, name)
		if domain.IsPrimary && primary == "" {
			primary = name
		}
	}
	if primary == "" && len(names) > 0 {
		primary = names[0]
	}
	return names, primary
}

// postalAddress flattens the address of the customer record, dropping the empty
// lines so a CSV cell never starts with a stray separator.
func postalAddress(customer reportCustomer) string {
	address := customer.PostalAddress
	parts := []string{
		address.OrganizationName,
		address.ContactName,
		address.AddressLine1,
		address.AddressLine2,
		address.AddressLine3,
		strings.Join(compact(address.PostalCode, address.Locality, address.Region), " "),
		address.CountryCode,
	}
	lines := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := textCell(part); value != "" {
			lines = append(lines, value)
		}
	}
	return strings.Join(lines, ", ")
}

// compact trims a list of address parts and drops the empty ones.
func compact(values ...string) []string {
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := textCell(value); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return kept
}

// usersTable builds the users section, or the suspended users section when
// suspendedOnly is true: the second is the first filtered, which is why it costs
// no extra call (the GAM flow re-listed the domain with `isSuspended=true`).
//
// The users section keeps every account, so a report that skips the derived
// section still lists the blocked ones — the `suspended` column tells them apart.
func (d *reportData) usersTable(suspendedOnly bool) providers.ReportTable {
	table := providers.ReportTable{Columns: userColumns()}
	d.loadUsers()
	if d.usersErr != nil {
		table.Error = d.usersErr.Error()
		return table
	}
	// The licence column needs the catalogue. A grant without apps.licensing
	// leaves it empty instead of failing the section: the users of the domain
	// are the point of the section, the licences are one column of it.
	d.loadLicenses()
	for _, user := range d.users {
		if suspendedOnly && !user.Suspended {
			continue
		}
		count, names := d.licensesOf(user)
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(user.PrimaryEmail),
			textCell(user.Name.FullName),
			strings.Join(user.Aliases, "; "),
			boolCell(user.IsAdmin),
			boolCell(user.IsDelegatedAdmin),
			boolCell(user.Suspended),
			boolCell(user.Archived),
			boolCell(user.IsEnforcedIn2Sv),
			boolCell(user.IsEnrolledIn2Sv),
			boolCell(user.IsMailboxSetup),
			boolCell(user.ChangePasswordAtNextLogin),
			textCell(user.LastLoginTime),
			textCell(user.CreationTime),
			textCell(user.OrgUnitPath),
			strings.Join(names, "; "),
			strconv.Itoa(count),
			textCell(user.CustomerID),
			textCell(user.ID),
		})
	}
	table.Truncated = d.usersCut
	return table
}

// groupsTable builds the groups section: every group of the domain, with the size
// the API reports for it.
func (d *reportData) groupsTable() providers.ReportTable {
	table := providers.ReportTable{Columns: groupColumns()}
	d.loadGroups()
	if d.groupsErr != nil {
		table.Error = d.groupsErr.Error()
		return table
	}
	for _, group := range d.groups {
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(group.Email),
			textCell(group.Name),
			textCell(group.Description),
			flagCell(group.AdminCreated),
			textCell(group.DirectMembersCount),
			strings.Join(group.Aliases, "; "),
			textCell(group.ID),
		})
	}
	table.Truncated = d.groupsCut
	return table
}

// groupMembersTable builds the membership of every group the groups section
// lists. It costs one walk per group — the same call the GAM flow issues — and it
// is the section an operator skips when the domain has thousands of groups.
//
// A group the directory refuses to enumerate (it was deleted between the two
// reads, or the grant stopped covering it) is skipped instead of costing the
// membership of the rest; only a pass that fails on *every* group reports an
// error.
func (d *reportData) groupMembersTable() providers.ReportTable {
	table := providers.ReportTable{Columns: memberColumns()}
	d.loadGroups()
	if d.groupsErr != nil {
		table.Error = d.groupsErr.Error()
		return table
	}
	read := 0
	var firstErr error
	for _, group := range d.groups {
		if err := d.ctx.Err(); err != nil {
			table.Error = err.Error()
			return table
		}
		if len(table.Rows) >= reportRowLimit {
			table.Truncated = true
			return table
		}
		key := textCell(group.Email)
		if key == "" {
			key = textCell(group.ID)
		}
		target, err := membersTarget(key)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		base := len(table.Rows)
		_, cut, err := d.walk(walkRequest{
			section:    providers.ReportSectionGroupMembers,
			target:     target,
			collection: "members",
			base:       base,
			budget:     reportRowLimit - base,
		}, func(raw json.RawMessage) bool {
			var member reportMember
			if err := json.Unmarshal(raw, &member); err != nil {
				return false
			}
			table.Rows = append(table.Rows, providers.ReportRow{
				key,
				textCell(member.Email),
				textCell(member.Role),
				textCell(member.Type),
				textCell(member.Status),
			})
			return true
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		read++
		if cut {
			table.Truncated = true
			return table
		}
	}
	if read == 0 && firstErr != nil {
		table.Error = firstErr.Error()
	}
	return table
}

// domainsTable builds the domains section: every domain of the customer with the
// aliases that point at it, which is the list an operator checks before a
// migration or a rebranding.
func (d *reportData) domainsTable() providers.ReportTable {
	table := providers.ReportTable{Columns: domainColumns()}
	d.loadDomains()
	if d.domainsErr != nil {
		table.Error = d.domainsErr.Error()
		return table
	}
	for _, domain := range d.domains {
		aliases := make([]string, 0, len(domain.DomainAliases))
		for _, alias := range domain.DomainAliases {
			if name := textCell(alias.DomainAliasName); name != "" {
				aliases = append(aliases, name)
			}
		}
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(domain.DomainName),
			boolCell(domain.IsPrimary),
			boolCell(domain.Verified),
			strconv.Itoa(len(aliases)),
			strings.Join(aliases, "; "),
		})
	}
	table.Truncated = d.domainsCut
	return table
}

// orgUnitsTable builds the organisational unit tree. The API answers it whole
// (`type=all`), so a domain with a deep tree is bounded by the row cap instead of
// by the pagination.
func (d *reportData) orgUnitsTable() providers.ReportTable {
	table := providers.ReportTable{Columns: orgUnitColumns()}
	_, cut, err := d.walk(walkRequest{
		section:    providers.ReportSectionOrgUnits,
		target:     customerTarget("orgunits", url.Values{"type": {"all"}}),
		collection: "organizationUnits",
		budget:     reportRowLimit,
	}, func(raw json.RawMessage) bool {
		var unit reportOrgUnit
		if err := json.Unmarshal(raw, &unit); err != nil {
			return false
		}
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(unit.OrgUnitPath),
			textCell(unit.Name),
			textCell(unit.ParentOrgUnitPath),
			boolCell(unit.BlockInheritance),
			textCell(unit.Description),
			textCell(unit.OrgUnitID),
		})
		return true
	})
	table.Truncated = cut
	table.Error = errorText(err)
	return table
}

// rolesTable builds the administrative role catalogue.
func (d *reportData) rolesTable() providers.ReportTable {
	table := providers.ReportTable{Columns: roleColumns()}
	_, cut, err := d.walk(walkRequest{
		section:    providers.ReportSectionRoles,
		target:     customerTarget("roles", url.Values{"maxResults": {strconv.Itoa(reportItemPageSize)}}),
		collection: "items",
		budget:     reportRowLimit,
	}, func(raw json.RawMessage) bool {
		var role reportRole
		if err := json.Unmarshal(raw, &role); err != nil {
			return false
		}
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(role.RoleID),
			textCell(role.RoleName),
			textCell(role.RoleDescription),
			boolCell(role.IsSuperAdminRole),
			boolCell(role.IsSystemRole),
		})
		return true
	})
	table.Truncated = cut
	table.Error = errorText(err)
	return table
}

// roleAssignmentsTable builds the "who can do what" table: every administrator,
// the role it holds, and the scope it holds it over.
func (d *reportData) roleAssignmentsTable() providers.ReportTable {
	table := providers.ReportTable{Columns: roleAssignmentColumns()}
	_, cut, err := d.walk(walkRequest{
		section:    providers.ReportSectionRoleAssignments,
		target:     customerTarget("roleassignments", url.Values{"maxResults": {strconv.Itoa(reportItemPageSize)}}),
		collection: "items",
		budget:     reportRowLimit,
	}, func(raw json.RawMessage) bool {
		var assignment reportRoleAssignment
		if err := json.Unmarshal(raw, &assignment); err != nil {
			return false
		}
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(assignment.AssignedTo),
			textCell(assignment.RoleID),
			textCell(assignment.AssigneeType),
			textCell(assignment.ScopeType),
			textCell(assignment.OrgUnitID),
			textCell(assignment.RoleAssignmentID),
		})
		return true
	})
	table.Truncated = cut
	table.Error = errorText(err)
	return table
}

// licensesTable builds the licences section from the assignments the catalogue
// walk collected. The rows are sorted by product and address so two reports of
// the same domain are diffable (the walk itself groups them by product only).
func (d *reportData) licensesTable() providers.ReportTable {
	table := providers.ReportTable{Columns: licenseColumns()}
	d.loadLicenses()
	if d.licensesErr != nil {
		table.Error = d.licensesErr.Error()
		return table
	}
	assignments := make([]reportLicense, 0, d.licenseRows)
	for _, list := range d.licenses {
		assignments = append(assignments, list...)
	}
	sort.Slice(assignments, func(i, j int) bool {
		left, right := assignments[i], assignments[j]
		if left.ProductID != right.ProductID {
			return left.ProductID < right.ProductID
		}
		if left.UserID != right.UserID {
			return left.UserID < right.UserID
		}
		return left.SkuID < right.SkuID
	})
	for _, license := range assignments {
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(license.ProductID),
			textCell(license.SkuID),
			textCell(license.SkuName),
			textCell(license.UserID),
		})
	}
	table.Truncated = d.licenseCut
	return table
}

// sharedDrivesTable builds the shared drives section from the Drive listing the
// folder picker already uses, so a report describes the same drives an operator
// can transfer. `My Drive` is not a shared drive and is left out.
func (d *reportData) sharedDrivesTable() providers.ReportTable {
	table := providers.ReportTable{Columns: sharedDriveColumns()}
	drives, err := d.sharedDriveList()
	if err != nil {
		table.Error = err.Error()
		return table
	}
	for _, drive := range drives {
		if drive.Kind != "shared" {
			continue
		}
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(drive.Name),
			textCell(drive.ID),
			textCell(drive.Owner),
		})
	}
	return table
}

// sharedDriveList reads the Drive listing once. The shared drives section lists it
// and the membership section opens every shared drive of it, so a report that
// carries both sections pays for the listing once.
func (d *reportData) sharedDriveList() ([]providers.Drive, error) {
	if !d.drivesLoaded {
		d.drivesLoaded = true
		drives, err := d.provider.Drives(d.ctx, providers.Credentials{}, d.tokens)
		d.drives, d.drivesErr = drives, err
	}
	return d.drives, d.drivesErr
}

// sharedDriveMembersTable builds the shared drive members section: who can reach
// each shared drive of the domain, with the role the drive gives them.
//
// It reads the permissions of every shared drive (`/drives/{id}/permissions`),
// which is the Drive API's own answer to "who has access to this shared drive" —
// there is no directory-wide equivalent, because a shared drive belongs to a
// Google Group and its members are permissions on the drive, not group members of
// the domain. `My Drive` is skipped, exactly as in the shared drives section: its
// permissions describe one person's files, not a shared location.
func (d *reportData) sharedDriveMembersTable() providers.ReportTable {
	table := providers.ReportTable{Columns: sharedDriveMemberColumns()}
	drives, err := d.sharedDriveList()
	if err != nil {
		table.Error = err.Error()
		return table
	}
	read := 0
	var firstErr error
	for _, drive := range drives {
		if drive.Kind != "shared" {
			continue
		}
		if err := d.ctx.Err(); err != nil {
			table.Error = err.Error()
			return table
		}
		if len(table.Rows) >= reportRowLimit {
			table.Truncated = true
			return table
		}
		if !identifierPattern.MatchString(drive.ID) {
			if firstErr == nil {
				firstErr = invalidIdentifierError("drive id", drive.ID)
			}
			continue
		}
		label := textCell(drive.Name)
		if label == "" {
			label = textCell(drive.ID)
		}
		target := withParam(
			listURL("/drives/"+drive.ID+"/permissions", "nextPageToken,permissions(id,displayName,emailAddress,role,type,domain)"),
			"pageSize", strconv.Itoa(reportDrivePageSize),
		)
		base := len(table.Rows)
		_, cut, err := d.walk(walkRequest{
			section:    providers.ReportSectionSharedDriveMembers,
			target:     target,
			collection: "permissions",
			base:       base,
			budget:     reportRowLimit - base,
		}, func(raw json.RawMessage) bool {
			var permission reportDrivePermission
			if err := json.Unmarshal(raw, &permission); err != nil {
				return false
			}
			table.Rows = append(table.Rows, providers.ReportRow{
				label,
				textCell(permission.DisplayName),
				textCell(permission.EmailAddress),
				textCell(permission.Role),
				textCell(permission.Type),
				textCell(permission.Domain),
			})
			return true
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		read++
		if cut {
			table.Truncated = true
			return table
		}
	}
	if read == 0 && firstErr != nil {
		table.Error = firstErr.Error()
	}
	return table
}

// column is one column of a section: the key the API and the CSV agree on and the
// label both display.
func column(key, title string) providers.ReportColumn {
	return providers.ReportColumn{Key: key, Title: title}
}

// organizationColumns is the header of the organization section.
func organizationColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("customer_id", "Customer id"),
		column("primary_domain", "Primary domain"),
		column("admin_email", "Administrator"),
		column("alternate_email", "Alternate email"),
		column("phone", "Phone"),
		column("language", "Language"),
		column("postal_address", "Postal address"),
		column("domains", "Domains"),
		column("source", "Source"),
	}
}

// userColumns is the header of the users and suspended users sections.
func userColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("primary_email", "Primary email"),
		column("full_name", "Full name"),
		column("aliases", "Aliases"),
		column("is_admin", "Administrator"),
		column("is_delegated_admin", "Delegated administrator"),
		column("suspended", "Suspended"),
		column("archived", "Archived"),
		column("two_step_enforced", "2-step verification enforced"),
		column("two_step_enrolled", "2-step verification enrolled"),
		column("mailbox_setup", "Mailbox set up"),
		column("password_change_required", "Password change required"),
		column("last_login", "Last login"),
		column("created", "Created"),
		column("org_unit", "Org unit"),
		column("licenses", "Licenses"),
		column("license_count", "License count"),
		column("customer_id", "Customer id"),
		column("user_id", "User id"),
	}
}

// groupColumns is the header of the groups section.
func groupColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("email", "Group email"),
		column("name", "Name"),
		column("description", "Description"),
		column("admin_created", "Admin created"),
		column("direct_members", "Direct members"),
		column("aliases", "Aliases"),
		column("group_id", "Group id"),
	}
}

// memberColumns is the header of the group members section: one row per
// membership, not per group.
func memberColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("group", "Group"),
		column("member", "Member"),
		column("role", "Role"),
		column("type", "Type"),
		column("status", "Status"),
	}
}

// domainColumns is the header of the domains section.
func domainColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("domain", "Domain"),
		column("is_primary", "Primary"),
		column("verified", "Verified"),
		column("alias_count", "Alias count"),
		column("aliases", "Domain aliases"),
	}
}

// orgUnitColumns is the header of the organisational units section.
func orgUnitColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("path", "Path"),
		column("name", "Name"),
		column("parent", "Parent"),
		column("block_inheritance", "Block inheritance"),
		column("description", "Description"),
		column("org_unit_id", "Org unit id"),
	}
}

// roleColumns is the header of the roles section.
func roleColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("role_id", "Role id"),
		column("role_name", "Role"),
		column("description", "Description"),
		column("super_admin", "Super admin"),
		column("system", "System role"),
	}
}

// roleAssignmentColumns is the header of the role assignments section.
func roleAssignmentColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("assigned_to", "Assigned to"),
		column("role_id", "Role id"),
		column("assignee_type", "Assignee type"),
		column("scope_type", "Scope"),
		column("org_unit", "Org unit id"),
		column("assignment_id", "Assignment id"),
	}
}

// licenseColumns is the header of the licenses section.
func licenseColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("product", "Product"),
		column("sku_id", "SKU id"),
		column("sku_name", "SKU"),
		column("user", "User"),
	}
}

// sharedDriveColumns is the header of the shared drives section.
func sharedDriveColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("name", "Name"),
		column("drive_id", "Drive id"),
		column("owner", "Owner"),
	}
}

// sharedDriveMemberColumns is the header of the shared drive members section.
func sharedDriveMemberColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("drive", "Shared drive"),
		column("member", "Member"),
		column("email", "Email"),
		column("role", "Role"),
		column("type", "Type"),
		column("domain", "Domain"),
	}
}

// textCell trims a value so a CSV never carries a stray newline or padding.
func textCell(value string) string { return strings.TrimSpace(value) }

// flagCell renders an optional boolean; nil stays empty ("not reported").
func flagCell(value *bool) string {
	if value == nil {
		return ""
	}
	return boolCell(*value)
}

// boolCell renders a boolean as the CSV and the UI display it.
func boolCell(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// errorText renders an error as the message of a failed section, or an empty
// string when the section succeeded.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
