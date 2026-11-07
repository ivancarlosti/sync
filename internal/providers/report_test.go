package providers

import (
	"errors"
	"fmt"
	"testing"
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

	if _, err := ReportSectionsFrom([]string{"users", "teams"}); !errors.Is(err, ErrInvalidIdentifier) {
		t.Fatalf("ReportSectionsFrom(teams) = %v, want an invalid identifier", err)
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
