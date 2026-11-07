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
)

// ReportSections lists the sections this provider can collect. Every one of them
// is served by a scope Sync already requests, so a directory report needs no
// extra consent: users, groups, sites, licences and domains come from the same
// grant the capability badges of the accounts screen are derived from.
//
// The sections that would need Team.ReadBasic.All, Channel.ReadBasic.All or
// MailboxSettings.Read (teams, channels, mailbox purpose) are deliberately
// absent: they arrive when the permission table does, so the API can offer
// "reconnect the account" instead of failing a section nobody can fix.
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
	if err != nil {
		var apiErr *graphError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
			users, cut, err = d.scanUsers(providers.ReportSectionUsers, usersTarget(false))
		}
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
	unified := false
	for _, kind := range group.GroupTypes {
		if strings.EqualFold(kind, "Unified") {
			unified = true
			break
		}
	}
	mail := group.MailEnabled != nil && *group.MailEnabled
	security := group.SecurityEnabled != nil && *group.SecurityEnabled
	switch {
	case unified:
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
