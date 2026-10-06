package microsoft

import (
	"context"
	"strings"
	"testing"

	graphmodels "github.com/microsoftgraph/msgraph-sdk-go/models"

	"github.com/ivancarlosti/sync/internal/providers"
)

// TestDriveFromGraphKeepsTheTwoFamiliesApart pins the kinds the folder picker
// groups by (see web/src/components/FolderPicker.vue): the account's own OneDrive
// is "personal" on a personal account and "business" on a work/school one, while
// every SharePoint library is "document_library". Folding the work OneDrive into
// document_library is exactly what left the picker's "OneDrive" select empty and
// listed the account's own drive as a SharePoint library.
func TestDriveFromGraphKeepsTheTwoFamiliesApart(t *testing.T) {
	const url = "https://contoso.sharepoint.com/sites/marketing/Documents"
	cases := []struct {
		name      string
		driveType string
		wantKind  string
	}{
		{name: "personal account", driveType: "personal", wantKind: "personal"},
		{name: "work account", driveType: "business", wantKind: "business"},
		{name: "work account, mixed case", driveType: "Business", wantKind: "business"},
		{name: "team library", driveType: "documentLibrary", wantKind: "document_library"},
		{name: "missing drive type", driveType: "", wantKind: "document_library"},
	}
	for _, testCase := range cases {
		drive := graphmodels.NewDrive()
		drive.SetId(ptr("drive-1"))
		drive.SetName(ptr("Documents"))
		drive.SetDriveType(ptr(testCase.driveType))
		drive.SetWebUrl(ptr(url))

		got := driveFromGraph(drive)
		if got.Kind != testCase.wantKind {
			t.Errorf("driveFromGraph(%s).Kind = %q, want %q", testCase.name, got.Kind, testCase.wantKind)
		}
		if got.Owner != url {
			t.Errorf("driveFromGraph(%s).Owner = %q, want the drive URL %q", testCase.name, got.Owner, url)
		}
	}
}

// TestDrivesCarryTheKindAndUrlOfEachRoot drives the real Graph builders behind a
// canned transport, so what the picker receives — the kind it groups by and the
// URL it tells two "Documents" apart with — is asserted end to end.
func TestDrivesCarryTheKindAndUrlOfEachRoot(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{
		"/v1.0/me/drive": `{"id":"b!onedrive","name":"Ivan","driveType":"business",` +
			`"webUrl":"https://contoso-my.sharepoint.com/personal/ivan_contoso_com/Documents"}`,
		"/v1.0/me/drives": `{"value":[` +
			`{"id":"b!onedrive","name":"Ivan","driveType":"business",` +
			`"webUrl":"https://contoso-my.sharepoint.com/personal/ivan_contoso_com/Documents"},` +
			`{"id":"b!library","name":"Documents","driveType":"documentLibrary",` +
			`"webUrl":"https://contoso.sharepoint.com/sites/marketing/Documents"}]}`,
	}}
	provider := New()
	provider.httpClient.Transport = transport

	drives, err := provider.Drives(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "drives-token"})
	if err != nil {
		t.Fatalf("Drives = %v", err)
	}
	if len(drives) != 2 {
		t.Fatalf("Drives = %+v, want the two roots of the account", drives)
	}
	oneDrive := drives[0]
	if oneDrive.ID != "b!onedrive" || oneDrive.Kind != "business" || oneDrive.Name != "Ivan" {
		t.Errorf("drives[0] = %+v, want the account's own OneDrive tagged business", oneDrive)
	}
	if oneDrive.Owner != "https://contoso-my.sharepoint.com/personal/ivan_contoso_com/Documents" {
		t.Errorf("drives[0].Owner = %q, want the drive URL", oneDrive.Owner)
	}
	library := drives[1]
	if library.Kind != "document_library" || library.Owner == "" {
		t.Errorf("drives[1] = %+v, want the library tagged and carrying its URL", library)
	}
}

// TestDrivesDropTheBookkeepingDrivesOfAPersonalAccount pins the broken personal
// account: `GET /me/drives` of a recent MSA also lists internal drives — an
// archive named ODCMetadataArchive and the Bundle drives — that Microsoft tags
// with the very same driveType "personal" as the real OneDrive, while their roots
// answer HTTP 400 "ObjectHandle is Invalid". The picker may only be offered the
// drive `GET /me/drive` names, and only once.
func TestDrivesDropTheBookkeepingDrivesOfAPersonalAccount(t *testing.T) {
	const ownID = "b!O_eMCHPCf0ysXS41VjTHz3ZuxvLQtANPkPKYvKefdK3fcT2ZuoImT54ODtNZQRub"
	transport := &recordingTransport{bodies: map[string]string{
		"/v1.0/me/drive": `{"id":"` + ownID + `","name":"OneDrive","driveType":"personal",` +
			`"webUrl":"https://onedrive.live.com/?id=root"}`,
		"/v1.0/me/drives": `{"value":[` +
			`{"id":"b!VZHvx3CtANPk","name":"ODCMetadataArchive","driveType":"personal"},` +
			`{"id":"BE77C2C82D9EC8EB","name":"Bundles_b896e2bb","driveType":"personal"},` +
			`{"id":"` + ownID + `","name":"OneDrive","driveType":"personal"},` +
			`{"id":"b!library","name":"Documents","driveType":"documentLibrary",` +
			`"webUrl":"https://contoso.sharepoint.com/sites/marketing/Documents"}]}`,
	}}
	provider := New()
	provider.httpClient.Transport = transport

	drives, err := provider.Drives(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "personal-token"})
	if err != nil {
		t.Fatalf("Drives = %v", err)
	}
	if len(drives) != 2 {
		t.Fatalf("Drives = %+v, want the own OneDrive and the library only", drives)
	}
	own := drives[0]
	if own.ID != ownID || own.Kind != "personal" || own.Name != "OneDrive" {
		t.Errorf("drives[0] = %+v, want the drive served for /me/drive", own)
	}
	if !strings.HasPrefix(transport.urls[0], "https://graph.microsoft.com/v1.0/me/drive?") {
		t.Errorf("the first request went to %q, want /me/drive", transport.urls[0])
	}
	if library := drives[1]; library.ID != "b!library" || library.Kind != "document_library" {
		t.Errorf("drives[1] = %+v, want the SharePoint library", library)
	}
}
