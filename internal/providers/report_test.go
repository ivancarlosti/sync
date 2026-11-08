package providers

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ivancarlosti/sync/internal/models"
)

// TestReportSectionsFromNormalisesTheRequest pins the vocabulary the API accepts:
// no request means every section, duplicates collapse, the canonical order wins
// and an unknown name is refused instead of silently dropped.
func TestReportSectionsFromNormalisesTheRequest(t *testing.T) {
	all, err := ReportSectionsFrom(nil)
	if err != nil {
		t.Fatalf("ReportSectionsFrom(nil) = %v", err)
	}
	if fmt.Sprint(all) != fmt.Sprint(ReportSections) {
		t.Fatalf("ReportSectionsFrom(nil) = %v, want every section in order", all)
	}
	// The returned slice must be a copy: a caller sorting it must not reorder
	// the catalogue every other report reads.
	all[0] = ReportSection("mutated")
	if ReportSections[0] == ReportSection("mutated") {
		t.Fatal("ReportSectionsFrom returned the shared catalogue")
	}

	ordered, err := ReportSectionsFrom([]string{"domains", " users ", "users", "guests"})
	if err != nil {
		t.Fatalf("ReportSectionsFrom = %v", err)
	}
	want := []ReportSection{ReportSectionUsers, ReportSectionGuests, ReportSectionDomains}
	if fmt.Sprint(ordered) != fmt.Sprint(want) {
		t.Fatalf("ReportSectionsFrom = %v, want %v", ordered, want)
	}

	if _, err := ReportSectionsFrom([]string{"users", "channels"}); !errors.Is(err, ErrInvalidIdentifier) {
		t.Fatalf("ReportSectionsFrom(channels) = %v, want an invalid identifier", err)
	}
}

// TestReportSectionsForSplitsTheVocabulary pins the provider affinity of the
// vocabulary: a collector is only offered the sections its directory can actually
// produce, so the start form never lists a table that would fail on the first
// call (SharePoint sites on a Workspace domain, shared drives on Entra ID).
func TestReportSectionsForSplitsTheVocabulary(t *testing.T) {
	microsoft := ReportSectionsFor(models.ProviderMicrosoft)
	google := ReportSectionsFor(models.ProviderGoogle)

	for _, section := range google {
		if !section.Valid() {
			t.Fatalf("ReportSectionsFor(google) returned the unknown section %q", section)
		}
	}
	for _, section := range microsoft {
		if !section.Valid() {
			t.Fatalf("ReportSectionsFor(microsoft) returned the unknown section %q", section)
		}
	}
	if containsSection(microsoft, ReportSectionSharedDrives) || containsSection(microsoft, ReportSectionOrgUnits) {
		t.Fatal("a Microsoft account must not be offered a Google-only section")
	}
	if containsSection(microsoft, ReportSectionSharedDriveMembers) {
		t.Fatal("a Microsoft account must not be offered the shared drive membership of a Workspace domain")
	}
	if containsSection(google, ReportSectionSites) || containsSection(google, ReportSectionGuests) {
		t.Fatal("a Google account must not be offered a Microsoft-only section")
	}
	// Teams and Planner are Microsoft 365 products; a Workspace domain has
	// neither, so the start form never offers them there — and Shared Drive
	// membership is the Workspace answer to "who reaches this drive".
	for _, section := range []ReportSection{ReportSectionTeams, ReportSectionTeamMembers, ReportSectionPlannerPlans} {
		if !containsSection(microsoft, section) {
			t.Fatalf("the Microsoft collector is not offered the %q section", section)
		}
		if containsSection(google, section) {
			t.Fatalf("a Google account must not be offered the %q section", section)
		}
	}
	if !containsSection(google, ReportSectionSharedDriveMembers) {
		t.Fatal("the Google collector is not offered the shared drive members section")
	}
	// Every section of the vocabulary belongs to somebody: a section no provider
	// collects would be a dead entry the UI would offer forever.
	covered := map[ReportSection]bool{}
	for _, section := range append(append([]ReportSection{}, microsoft...), google...) {
		covered[section] = true
	}
	for _, section := range ReportSections {
		if !covered[section] {
			t.Fatalf("section %q is offered to no provider", section)
		}
	}
	// The shared sections are the ones both collectors produce; a section that
	// landed in neither exclusive list must be in both.
	for _, section := range ReportSections {
		inMicrosoft := containsSection(microsoft, section)
		inGoogle := containsSection(google, section)
		if inMicrosoft != inGoogle && !isExclusiveSection(section) {
			t.Fatalf("section %q is shared but offered to one provider only", section)
		}
	}
	if !containsSection(microsoft, ReportSectionSites) || !containsSection(google, ReportSectionSharedDrives) {
		t.Fatal("the exclusive sections landed on the wrong provider")
	}
}

// containsSection reports whether a section list holds a section.
func containsSection(sections []ReportSection, want ReportSection) bool {
	for _, section := range sections {
		if section == want {
			return true
		}
	}
	return false
}

// isExclusiveSection reports whether a section belongs to one provider only.
func isExclusiveSection(section ReportSection) bool {
	_, exclusive := reportSectionExclusive[section]
	return exclusive
}

// TestReportSectionsForUnknownProviderKeepsTheSharedOnes pins the fallback: a
// provider with no collector of its own is still offered the sections every
// directory has, which is what keeps the endpoint honest when a third provider
// lands.
func TestReportSectionsForUnknownProviderKeepsTheSharedOnes(t *testing.T) {
	sections := ReportSectionsFor(models.ProviderName("unknown"))
	if !containsSection(sections, ReportSectionUsers) || !containsSection(sections, ReportSectionDomains) {
		t.Fatalf("ReportSectionsFor(unknown) = %v, want the shared sections", sections)
	}
	if containsSection(sections, ReportSectionSites) || containsSection(sections, ReportSectionSuspendedUsers) {
		t.Fatalf("ReportSectionsFor(unknown) = %v, want no exclusive section", sections)
	}
	if len(sections) == len(ReportSections) {
		t.Fatal("ReportSectionsFor(unknown) returned the whole vocabulary")
	}
}

// TestReportTableFailed checks that a section with an error is a failed section
// even when it carries a blank reason padded with spaces.
func TestReportTableFailed(t *testing.T) {
	if (ReportTable{Section: ReportSectionUsers}).Failed() {
		t.Fatal("a table without an error must not be failed")
	}
	if !(ReportTable{Section: ReportSectionSites, Error: "not supported"}).Failed() {
		t.Fatal("a table with an error must be failed")
	}
	if (ReportTable{Section: ReportSectionSites, Error: "   "}).Failed() {
		t.Fatal("a blank error must not fail a section")
	}
}
