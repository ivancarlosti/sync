package providers

import (
	"context"
	"fmt"
	"strings"

	"github.com/ivancarlosti/sync/internal/models"
)

// ReportSection identifies one table of a directory report.
//
// A directory report is the read-only description of the organisation behind an
// account: who the users are, which groups and teams exist, which sites were
// created, which licences are consumed. Every section is an independent table,
// so a tenant that refuses one endpoint (a personal account has no `/sites`, an
// administrator may not read the licence catalogue) still produces a usable
// report with the sections it does allow.
type ReportSection string

const (
	// ReportSectionOrganization is the tenant itself: id, name, country,
	// creation date, default domain.
	ReportSectionOrganization ReportSection = "organization"
	// ReportSectionUsers is every user account of the directory.
	ReportSectionUsers ReportSection = "users"
	// ReportSectionGuests is the subset of users whose user type is Guest. It is
	// derived from the users page instead of a second listing call.
	ReportSectionGuests ReportSection = "guests"
	// ReportSectionGroups is every group: Microsoft 365, security and mail
	// enabled.
	ReportSectionGroups ReportSection = "groups"
	// ReportSectionDistributionLists is the subset of groups that only route
	// mail (`mailEnabled` and not `securityEnabled`, no Teams backing). It is
	// derived from the groups page.
	ReportSectionDistributionLists ReportSection = "distribution_lists"
	// ReportSectionSites is every SharePoint site collection of the tenant.
	ReportSectionSites ReportSection = "sites"
	// ReportSectionLicenses is the subscription catalogue with consumption.
	ReportSectionLicenses ReportSection = "licenses"
	// ReportSectionDomains is the verified and unverified domains of the tenant.
	ReportSectionDomains ReportSection = "domains"
	// ReportSectionSuspendedUsers is the subset of users whose account is
	// suspended. It is derived from the users page instead of a second listing
	// call, exactly like ReportSectionGuests, and it is the Google analogue of
	// it: a Workspace directory has no guest accounts, it has blocked ones.
	ReportSectionSuspendedUsers ReportSection = "suspended_users"
	// ReportSectionGroupMembers is the membership of every group: who belongs
	// to it, with which role and status.
	ReportSectionGroupMembers ReportSection = "group_members"
	// ReportSectionOrgUnits is the organisational unit tree, which is what
	// places a Workspace user in the hierarchy of the domain.
	ReportSectionOrgUnits ReportSection = "org_units"
	// ReportSectionRoles is the administrative role catalogue (super admin,
	// help desk, groups admin…).
	ReportSectionRoles ReportSection = "roles"
	// ReportSectionRoleAssignments is who holds which administrative role, over
	// which scope: the "who can do what" table of the domain.
	ReportSectionRoleAssignments ReportSection = "role_assignments"
	// ReportSectionSharedDrives is every Google Shared Drive of the domain.
	ReportSectionSharedDrives ReportSection = "shared_drives"
)

// ReportSections lists every section of the report vocabulary in the order the
// report and the UI display them.
//
// It is the *vocabulary*, not the catalogue of one provider: a section a
// provider cannot collect is filtered out by ReportSectionsFor before a report
// is started (and refused again by the section endpoint, so a stored report can
// never be read back as a table that was never built). New sections are appended
// so the tables of an already collected report keep their order.
var ReportSections = []ReportSection{
	ReportSectionOrganization,
	ReportSectionUsers,
	ReportSectionGuests,
	ReportSectionGroups,
	ReportSectionDistributionLists,
	ReportSectionSites,
	ReportSectionLicenses,
	ReportSectionDomains,
	ReportSectionSuspendedUsers,
	ReportSectionGroupMembers,
	ReportSectionOrgUnits,
	ReportSectionRoles,
	ReportSectionRoleAssignments,
	ReportSectionSharedDrives,
}

// reportSectionExclusive names the sections exactly one provider can collect:
// the rest of the vocabulary is shared, because the same concept exists in every
// directory (a user is a user, whether it comes from Entra ID or from a
// Workspace domain). The split is what keeps the two collectors honest — a
// Microsoft account is never offered the organisational units of a Google
// domain, and a Google account is never offered the SharePoint sites it does not
// have.
var reportSectionExclusive = map[ReportSection]models.ProviderName{
	ReportSectionGuests:            models.ProviderMicrosoft,
	ReportSectionDistributionLists: models.ProviderMicrosoft,
	ReportSectionSites:             models.ProviderMicrosoft,

	ReportSectionSuspendedUsers:  models.ProviderGoogle,
	ReportSectionGroupMembers:    models.ProviderGoogle,
	ReportSectionOrgUnits:        models.ProviderGoogle,
	ReportSectionRoles:           models.ProviderGoogle,
	ReportSectionRoleAssignments: models.ProviderGoogle,
	ReportSectionSharedDrives:    models.ProviderGoogle,
}

// ReportSectionsFor answers the sections a provider can collect: the shared ones
// plus the ones it owns, in the canonical order of ReportSections. A provider
// with no collector of its own answers an empty list, which is what makes the
// API answer `supported:false` instead of offering a report nothing could build.
func ReportSectionsFor(name models.ProviderName) []ReportSection {
	sections := make([]ReportSection, 0, len(ReportSections))
	for _, section := range ReportSections {
		if owner, exclusive := reportSectionExclusive[section]; exclusive && owner != name {
			continue
		}
		sections = append(sections, section)
	}
	return sections
}

// Valid reports whether the value is a section of the vocabulary.
func (s ReportSection) Valid() bool {
	for _, known := range ReportSections {
		if s == known {
			return true
		}
	}
	return false
}

// ReportSectionsFrom validates and normalises the sections a caller requested.
// Duplicates are dropped, unknown values are refused, and an empty request means
// every section, in the order of ReportSections.
func ReportSectionsFrom(raw []string) ([]ReportSection, error) {
	if len(raw) == 0 {
		return append([]ReportSection(nil), ReportSections...), nil
	}
	seen := make(map[ReportSection]bool, len(raw))
	for _, value := range raw {
		section := ReportSection(strings.TrimSpace(value))
		if !section.Valid() {
			return nil, fmt.Errorf("%w: unknown report section %q", ErrInvalidIdentifier, string(section))
		}
		seen[section] = true
	}
	ordered := make([]ReportSection, 0, len(seen))
	for _, section := range ReportSections {
		if seen[section] {
			ordered = append(ordered, section)
		}
	}
	return ordered, nil
}

// ReportColumn is one column of a section. Key is the stable identifier the API
// and the CSV header use; Title is the human readable label of that header.
//
// Neither is translated: a report is a data export, so its header stays English
// whatever locale the operator reads the UI in (the same rule the audit CSV
// follows). The section name is the only part the UI translates.
type ReportColumn struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// ReportRow is one record of a section, aligned with the section columns: the
// cell at index i belongs to column i.
//
// Cells are strings because a report is a document, not a model: every value is
// rendered exactly once (a date as RFC 3339, a size in bytes, a boolean as
// true/false) and the column snapshot stored with the section keeps an old
// report readable after a column is added.
type ReportRow []string

// ReportTable is one collected section: its columns, its rows, and the two
// health flags the UI needs.
type ReportTable struct {
	Section ReportSection  `json:"section"`
	Columns []ReportColumn `json:"columns"`
	Rows    []ReportRow    `json:"rows"`
	// Truncated is true when the section stopped at the row cap, so every row
	// up to the cap is exact but the tail is missing.
	Truncated bool `json:"truncated"`
	// Error is the reason the section could not be collected (a refused scope, a
	// provider that does not have the endpoint). A failed section carries no
	// rows and never fails the report around it.
	Error string `json:"error,omitempty"`
}

// Failed reports whether the section carries an error instead of a table.
func (t ReportTable) Failed() bool { return strings.TrimSpace(t.Error) != "" }

// ReportProgress is what a collector reports while it works: the section it is
// filling and how many rows it holds so far. The service mirrors it onto the run
// row, which is all the polling UI reads.
type ReportProgress struct {
	Section ReportSection
	Rows    int
}

// ReportSource is implemented by providers that can build a directory report.
//
// It is optional, like ScopeCatalog and SiteBrowser, so the mandatory Provider
// contract stays about file synchronisation. A provider that does not implement
// it reports no sections and the API answers "not configured" instead of
// starting a report that could never be built.
type ReportSource interface {
	// ReportSections lists the sections this provider can collect, in display
	// order.
	ReportSections() []ReportSection
	// CollectReport builds the requested sections. A section that cannot be
	// collected is returned with ReportTable.Error set; the error return is
	// reserved for a failure that invalidates the whole pass (a cancelled
	// context, an unreadable token).
	CollectReport(ctx context.Context, creds Credentials, tokens *Tokens, sections []ReportSection, progress func(ReportProgress)) ([]ReportTable, error)
}

// ReportSourceOf returns the report source of a provider, or nil when the
// provider cannot build reports.
func ReportSourceOf(provider Provider) ReportSource {
	source, ok := provider.(ReportSource)
	if !ok {
		return nil
	}
	return source
}
