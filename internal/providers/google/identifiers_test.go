package google

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/providers"
)

// forbiddenValues are the shapes a Drive identifier can never have. They stand
// for what a stored job, a crafted explorer link or a hand-edited database row
// can carry into a request URL: a relative segment, another endpoint, a query or
// fragment delimiter, an encoded separator, an authority delimiter, a space, a
// control character — and a value that is simply too long.
var forbiddenValues = []string{
	"",
	"..",
	"../permissions",
	"files/1AbCdEf",
	"1AbCdEf/../../permissions",
	`1AbCdEf\..\permissions`,
	"1AbCdEf?alt=media",
	"1AbCdEf&supportsAllDrives=true",
	"1AbCdEf#fragment",
	"1AbCdEf%2F..%2Fpermissions",
	"1AbCdEf:2",
	"user@example.com",
	"1AbCdEf x",
	"1AbCdEf\nHost: evil.example",
	"1AbCdEf'x",
	"%2e%2e",
	strings.Repeat("a", 513),
}

// idCall hands one value to a provider entry point in a single identifier
// position.
type idCall struct {
	name string
	// allowsEmpty marks a position where an empty value is how the interface
	// says "the default": the root folder, the drive of the account, or "create
	// the item" instead of "update it". Everywhere else an empty value is not an
	// identifier either.
	allowsEmpty bool
	call        func(provider *Provider, value string) error
}

// idCalls covers every method that interpolates an identifier into a request,
// in the path and in the optional drive position.
func idCalls(ctx context.Context) []idCall {
	creds := providers.Credentials{}
	tokens := &providers.Tokens{AccessToken: "test-token"}
	return []idCall{
		{name: "Children folder", allowsEmpty: true, call: func(p *Provider, value string) error {
			_, err := p.Children(ctx, creds, tokens, "drive-1", value)
			return err
		}},
		{name: "Children drive", allowsEmpty: true, call: func(p *Provider, value string) error {
			_, err := p.Children(ctx, creds, tokens, value, "folder-1")
			return err
		}},
		{name: "Item item", call: func(p *Provider, value string) error {
			_, err := p.Item(ctx, creds, tokens, "drive-1", value)
			return err
		}},
		{name: "Item drive", allowsEmpty: true, call: func(p *Provider, value string) error {
			_, err := p.Item(ctx, creds, tokens, value, "item-1")
			return err
		}},
		{name: "Download item", call: func(p *Provider, value string) error {
			transfer, err := p.Download(ctx, creds, tokens, "drive-1", value)
			if transfer != nil && transfer.Body != nil {
				_ = transfer.Body.Close()
			}
			return err
		}},
		{name: "Download drive", allowsEmpty: true, call: func(p *Provider, value string) error {
			transfer, err := p.Download(ctx, creds, tokens, value, "item-1")
			if transfer != nil && transfer.Body != nil {
				_ = transfer.Body.Close()
			}
			return err
		}},
		{name: "Delete item", call: func(p *Provider, value string) error {
			return p.Delete(ctx, creds, tokens, "drive-1", value)
		}},
		{name: "Delete drive", allowsEmpty: true, call: func(p *Provider, value string) error {
			return p.Delete(ctx, creds, tokens, value, "item-1")
		}},
		{name: "Upload existing item", allowsEmpty: true, call: func(p *Provider, value string) error {
			_, err := p.Upload(ctx, creds, tokens, providers.UploadRequest{
				DriveID: "drive-1", ParentID: "parent-1", ExistingID: value, Name: "note.txt",
			})
			return err
		}},
		{name: "Upload parent", allowsEmpty: true, call: func(p *Provider, value string) error {
			_, err := p.Upload(ctx, creds, tokens, providers.UploadRequest{
				DriveID: "drive-1", ParentID: value, Name: "note.txt",
			})
			return err
		}},
		{name: "Upload drive", allowsEmpty: true, call: func(p *Provider, value string) error {
			_, err := p.Upload(ctx, creds, tokens, providers.UploadRequest{
				DriveID: value, ParentID: "parent-1", Name: "note.txt",
			})
			return err
		}},
		{name: "CreateFolder parent", allowsEmpty: true, call: func(p *Provider, value string) error {
			_, err := p.CreateFolder(ctx, creds, tokens, "drive-1", value, "sub")
			return err
		}},
		{name: "CreateFolder drive", allowsEmpty: true, call: func(p *Provider, value string) error {
			_, err := p.CreateFolder(ctx, creds, tokens, value, "parent-1", "sub")
			return err
		}},
	}
}

// TestProviderRefusesValuesThatAreNotDriveIdentifiers pins the guard of every
// entry point: a value that is not a Drive token is refused with
// providers.ErrInvalidIdentifier and no request is issued for it.
func TestProviderRefusesValuesThatAreNotDriveIdentifiers(t *testing.T) {
	for _, call := range idCalls(context.Background()) {
		for _, value := range forbiddenValues {
			if value == "" && call.allowsEmpty {
				continue
			}
			transport := &recordingTransport{}
			err := call.call(testProvider(transport), value)
			if !errors.Is(err, providers.ErrInvalidIdentifier) {
				t.Errorf("%s(%q) = %v, want providers.ErrInvalidIdentifier", call.name, value, err)
			}
			if len(transport.targets) != 0 {
				t.Errorf("%s(%q) reached the network: %v", call.name, value, transport.targets)
			}
		}
	}
}

// TestProviderAcceptsDriveIdentifiers is the other half of the contract: a token
// Drive itself hands out — and the synthetic ids Sync adds — still reaches
// Google's host with the identifier in its place.
func TestProviderAcceptsDriveIdentifiers(t *testing.T) {
	ctx := context.Background()
	creds := providers.Credentials{}
	tokens := &providers.Tokens{AccessToken: "test-token"}
	const token = "1AbCdEfGh_-2"

	calls := []struct {
		name string
		// wantInTarget is a fragment the request must carry. An empty value only
		// asserts the host: the synthetic drive ids are replaced by the corpora
		// parameters instead of being sent.
		wantInTarget string
		call         func(provider *Provider) error
	}{
		{name: "Children", wantInTarget: token, call: func(p *Provider) error {
			_, err := p.Children(ctx, creds, tokens, token, token)
			return err
		}},
		{name: "Children of My Drive", wantInTarget: "corpora=user", call: func(p *Provider) error {
			_, err := p.Children(ctx, creds, tokens, MyDrive, providers.DriveRoot)
			return err
		}},
		{name: "Item", wantInTarget: "/files/" + token, call: func(p *Provider) error {
			_, err := p.Item(ctx, creds, tokens, token, token)
			return err
		}},
		{name: "Download", wantInTarget: "/files/" + token + "?alt=media", call: func(p *Provider) error {
			transfer, err := p.Download(ctx, creds, tokens, token, token)
			if transfer != nil && transfer.Body != nil {
				_ = transfer.Body.Close()
			}
			return err
		}},
		{name: "Delete", wantInTarget: "/files/" + token, call: func(p *Provider) error {
			return p.Delete(ctx, creds, tokens, token, token)
		}},
		{name: "Upload", wantInTarget: "/files/" + token, call: func(p *Provider) error {
			_, err := p.Upload(ctx, creds, tokens, providers.UploadRequest{
				DriveID: token, ParentID: token, ExistingID: token, Name: "note.txt",
			})
			return err
		}},
		{name: "CreateFolder", wantInTarget: "driveId=" + token, call: func(p *Provider) error {
			_, err := p.CreateFolder(ctx, creds, tokens, token, token, "sub")
			return err
		}},
	}

	for _, call := range calls {
		transport := &recordingTransport{}
		if err := call.call(testProvider(transport)); err != nil {
			t.Errorf("%s = %v, want no error", call.name, err)
			continue
		}
		if len(transport.targets) == 0 {
			t.Errorf("%s issued no request", call.name)
			continue
		}
		for _, target := range transport.targets {
			if !strings.HasPrefix(target, "https://www.googleapis.com/") {
				t.Errorf("%s requested %q, want a googleapis.com URL", call.name, target)
			}
		}
		if call.wantInTarget != "" && !strings.Contains(transport.targets[0], call.wantInTarget) {
			t.Errorf("%s requested %q, want it to carry %q", call.name, transport.targets[0], call.wantInTarget)
		}
	}
}

// TestChildrenOfASharedDriveRootAddressesTheDrive pins the folder picker of a
// Shared Drive: the "root" alias names the My Drive root (Drive documents
// parents.isRoot as true for that folder alone), so the top level of a shared
// drive has to be filtered by the drive id — which is the id of its root folder
// — and the listing must be scoped to that one drive (corpora=drive). Drive
// refuses a listing that carries a driveId without includeItemsFromAllDrives
// (HTTP 403, includeTeamDriveItemsRequired), so that flag must be present.
func TestChildrenOfASharedDriveRootAddressesTheDrive(t *testing.T) {
	ctx := context.Background()
	creds := providers.Credentials{}
	tokens := &providers.Tokens{AccessToken: "test-token"}
	const shared = "shared-drive-1"

	for _, folderID := range []string{"", providers.DriveRoot} {
		transport := &recordingTransport{}
		if _, err := testProvider(transport).Children(ctx, creds, tokens, shared, folderID); err != nil {
			t.Fatalf("Children(%q, %q) = %v, want no error", shared, folderID, err)
		}
		if len(transport.targets) != 1 {
			t.Fatalf("Children(%q, %q) issued %d requests, want 1", shared, folderID, len(transport.targets))
		}
		target, err := url.QueryUnescape(transport.targets[0])
		if err != nil {
			t.Fatalf("cannot decode %q: %v", transport.targets[0], err)
		}
		for _, want := range []string{
			"corpora=drive",
			"driveId=" + shared,
			"includeItemsFromAllDrives=true",
			"'" + shared + "' in parents",
		} {
			if !strings.Contains(target, want) {
				t.Errorf("Children(%q, %q) requested %q, want it to carry %q", shared, folderID, transport.targets[0], want)
			}
		}
		for _, unwanted := range []string{"'root' in parents", "corpora=user"} {
			if strings.Contains(target, unwanted) {
				t.Errorf("Children(%q, %q) requested %q, want no %q", shared, folderID, transport.targets[0], unwanted)
			}
		}
	}
}

// TestChildrenOfMyDriveDoesNotWidenToSharedDrives is the other half of the same
// contract: the My Drive listing selects the user corpus and must not carry the
// shared-drive flags (includeItemsFromAllDrives widens an unscoped listing to the
// shared drives, which is exactly the leak the shared-drive fix removed).
func TestChildrenOfMyDriveDoesNotWidenToSharedDrives(t *testing.T) {
	ctx := context.Background()
	creds := providers.Credentials{}
	tokens := &providers.Tokens{AccessToken: "test-token"}

	for _, driveID := range []string{"", providers.DriveRoot, MyDrive} {
		transport := &recordingTransport{}
		if _, err := testProvider(transport).Children(ctx, creds, tokens, driveID, providers.DriveRoot); err != nil {
			t.Fatalf("Children(%q, root) = %v, want no error", driveID, err)
		}
		if len(transport.targets) != 1 {
			t.Fatalf("Children(%q, root) issued %d requests, want 1", driveID, len(transport.targets))
		}
		target, err := url.QueryUnescape(transport.targets[0])
		if err != nil {
			t.Fatalf("cannot decode %q: %v", transport.targets[0], err)
		}
		if !strings.Contains(target, "corpora=user") {
			t.Errorf("Children(%q, root) requested %q, want it to carry corpora=user", driveID, transport.targets[0])
		}
		for _, unwanted := range []string{"includeItemsFromAllDrives", "driveId="} {
			if strings.Contains(target, unwanted) {
				t.Errorf("Children(%q, root) requested %q, want no %q", driveID, transport.targets[0], unwanted)
			}
		}
	}
}

// TestWritesParentTheSharedDriveRoot is the write half of the same fix: a job
// that creates into the top level of a shared drive must parent the new item with
// the drive id, because "root" would put it in My Drive.
func TestWritesParentTheSharedDriveRoot(t *testing.T) {
	ctx := context.Background()
	creds := providers.Credentials{}
	tokens := &providers.Tokens{AccessToken: "test-token"}
	const shared = "shared-drive-1"
	parents := `"parents":["` + shared + `"]`

	transport := &recordingTransport{}
	if _, err := testProvider(transport).CreateFolder(ctx, creds, tokens, shared, providers.DriveRoot, "sub"); err != nil {
		t.Fatalf("CreateFolder(shared drive root) = %v, want no error", err)
	}
	if len(transport.bodies) != 1 || !strings.Contains(transport.bodies[0], parents) {
		t.Errorf("CreateFolder(shared drive root) sent %v, want the drive id as the parent", transport.bodies)
	}

	transport = &recordingTransport{}
	if _, err := testProvider(transport).Upload(ctx, creds, tokens, providers.UploadRequest{
		DriveID: shared, ParentID: providers.DriveRoot, Name: "note.txt", Body: strings.NewReader("hi"),
	}); err != nil {
		t.Fatalf("Upload(shared drive root) = %v, want no error", err)
	}
	if len(transport.bodies) == 0 || !strings.Contains(transport.bodies[0], parents) {
		t.Errorf("Upload(shared drive root) sent %v, want the drive id as the parent", transport.bodies)
	}
}

// TestRootParentLeavesTheOtherRootsAlone is the guard rail of the helper: only a
// real shared drive replaces the alias, so My Drive and a nested folder keep the
// id they were given.
func TestRootParentLeavesTheOtherRootsAlone(t *testing.T) {
	cases := []struct {
		name     string
		driveID  string
		folderID string
		want     string
	}{
		{name: "shared drive root", driveID: "shared-drive-1", folderID: "root", want: "shared-drive-1"},
		{name: "My Drive by token", driveID: MyDrive, folderID: "root", want: "root"},
		{name: "My Drive by alias", driveID: providers.DriveRoot, folderID: "root", want: "root"},
		{name: "no drive", driveID: "", folderID: "root", want: "root"},
		{name: "nested folder of a shared drive", driveID: "shared-drive-1", folderID: "1AbCdEfGh_-2", want: "1AbCdEfGh_-2"},
		{name: "no folder", driveID: "shared-drive-1", folderID: "", want: ""},
	}
	for _, testCase := range cases {
		if got := rootParent(testCase.driveID, testCase.folderID); got != testCase.want {
			t.Errorf("rootParent(%q, %q) = %q, want %q", testCase.driveID, testCase.folderID, got, testCase.want)
		}
	}
}

// forbiddenURLs are request URLs this provider must never send: another host,
// a host that only ends in the same letters, plain http, a port, credentials in
// the authority part, a relative URL, a backslash, and a value carrying a
// newline (which would split the request line).
var forbiddenURLs = []string{
	"",
	"evil/drive/v3/files",
	"//www.googleapis.com/drive/v3/files",
	"http://www.googleapis.com/drive/v3/files",
	"https://evil.example/drive/v3/files",
	"https://www.googleapis.com.evil.example/drive/v3/files",
	"https://notgoogleapis.com/drive/v3/files",
	"https://www.googleapis.com@evil.example/drive/v3/files",
	"https://www.googleapis.com:8443/drive/v3/files",
	"https://metadata.google.internal/computeMetadata/v1/",
	`https://www.googleapis.com/drive/v3/files\..\permissions`,
	"https://www.googleapis.com/drive/v3/files\nHost: evil.example",
	"https://www.googleapis.com/drive/v3/files\r\nHost: evil.example",
}

// allowedURLs are the shapes the provider itself builds.
var allowedURLs = []string{
	apiBase + "/files",
	apiBase + "/files?fields=id%2Cname&supportsAllDrives=true",
	apiBase + "/files/1AbCdEfGh_-2?alt=media&supportsAllDrives=true",
	apiBase + "/drives?corpora=drive&driveId=1AbCdEfGh_-2",
	uploadBase + "/files/1AbCdEfGh_-2?uploadType=resumable&upload_id=session-1",
	userInfoURL,
}

// TestProviderRefusesRequestsOutsideTheDriveAPIHost pins the second layer of the
// guard: even when a function builds an URL of its own, a request to anything
// but the Drive API host is refused before it is sent.
func TestProviderRefusesRequestsOutsideTheDriveAPIHost(t *testing.T) {
	for _, target := range forbiddenURLs {
		transport := &recordingTransport{}
		err := testProvider(transport).doJSON(context.Background(), &http.Client{Transport: transport}, http.MethodGet, target, nil, nil)
		if !errors.Is(err, providers.ErrInvalidIdentifier) {
			t.Errorf("doJSON(%q) = %v, want providers.ErrInvalidIdentifier", target, err)
		}
		if len(transport.targets) != 0 {
			t.Errorf("doJSON(%q) reached the network: %v", target, transport.targets)
		}
	}
	for _, target := range allowedURLs {
		transport := &recordingTransport{}
		err := testProvider(transport).doJSON(context.Background(), &http.Client{Transport: transport}, http.MethodGet, target, nil, nil)
		if err != nil {
			t.Errorf("doJSON(%q) = %v, want no error", target, err)
		}
		if len(transport.targets) != 1 {
			t.Errorf("doJSON(%q) issued %d requests, want 1", target, len(transport.targets))
		}
	}
}

// testProvider builds a provider whose transport is the given one, so a test
// never reaches the network.
func testProvider(transport http.RoundTripper) *Provider {
	return &Provider{httpClient: &http.Client{Transport: transport}}
}

// recordingTransport answers every request with an empty JSON document (and an
// upload session URI, which is what the resumable upload needs next) while
// recording the targets it was asked for: that is what proves whether a value
// was refused before a request was built. The bodies are kept too, so a test can
// assert what a write sent in its metadata document.
type recordingTransport struct {
	targets []string
	bodies  []string
}

// RoundTrip implements http.RoundTripper.
func (t *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.targets = append(t.targets, request.URL.String())
	body := ""
	if request.Body != nil {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		_ = request.Body.Close()
		body = string(payload)
	}
	t.bodies = append(t.bodies, body)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Location", "https://www.googleapis.com/upload/drive/v3/files?uploadType=resumable&upload_id=session-1")
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        header,
		Body:          io.NopCloser(strings.NewReader("{}")),
		ContentLength: 2,
		Request:       request,
	}, nil
}
