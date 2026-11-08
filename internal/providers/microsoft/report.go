package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// Bounds of one collected section. They exist so a huge or misbehaving tenant
// cannot exhaust the database, the memory of the process or the patience of the
// operator: a section that hits the row cap is stored truncated (and says so),
// never silently cut.
const (
	// reportRowLimit caps the rows one section may hold.
	reportRowLimit = 20000
	// reportPageLimit caps the pages one section may read, so a nextLink chain
	// that never ends cannot loop forever.
	reportPageLimit = 500
	// reportPageSize is Graph's ceiling for $top on the directory collections.
	reportPageSize = 999
	// reportGroupWalkLimit caps how many groups (or teams) the membership
	// sections open. Those sections pay one or two calls per parent, so a tenant
	// with tens of thousands of groups would otherwise turn one report into a
	// throttling storm; a truncated section says so instead.
	reportGroupWalkLimit = 5000
)

// ReportSections lists the sections this provider can collect. Every one of them
// is served by a scope Sync requests, so a directory report needs no extra
// consent beyond the connection: users, groups, sites, licences and domains come
// from the directory grant, and the membership sections (group members, teams,
// team members, planner plans) from CapabilityMembers, CapabilityTeams and
// CapabilityPlanner.
//
// An account connected before those scopes were added to the table reports them as
// missing capabilities instead of failing a section silently, which is what makes
// "reconnect the account" the honest answer (see docs/reports.md). The sections
// that would need a per-mailbox identity (delegates), a channel or mailbox
// settings remain absent for the same reason they always were: no scope of the
// connection serves them.
//
// The list is the shared vocabulary filtered to what an Entra ID directory has:
// the Google-only sections (organisational units, administrative roles, shared
// drives) are not offered to a Microsoft account (see providers.ReportSectionsFor).
func (p *Provider) ReportSections() []providers.ReportSection {
	return providers.ReportSectionsFor(models.ProviderMicrosoft)
}

// reportUser is one user record, trimmed to the columns of the users section.
type reportUser struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	UserPrincipalName string `json:"userPrincipalName"`
	Mail              string `json:"mail"`
	UserType          string `json:"userType"`
	AccountEnabled    *bool  `json:"accountEnabled"`
	JobTitle          string `json:"jobTitle"`
	Department        string `json:"department"`
	OfficeLocation    string `json:"officeLocation"`
	CreatedDateTime   string `json:"createdDateTime"`
	// AssignedLicenses is empty when the tenant refused the navigation property
	// in $select (see reportUsers).
	AssignedLicenses []struct {
		SkuID string `json:"skuId"`
	} `json:"assignedLicenses"`
}

// reportGroup is one group record, trimmed to the columns of the groups and
// distribution lists sections.
type reportGroup struct {
	ID              string   `json:"id"`
	DisplayName     string   `json:"displayName"`
	Mail            string   `json:"mail"`
	MailEnabled     *bool    `json:"mailEnabled"`
	SecurityEnabled *bool    `json:"securityEnabled"`
	GroupTypes      []string `json:"groupTypes"`
	Visibility      string   `json:"visibility"`
	CreatedDateTime string   `json:"createdDateTime"`
}

// reportSite is one SharePoint site collection.
type reportSite struct {
	ID                   string `json:"id"`
	DisplayName          string `json:"displayName"`
	Name                 string `json:"name"`
	WebURL               string `json:"webUrl"`
	CreatedDateTime      string `json:"createdDateTime"`
	LastModifiedDateTime string `json:"lastModifiedDateTime"`
}

// reportMember is one member or owner of a group, as the directory endpoints
// return it. The `@odata.type` of the record tells whether the subject is a user,
// a group, a device or a service principal, which is what the member type column
// shows: a group nested in a group is a member like any other, but it is not a
// person.
type reportMember struct {
	ODataType         string `json:"@odata.type"`
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	UserPrincipalName string `json:"userPrincipalName"`
	Mail              string `json:"mail"`
	AccountEnabled    *bool  `json:"accountEnabled"`
}

// reportTeam is one Microsoft Team of the tenant, trimmed to the columns of the
// teams section.
type reportTeam struct {
	ID              string `json:"id"`
	DisplayName     string `json:"displayName"`
	Description     string `json:"description"`
	Visibility      string `json:"visibility"`
	IsArchived      *bool  `json:"isArchived"`
	CreatedDateTime string `json:"createdDateTime"`
}

// reportTeamMember is one member of a team: the `aadUserConversationMember` of
// the team membership endpoint, whose Roles list says whether the member is an
// owner of the team (a team member can hold both, and Graph reports both).
type reportTeamMember struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"displayName"`
	Email       string   `json:"email"`
	UserID      string   `json:"userId"`
	Roles       []string `json:"roles"`
}

// reportPlan is one Planner plan of a group.
type reportPlan struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Owner           string `json:"owner"`
	CreatedDateTime string `json:"createdDateTime"`
}

// reportSku is one subscribed licence of the tenant.
type reportSku struct {
	SkuID            string `json:"skuId"`
	SkuPartNumber    string `json:"skuPartNumber"`
	CapabilityStatus string `json:"capabilityStatus"`
	AppliesTo        string `json:"appliesTo"`
	ConsumedUnits    int    `json:"consumedUnits"`
	PrepaidUnits     struct {
		Enabled   int `json:"enabled"`
		Suspended int `json:"suspended"`
		Warning   int `json:"warning"`
	} `json:"prepaidUnits"`
	ServicePlans []struct {
		ServicePlanName string `json:"servicePlanName"`
	} `json:"servicePlans"`
}

// reportDomain is one domain of the tenant.
type reportDomain struct {
	ID                 string   `json:"id"`
	IsDefault          bool     `json:"isDefault"`
	IsInitial          bool     `json:"isInitial"`
	IsVerified         bool     `json:"isVerified"`
	AuthenticationType string   `json:"authenticationType"`
	SupportedServices  []string `json:"supportedServices"`
}

// reportOrganization is the tenant record of GET /organization.
type reportOrganization struct {
	ID                    string `json:"id"`
	DisplayName           string `json:"displayName"`
	CountryLetterCode     string `json:"countryLetterCode"`
	PreferredLanguage     string `json:"preferredLanguage"`
	CreatedDateTime       string `json:"createdDateTime"`
	OnPremisesSyncEnabled *bool  `json:"onPremisesSyncEnabled"`
	VerifiedDomains       []struct {
		Name      string `json:"name"`
		IsDefault bool   `json:"isDefault"`
		Type      string `json:"type"`
	} `json:"verifiedDomains"`
	TechnicalNotificationMails []string `json:"technicalNotificationMails"`
}

// reportPage is one page of a Graph collection.
type reportPage struct {
	Value    []json.RawMessage `json:"value"`
	NextLink string            `json:"@odata.nextLink"`
}

// CollectReport builds the requested sections of one directory report.
//
// The sections are read in the canonical order, and the three collections that
// feed more than one of them (users, groups and the licence catalogue) are read
// once and reused: the guests section is the users page filtered, the
// distribution lists section is the groups page filtered, and the licence names
// of the users section come from the catalogue the licences section already
// reads. That is what turns the per-user `licenseDetails` loop of the n8n flow —
// one call per user in the tenant — into a fixed handful of calls.
//
// A section that cannot be collected is returned with its Error set instead of
// failing the pass: a personal account has no `/sites`, and a tenant may refuse
// `/domains`, but the users of both are still worth reporting.
func (p *Provider) CollectReport(ctx context.Context, _ providers.Credentials, tokens *providers.Tokens, sections []providers.ReportSection, progress func(providers.ReportProgress)) ([]providers.ReportTable, error) {
	if tokens == nil || strings.TrimSpace(tokens.AccessToken) == "" {
		return nil, fmt.Errorf("microsoft graph: a directory report needs an access token")
	}
	if progress == nil {
		progress = func(providers.ReportProgress) {}
	}
	data := &reportData{
		provider: p,
		ctx:      ctx,
		token:    tokens.AccessToken,
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

// reportData is one collection pass: the token, the report progress callback and
// the cache of the collections more than one section reads.
type reportData struct {
	provider *Provider
	ctx      context.Context
	token    string
	progress func(providers.ReportProgress)

	users       []reportUser
	usersLoaded bool
	usersCut    bool
	usersErr    error

	groups       []reportGroup
	groupsLoaded bool
	groupsCut    bool
	groupsErr    error

	teams       []reportTeam
	teamsLoaded bool
	teamsCut    bool
	teamsErr    error
	// teamMembersPlain says the tenant refused the `email` projection of the team
	// membership collection, so the following teams are read plainly first (see
	// walkTeamMembers).
	teamMembersPlain bool

	skus       []reportSku
	skusLoaded bool
	skusErr    error
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
	case providers.ReportSectionGuests:
		out = d.usersTable(true)
	case providers.ReportSectionGroups:
		out = d.groupsTable(false)
	case providers.ReportSectionDistributionLists:
		out = d.groupsTable(true)
	case providers.ReportSectionSites:
		out = d.sitesTable()
	case providers.ReportSectionLicenses:
		out = d.licensesTable()
	case providers.ReportSectionDomains:
		out = d.domainsTable()
	case providers.ReportSectionGroupMembers:
		out = d.groupMembersTable()
	case providers.ReportSectionTeams:
		out = d.teamsTable()
	case providers.ReportSectionTeamMembers:
		out = d.teamMembersTable()
	case providers.ReportSectionPlannerPlans:
		out = d.plannerPlansTable()
	default:
		out = providers.ReportTable{Error: fmt.Sprintf("unknown report section %q", string(section))}
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

// walk reads one Graph collection page by page, handing every element to visit,
// which answers whether the element was accepted into the section. It answers
// how many rows were accepted, whether the row or page cap cut the listing, and
// the first error that stopped it.
//
// The page link guard is the one of the activity feed: a nextLink carries the
// access token, so following one that leaves Graph would hand the token to
// whoever answered.
func (d *reportData) walk(section providers.ReportSection, target string, visit func(json.RawMessage) bool) (int, bool, error) {
	rows := 0
	for page := 0; ; page++ {
		if err := d.ctx.Err(); err != nil {
			return rows, false, err
		}
		if page >= reportPageLimit {
			return rows, true, nil
		}
		var answer reportPage
		if err := d.provider.getJSON(d.ctx, target, d.token, &answer); err != nil {
			return rows, false, err
		}
		for _, raw := range answer.Value {
			if rows >= reportRowLimit {
				return rows, true, nil
			}
			if visit(raw) {
				rows++
			}
		}
		d.progress(providers.ReportProgress{Section: section, Rows: rows})
		if answer.NextLink == "" {
			return rows, false, nil
		}
		if !strings.HasPrefix(answer.NextLink, baseURL+"/") {
			return rows, false, fmt.Errorf("microsoft graph: a report page link left Graph (%s)", safeURL(answer.NextLink))
		}
		target = answer.NextLink
	}
}

// collectionTarget builds the first page of one collection.
func collectionTarget(resource string, selectFields []string, values url.Values) string {
	query := url.Values{}
	for key, list := range values {
		query[key] = list
	}
	if len(selectFields) > 0 {
		query.Set("$select", strings.Join(selectFields, ","))
	}
	encoded := query.Encode()
	if encoded == "" {
		return baseURL + resource
	}
	return baseURL + resource + "?" + encoded
}

// usersTarget builds the first page of the users collection. The licence
// property is optional: see loadUsers.
func usersTarget(withLicences bool) string {
	fields := []string{
		"id", "displayName", "userPrincipalName", "mail", "userType", "accountEnabled",
		"jobTitle", "department", "officeLocation", "createdDateTime",
	}
	if withLicences {
		fields = append(fields, "assignedLicenses")
	}
	return collectionTarget("/users", fields, url.Values{"$top": {strconv.Itoa(reportPageSize)}})
}

// groupsTarget builds the first page of the groups collection.
func groupsTarget() string {
	return collectionTarget("/groups", []string{
		"id", "displayName", "mail", "mailEnabled", "securityEnabled",
		"groupTypes", "visibility", "createdDateTime",
	}, url.Values{"$top": {strconv.Itoa(reportPageSize)}})
}

// teamsTarget builds the first page of the teams collection. It is a distinct
// endpoint from `/groups` (a team is the workload behind a group), which is why
// the teams section lists it instead of filtering the groups page.
func teamsTarget() string {
	return collectionTarget("/teams", []string{
		"id", "displayName", "description", "visibility", "isArchived", "createdDateTime",
	}, url.Values{"$top": {strconv.Itoa(reportPageSize)}})
}

// membersTarget builds the first page of the members or owners of a group.
//
// The projection is deliberately the small set every directory object type
// carries a value for (a member can be a user, a nested group, a device or a
// service principal): Graph rejects a `$select` naming a property the collection
// does not have, and losing the whole section over one column would be a poor
// trade. The `@odata.type` of each record is what the member type column shows.
func membersTarget(groupID, relation string) string {
	return collectionTarget("/groups/"+url.PathEscape(groupID)+"/"+relation, []string{
		"id", "displayName", "userPrincipalName", "mail",
	}, nil)
}

// teamMembersTarget builds the first page of the members of a team.
//
// `email` and `userId` belong to the `aadUserConversationMember` derived type, not
// to the `conversationMember` base type the collection is typed as, and a tenant
// whose Graph resolves `$select` against that base type answers 400 ("Could not
// find a property named 'email' on type 'microsoft.graph.conversationMember'").
// The projection is therefore optional: walkTeamMembers reads the collection again
// without it, which is the plain listing of the Graph documentation and carries
// the address of every member as well.
func teamMembersTarget(teamID string, projected bool) string {
	fields := []string{"id", "displayName", "email", "userId", "roles"}
	if !projected {
		fields = nil
	}
	return collectionTarget("/teams/"+url.PathEscape(teamID)+"/members", fields, nil)
}

// plansTarget builds the first page of the Planner plans of a group.
func plansTarget(groupID string) string {
	return collectionTarget("/groups/"+url.PathEscape(groupID)+"/planner/plans", []string{
		"id", "title", "owner", "createdDateTime",
	}, nil)
}

// loadUsers reads the users collection once, with the licence column when the
// tenant allows it.
//
// `assignedLicenses` is a navigation property and a tenant may refuse it in
// $select (Graph answers 400). The reader retries the collection without it and
// leaves the licence column empty: losing the whole users section over one
// optional column would be a poor trade, and the licences section still reports
// the catalogue.
func (d *reportData) loadUsers() {
	if d.usersLoaded {
		return
	}
	d.usersLoaded = true
	users, cut, err := d.scanUsers(providers.ReportSectionUsers, usersTarget(true))
	if isStatus(err, http.StatusBadRequest) {
		users, cut, err = d.scanUsers(providers.ReportSectionUsers, usersTarget(false))
	}
	d.users, d.usersCut, d.usersErr = users, cut, err
}

// scanUsers reads one users collection.
func (d *reportData) scanUsers(section providers.ReportSection, target string) ([]reportUser, bool, error) {
	users := make([]reportUser, 0, reportPageSize)
	_, cut, err := d.walk(section, target, func(raw json.RawMessage) bool {
		var user reportUser
		if err := json.Unmarshal(raw, &user); err != nil {
			return false
		}
		users = append(users, user)
		return true
	})
	return users, cut, err
}

// loadGroups reads the groups collection once; the distribution lists section
// filters it instead of listing the directory a second time.
func (d *reportData) loadGroups() {
	if d.groupsLoaded {
		return
	}
	d.groupsLoaded = true
	groups := make([]reportGroup, 0, reportPageSize)
	_, cut, err := d.walk(providers.ReportSectionGroups, groupsTarget(), func(raw json.RawMessage) bool {
		var group reportGroup
		if err := json.Unmarshal(raw, &group); err != nil {
			return false
		}
		groups = append(groups, group)
		return true
	})
	d.groups, d.groupsCut, d.groupsErr = groups, cut, err
}

// loadTeams reads the teams collection once; the teams section lists it and the
// team members section opens it, so one call serves both.
func (d *reportData) loadTeams() {
	if d.teamsLoaded {
		return
	}
	d.teamsLoaded = true
	teams := make([]reportTeam, 0, reportPageSize)
	_, cut, err := d.walk(providers.ReportSectionTeams, teamsTarget(), func(raw json.RawMessage) bool {
		var team reportTeam
		if err := json.Unmarshal(raw, &team); err != nil {
			return false
		}
		teams = append(teams, team)
		return true
	})
	d.teams, d.teamsCut, d.teamsErr = teams, cut, err
}

// loadSkus reads the subscribed licence catalogue once. The users section uses
// it to turn a sku id into a readable name and the licences section lists it, so
// one call serves both.
func (d *reportData) loadSkus() {
	if d.skusLoaded {
		return
	}
	d.skusLoaded = true
	skus := make([]reportSku, 0, 32)
	target := collectionTarget("/subscribedSkus", []string{
		"skuId", "skuPartNumber", "capabilityStatus", "appliesTo",
		"consumedUnits", "prepaidUnits", "servicePlans",
	}, nil)
	_, _, err := d.walk(providers.ReportSectionLicenses, target, func(raw json.RawMessage) bool {
		var sku reportSku
		if err := json.Unmarshal(raw, &sku); err != nil {
			return false
		}
		skus = append(skus, sku)
		return true
	})
	d.skus, d.skusErr = skus, err
}

// skuNames maps a sku id to its part number, which is the name an operator
// recognises (`ENTERPRISEPACK`, not a GUID).
func (d *reportData) skuNames() map[string]string {
	d.loadSkus()
	names := make(map[string]string, len(d.skus))
	for _, sku := range d.skus {
		if sku.SkuID == "" || sku.SkuPartNumber == "" {
			continue
		}
		names[strings.ToLower(sku.SkuID)] = sku.SkuPartNumber
	}
	return names
}

// column is one column of a section: the key the API and the CSV agree on and
// the label both display.
func column(key, title string) providers.ReportColumn {
	return providers.ReportColumn{Key: key, Title: title}
}

// userColumns is the header of the users and guests sections.
func userColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("display_name", "Display name"),
		column("user_principal_name", "User principal name"),
		column("mail", "Mail"),
		column("user_type", "User type"),
		column("account_enabled", "Account enabled"),
		column("job_title", "Job title"),
		column("department", "Department"),
		column("office_location", "Office location"),
		column("created", "Created"),
		column("licenses", "Licenses"),
		column("license_count", "License count"),
	}
}

// groupColumns is the header of the groups section.
func groupColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("display_name", "Display name"),
		column("mail", "Mail"),
		column("kind", "Kind"),
		column("mail_enabled", "Mail enabled"),
		column("security_enabled", "Security enabled"),
		column("visibility", "Visibility"),
		column("group_types", "Group types"),
		column("created", "Created"),
	}
}

// distributionColumns is the header of the distribution lists section.
func distributionColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("display_name", "Display name"),
		column("mail", "Mail"),
		column("created", "Created"),
	}
}

// siteColumns is the header of the sites section.
func siteColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("display_name", "Display name"),
		column("name", "Name"),
		column("web_url", "Web URL"),
		column("created", "Created"),
		column("last_modified", "Last modified"),
		column("site_id", "Site id"),
	}
}

// licenseColumns is the header of the licences section.
func licenseColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("sku_part_number", "SKU part number"),
		column("sku_id", "SKU id"),
		column("status", "Status"),
		column("applies_to", "Applies to"),
		column("enabled_units", "Enabled units"),
		column("consumed_units", "Consumed units"),
		column("available_units", "Available units"),
		column("usage_percent", "Usage (%)"),
		column("service_plan_count", "Service plan count"),
	}
}

// domainColumns is the header of the domains section.
func domainColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("domain", "Domain"),
		column("is_default", "Default"),
		column("is_initial", "Initial"),
		column("is_verified", "Verified"),
		column("authentication_type", "Authentication type"),
		column("services", "Services"),
	}
}

// groupMemberColumns is the header of the group members section.
func groupMemberColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("group", "Group"),
		column("member", "Member"),
		column("member_type", "Member type"),
		column("mail", "Mail"),
		column("role", "Role"),
		column("member_id", "Member id"),
	}
}

// teamColumns is the header of the teams section.
func teamColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("display_name", "Display name"),
		column("visibility", "Visibility"),
		column("archived", "Archived"),
		column("description", "Description"),
		column("created", "Created"),
		column("team_id", "Team id"),
	}
}

// teamMemberColumns is the header of the team members section.
func teamMemberColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("team", "Team"),
		column("member", "Member"),
		column("mail", "Mail"),
		column("role", "Role"),
		column("user_id", "User id"),
	}
}

// plannerPlanColumns is the header of the planner plans section.
func plannerPlanColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("group", "Group"),
		column("plan", "Plan"),
		column("owner", "Owner"),
		column("created", "Created"),
		column("plan_id", "Plan id"),
	}
}

// organizationColumns is the header of the organization section.
func organizationColumns() []providers.ReportColumn {
	return []providers.ReportColumn{
		column("tenant_id", "Tenant id"),
		column("display_name", "Display name"),
		column("country", "Country"),
		column("preferred_language", "Preferred language"),
		column("created", "Created"),
		column("default_domain", "Default domain"),
		column("verified_domains", "Verified domains"),
		column("notification_mails", "Notification mails"),
		column("on_premises_sync", "On-premises sync"),
	}
}

// textCell trims a value so a CSV never carries a stray newline or padding.
func textCell(value string) string { return strings.TrimSpace(value) }

// isUnified reports whether the group is a Microsoft 365 group. The `Unified`
// type in `groupTypes` is what the kind column and the Planner walk both read: a
// security group or a distribution list has no workload behind it.
func (g reportGroup) isUnified() bool {
	for _, kind := range g.GroupTypes {
		if strings.EqualFold(strings.TrimSpace(kind), "Unified") {
			return true
		}
	}
	return false
}

// label is how a group is named inside another section: its display name, then its
// mail, then its id. A group an administrator never named is still reported, and
// it is reported under something an operator can search for.
func (g reportGroup) label() string {
	if name := textCell(g.DisplayName); name != "" {
		return name
	}
	if mail := textCell(g.Mail); mail != "" {
		return mail
	}
	return textCell(g.ID)
}

// decodeMember reads one member or owner record, answering false for a document
// the section cannot use.
func decodeMember(raw json.RawMessage) (reportMember, bool) {
	var member reportMember
	if err := json.Unmarshal(raw, &member); err != nil {
		return reportMember{}, false
	}
	return member, true
}

// memberRow renders one member or owner of a group. The address column falls back
// to the user principal name, because a user whose mailbox was never provisioned
// has a mail property that is empty while the UPN is still the address people use.
func memberRow(group string, member reportMember, role string) providers.ReportRow {
	mail := textCell(member.Mail)
	if mail == "" {
		mail = textCell(member.UserPrincipalName)
	}
	return providers.ReportRow{
		group,
		textCell(member.DisplayName),
		memberType(member.ODataType),
		mail,
		role,
		textCell(member.ID),
	}
}

// memberType turns the `@odata.type` of a directory object into the short word the
// member type column shows (`user`, `group`, `servicePrincipal`, …). The type is
// what tells a nested group or a service principal from a person, which a bare
// display name cannot.
func memberType(odataType string) string {
	return strings.TrimPrefix(textCell(odataType), "#microsoft.graph.")
}

// teamRole renders the roles of a team member as the one word the role column
// carries. A member can hold several roles at once; owner is the one that grants
// the team, so it wins and the row stays filterable.
func teamRole(roles []string) string {
	for _, role := range roles {
		if strings.EqualFold(strings.TrimSpace(role), "owner") {
			return "owner"
		}
	}
	return "member"
}

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

// isGuestUser reports whether a record is a guest account.
func isGuestUser(user reportUser) bool {
	return strings.EqualFold(strings.TrimSpace(user.UserType), "guest")
}

// hasAssignedLicences reports whether any user of the listing carries a licence,
// which is what makes the sku catalogue worth reading.
func hasAssignedLicences(users []reportUser) bool {
	for _, user := range users {
		if len(user.AssignedLicenses) > 0 {
			return true
		}
	}
	return false
}

// usersTable builds the users section, or the guests section when guests is
// true: the second is the first filtered, which is why it costs no extra call.
// The users section keeps every account (the user type column tells a guest from
// a member), so a report that skips the guests section still lists them.
func (d *reportData) usersTable(guests bool) providers.ReportTable {
	table := providers.ReportTable{Columns: userColumns()}
	d.loadUsers()
	if d.usersErr != nil {
		table.Error = d.usersErr.Error()
		return table
	}
	// The licence names come from the catalogue, so the call is only made when
	// a user actually carries a licence: a tenant that refused the property
	// must not pay for a mapping nobody reads.
	names := map[string]string{}
	if hasAssignedLicences(d.users) {
		names = d.skuNames()
	}
	for _, user := range d.users {
		if guests && !isGuestUser(user) {
			continue
		}
		licenses := make([]string, 0, len(user.AssignedLicenses))
		for _, license := range user.AssignedLicenses {
			name := names[strings.ToLower(license.SkuID)]
			if name == "" {
				name = license.SkuID
			}
			if name != "" {
				licenses = append(licenses, name)
			}
		}
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(user.DisplayName),
			textCell(user.UserPrincipalName),
			textCell(user.Mail),
			textCell(user.UserType),
			flagCell(user.AccountEnabled),
			textCell(user.JobTitle),
			textCell(user.Department),
			textCell(user.OfficeLocation),
			textCell(user.CreatedDateTime),
			strings.Join(licenses, "; "),
			strconv.Itoa(len(licenses)),
		})
	}
	table.Truncated = d.usersCut
	return table
}

// groupKind classifies a group the way an operator reads it: a Microsoft 365
// group (Teams backed), a security group that also routes mail, a pure
// distribution list or a plain security group.
func groupKind(group reportGroup) string {
	mail := group.MailEnabled != nil && *group.MailEnabled
	security := group.SecurityEnabled != nil && *group.SecurityEnabled
	switch {
	case group.isUnified():
		return "microsoft365"
	case mail && security:
		return "mail_enabled_security"
	case mail:
		return "distribution_list"
	default:
		return "security"
	}
}

// groupsTable builds the groups section, or the distribution lists section when
// distribution is true. Like the users section the full table keeps every group
// (the kind column classifies it), so skipping the distribution lists section
// never hides one.
func (d *reportData) groupsTable(distribution bool) providers.ReportTable {
	table := providers.ReportTable{Columns: groupColumns()}
	if distribution {
		table.Columns = distributionColumns()
	}
	d.loadGroups()
	if d.groupsErr != nil {
		table.Error = d.groupsErr.Error()
		return table
	}
	for _, group := range d.groups {
		kind := groupKind(group)
		if distribution && kind != "distribution_list" {
			continue
		}
		if distribution {
			table.Rows = append(table.Rows, providers.ReportRow{
				textCell(group.DisplayName),
				textCell(group.Mail),
				textCell(group.CreatedDateTime),
			})
			continue
		}
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(group.DisplayName),
			textCell(group.Mail),
			kind,
			flagCell(group.MailEnabled),
			flagCell(group.SecurityEnabled),
			textCell(group.Visibility),
			strings.Join(group.GroupTypes, "; "),
			textCell(group.CreatedDateTime),
		})
	}
	table.Truncated = d.groupsCut
	return table
}

// sitesTable builds the sites section from GET /sites?search=*.
//
// A personal Microsoft account owns no site collection and Graph refuses the
// endpoint for it, so the failure is reported on the section (the rest of the
// report is still worth having) instead of aborting the pass.
func (d *reportData) sitesTable() providers.ReportTable {
	table := providers.ReportTable{Columns: siteColumns()}
	target := collectionTarget("/sites", []string{
		"id", "displayName", "name", "webUrl", "createdDateTime", "lastModifiedDateTime",
	}, url.Values{"search": {"*"}})
	_, cut, err := d.walk(providers.ReportSectionSites, target, func(raw json.RawMessage) bool {
		var site reportSite
		if err := json.Unmarshal(raw, &site); err != nil {
			return false
		}
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(site.DisplayName),
			textCell(site.Name),
			textCell(site.WebURL),
			textCell(site.CreatedDateTime),
			textCell(site.LastModifiedDateTime),
			textCell(site.ID),
		})
		return true
	})
	table.Truncated = cut
	table.Error = errorText(err)
	return table
}

// licensesTable builds the licences section from the subscribed catalogue, with
// the consumption of every subscription.
func (d *reportData) licensesTable() providers.ReportTable {
	table := providers.ReportTable{Columns: licenseColumns()}
	d.loadSkus()
	if d.skusErr != nil {
		table.Error = d.skusErr.Error()
		return table
	}
	for _, sku := range d.skus {
		enabled := sku.PrepaidUnits.Enabled
		available := enabled - sku.ConsumedUnits
		if available < 0 {
			available = 0
		}
		usage := ""
		if enabled > 0 {
			usage = strconv.FormatFloat(float64(sku.ConsumedUnits)*100/float64(enabled), 'f', 1, 64)
		}
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(sku.SkuPartNumber),
			textCell(sku.SkuID),
			textCell(sku.CapabilityStatus),
			textCell(sku.AppliesTo),
			strconv.Itoa(enabled),
			strconv.Itoa(sku.ConsumedUnits),
			strconv.Itoa(available),
			usage,
			strconv.Itoa(len(sku.ServicePlans)),
		})
	}
	return table
}

// domainsTable builds the domains section: every domain of the tenant with its
// verification state and the services it carries.
func (d *reportData) domainsTable() providers.ReportTable {
	table := providers.ReportTable{Columns: domainColumns()}
	target := collectionTarget("/domains", []string{
		"id", "isDefault", "isInitial", "isVerified", "authenticationType", "supportedServices",
	}, nil)
	_, cut, err := d.walk(providers.ReportSectionDomains, target, func(raw json.RawMessage) bool {
		var domain reportDomain
		if err := json.Unmarshal(raw, &domain); err != nil {
			return false
		}
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(domain.ID),
			boolCell(domain.IsDefault),
			boolCell(domain.IsInitial),
			boolCell(domain.IsVerified),
			textCell(domain.AuthenticationType),
			strings.Join(domain.SupportedServices, "; "),
		})
		return true
	})
	table.Truncated = cut
	table.Error = errorText(err)
	return table
}

// groupMembersTable builds the group members section: who belongs to every group
// of the directory, with the role the group gives them.
//
// The section pays two calls per group (owners, then members) because Graph keeps
// the two relationships apart while a report wants one row per subject with the
// role that matters. A person who is both is therefore reported once, as an
// owner: a second row would say nothing the first does not. It is the same pair of
// endpoints (`/groups/{id}/members`, `/groups/{id}/owners`) the n8n flow called
// for every group it walked.
//
// The walk is bounded (reportGroupWalkLimit groups, reportRowLimit rows) because
// every parent costs calls: a tenant with tens of thousands of groups would turn
// one report into a throttling storm, and a truncated section says so instead.
func (d *reportData) groupMembersTable() providers.ReportTable {
	table := providers.ReportTable{Columns: groupMemberColumns()}
	d.loadGroups()
	if d.groupsErr != nil {
		table.Error = d.groupsErr.Error()
		return table
	}
	walked := 0
	for _, group := range d.groups {
		if err := d.ctx.Err(); err != nil {
			table.Error = err.Error()
			return table
		}
		if walked >= reportGroupWalkLimit || len(table.Rows) >= reportRowLimit {
			table.Truncated = true
			return table
		}
		if strings.TrimSpace(group.ID) == "" {
			continue
		}
		walked++
		label := group.label()
		owners := map[string]bool{}
		_, cut, err := d.walk(providers.ReportSectionGroupMembers, membersTarget(group.ID, "owners"), func(raw json.RawMessage) bool {
			member, ok := decodeMember(raw)
			if !ok {
				return false
			}
			if member.ID != "" {
				owners[member.ID] = true
			}
			table.Rows = append(table.Rows, memberRow(label, member, "owner"))
			return true
		})
		if cut {
			table.Truncated = true
		}
		if err != nil {
			table.Error = errorText(err)
			return table
		}
		_, cut, err = d.walk(providers.ReportSectionGroupMembers, membersTarget(group.ID, "members"), func(raw json.RawMessage) bool {
			member, ok := decodeMember(raw)
			if !ok {
				return false
			}
			if owners[member.ID] {
				return false
			}
			table.Rows = append(table.Rows, memberRow(label, member, "member"))
			return true
		})
		if cut {
			table.Truncated = true
		}
		if err != nil {
			table.Error = errorText(err)
			return table
		}
	}
	return table
}

// teamsTable builds the teams section from the cached teams collection.
func (d *reportData) teamsTable() providers.ReportTable {
	table := providers.ReportTable{Columns: teamColumns()}
	d.loadTeams()
	if d.teamsErr != nil {
		table.Error = d.teamsErr.Error()
		return table
	}
	for _, team := range d.teams {
		table.Rows = append(table.Rows, providers.ReportRow{
			textCell(team.DisplayName),
			textCell(team.Visibility),
			flagCell(team.IsArchived),
			textCell(team.Description),
			textCell(team.CreatedDateTime),
			textCell(team.ID),
		})
	}
	table.Truncated = d.teamsCut
	return table
}

// teamMembersTable builds the team members section: who belongs to every team,
// with the role the team gives them. It reads `/teams/{id}/members` rather than
// the group membership, because the conversation roles of a member (owner of the
// team, as opposed to owner of the backing group) only exist there.
func (d *reportData) teamMembersTable() providers.ReportTable {
	table := providers.ReportTable{Columns: teamMemberColumns()}
	d.loadTeams()
	if d.teamsErr != nil {
		table.Error = d.teamsErr.Error()
		return table
	}
	walked := 0
	for _, team := range d.teams {
		if err := d.ctx.Err(); err != nil {
			table.Error = err.Error()
			return table
		}
		if walked >= reportGroupWalkLimit || len(table.Rows) >= reportRowLimit {
			table.Truncated = true
			return table
		}
		if strings.TrimSpace(team.ID) == "" {
			continue
		}
		walked++
		label := textCell(team.DisplayName)
		if label == "" {
			label = textCell(team.ID)
		}
		cut, err := d.walkTeamMembers(team.ID, func(member reportTeamMember) bool {
			table.Rows = append(table.Rows, providers.ReportRow{
				label,
				textCell(member.DisplayName),
				textCell(member.Email),
				teamRole(member.Roles),
				textCell(member.UserID),
			})
			return true
		})
		if cut {
			table.Truncated = true
		}
		if err != nil {
			table.Error = errorText(err)
			return table
		}
	}
	return table
}

// walkTeamMembers walks the membership of one team, handing every member to visit
// and answering whether the listing was cut and the first error that stopped it.
//
// The `email` projection is optional (see teamMembersTarget): a 400 on the first
// page means the tenant read the `$select` against the base conversation member
// type, so the collection is read again as the plain listing — which carries the
// address too, unlike the licence fallback of loadUsers, so no column is lost. The
// refusal is remembered on the pass so the teams after it are read plainly first
// instead of paying a doomed call each; a 400 that already produced rows is a real
// failure and is left to the section.
func (d *reportData) walkTeamMembers(teamID string, visit func(reportTeamMember) bool) (bool, error) {
	read := func(projected bool) (int, bool, error) {
		return d.walk(providers.ReportSectionTeamMembers, teamMembersTarget(teamID, projected), func(raw json.RawMessage) bool {
			var member reportTeamMember
			if err := json.Unmarshal(raw, &member); err != nil {
				return false
			}
			return visit(member)
		})
	}
	projected := !d.teamMembersPlain
	rows, cut, err := read(projected)
	if projected && rows == 0 && isStatus(err, http.StatusBadRequest) {
		d.teamMembersPlain = true
		_, cut, err = read(false)
	}
	return cut, err
}

// plannerPlansTable builds the planner plans section: every plan of every
// Microsoft 365 group, with the group that owns it. Graph has no directory-wide
// plans collection (a plan hangs off the group that owns it), which is why this is
// the groups walk.
//
// Only a Microsoft 365 group can own a plan (a security group or a distribution
// list has no Planner workload behind it), so the groups page is filtered first: a
// tenant whose directory is mostly security groups would otherwise pay a refused
// call per group. A group Planner answers 403 for — the signed-in user cannot see
// its plans — or 404 for — it never had one — is skipped as well: that is a
// visibility boundary, not a broken report, and losing the plans of every group
// that did answer would be a poor trade. The section fails only when every group
// that could own a plan refused it, which is the shape of a missing grant rather
// than of an empty tenant.
func (d *reportData) plannerPlansTable() providers.ReportTable {
	table := providers.ReportTable{Columns: plannerPlanColumns()}
	d.loadGroups()
	if d.groupsErr != nil {
		table.Error = d.groupsErr.Error()
		return table
	}
	walked := 0
	answered := 0
	var refusal error
	for _, group := range d.groups {
		if err := d.ctx.Err(); err != nil {
			table.Error = err.Error()
			return table
		}
		if walked >= reportGroupWalkLimit || len(table.Rows) >= reportRowLimit {
			table.Truncated = true
			return table
		}
		if strings.TrimSpace(group.ID) == "" || !group.isUnified() {
			continue
		}
		walked++
		label := group.label()
		_, cut, err := d.walk(providers.ReportSectionPlannerPlans, plansTarget(group.ID), func(raw json.RawMessage) bool {
			var plan reportPlan
			if err := json.Unmarshal(raw, &plan); err != nil {
				return false
			}
			table.Rows = append(table.Rows, providers.ReportRow{
				label,
				textCell(plan.Title),
				textCell(plan.Owner),
				textCell(plan.CreatedDateTime),
				textCell(plan.ID),
			})
			return true
		})
		if cut {
			table.Truncated = true
		}
		if err != nil {
			if isGroupRefusal(err) {
				if refusal == nil {
					refusal = err
				}
				continue
			}
			table.Error = errorText(err)
			return table
		}
		answered++
	}
	if answered == 0 && len(table.Rows) == 0 && refusal != nil {
		table.Error = errorText(refusal)
	}
	return table
}

// organizationTable builds the organization section: the tenant record itself,
// which is a single document rather than a collection.
func (d *reportData) organizationTable() providers.ReportTable {
	table := providers.ReportTable{Columns: organizationColumns()}
	target := collectionTarget("/organization", []string{
		"id", "displayName", "countryLetterCode", "preferredLanguage", "createdDateTime",
		"verifiedDomains", "technicalNotificationMails", "onPremisesSyncEnabled",
	}, nil)
	var answer reportPage
	if err := d.provider.getJSON(d.ctx, target, d.token, &answer); err != nil {
		table.Error = err.Error()
		return table
	}
	if len(answer.Value) == 0 {
		return table
	}
	var organization reportOrganization
	if err := json.Unmarshal(answer.Value[0], &organization); err != nil {
		table.Error = fmt.Errorf("microsoft graph: cannot read the organization record: %w", err).Error()
		return table
	}
	domains := make([]string, 0, len(organization.VerifiedDomains))
	defaultDomain := ""
	for _, domain := range organization.VerifiedDomains {
		name := textCell(domain.Name)
		if name == "" {
			continue
		}
		domains = append(domains, name)
		if domain.IsDefault && defaultDomain == "" {
			defaultDomain = name
		}
	}
	table.Rows = append(table.Rows, providers.ReportRow{
		textCell(organization.ID),
		textCell(organization.DisplayName),
		textCell(organization.CountryLetterCode),
		textCell(organization.PreferredLanguage),
		textCell(organization.CreatedDateTime),
		defaultDomain,
		strings.Join(domains, "; "),
		strings.Join(organization.TechnicalNotificationMails, "; "),
		flagCell(organization.OnPremisesSyncEnabled),
	})
	return table
}

// errorText renders an error as the message of a failed section, or an empty
// string when the section succeeded.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// isStatus reports whether err is a Graph refusal with the given status, which is
// how the collector tells "this tenant does not serve that projection" apart from
// "the report is broken".
func isStatus(err error, status int) bool {
	var apiErr *graphError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// isGroupRefusal reports whether err is Graph refusing one parent of a
// per-collection walk: 403 when the signed-in user cannot see the plans of the
// group, 404 when the group has none. Both mean "nothing to report here", so the
// walk moves on to the next parent instead of failing the section.
func isGroupRefusal(err error) bool {
	return isStatus(err, http.StatusForbidden) || isStatus(err, http.StatusNotFound)
}
