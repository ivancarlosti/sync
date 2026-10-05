package microsoft

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	graphmodels "github.com/microsoftgraph/msgraph-sdk-go/models"
	"golang.org/x/oauth2"

	"github.com/ivancarlosti/sync/internal/providers"
)

func TestItemFromDriveItem(t *testing.T) {
	folder := graphmodels.NewDriveItem()
	folder.SetId(ptr("f1"))
	folder.SetName(ptr("docs"))
	folder.SetFolder(graphmodels.NewFolder())

	file := graphmodels.NewDriveItem()
	file.SetId(ptr("i1"))
	file.SetName(ptr("report.pdf"))
	file.SetSize(ptr(int64(2048)))
	file.SetFile(graphmodels.NewFile())
	file.GetFile().SetMimeType(ptr("application/pdf"))
	hashes := graphmodels.NewHashes()
	hashes.SetQuickXorHash(ptr("QUICK"))
	file.GetFile().SetHashes(hashes)

	stamp := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	info := graphmodels.NewFileSystemInfo()
	info.SetLastModifiedDateTime(&stamp)
	file.SetFileSystemInfo(info)

	parent := graphmodels.NewItemReference()
	parent.SetId(ptr("f1"))
	file.SetParentReference(parent)

	got := itemFromDriveItem(file)
	if got.ID != "i1" || got.Name != "report.pdf" || got.IsDir || got.Size != 2048 ||
		got.MimeType != "application/pdf" || got.Hash != "QUICK" || got.ParentID != "f1" {
		t.Fatalf("unexpected file item: %+v", got)
	}
	if !got.ModifiedAt.Equal(stamp) {
		t.Fatalf("modified at = %s, want %s", got.ModifiedAt, stamp)
	}

	dir := itemFromDriveItem(folder)
	if !dir.IsDir || dir.MimeType != directoryMimeType {
		t.Fatalf("unexpected folder item: %+v", dir)
	}
}

func TestHelpers(t *testing.T) {
	if got := normaliseItemID(""); got != providers.DriveRoot {
		t.Fatalf("normaliseItemID(\"\") = %q", got)
	}
	if got := normaliseItemID("abc"); got != "abc" {
		t.Fatalf("normaliseItemID(abc) = %q", got)
	}
	if got := mediaType("application/json; charset=UTF-8"); got != "application/json" {
		t.Fatalf("mediaType = %q", got)
	}
	if got := splitScopes("files.readwrite.all offline_access"); len(got) != 2 {
		t.Fatalf("splitScopes = %v", got)
	}
	if got := normaliseTokenType("bearer"); got != "Bearer" {
		t.Fatalf("normaliseTokenType = %q", got)
	}
}

// TestTokensFromOAuthDecodesTheReportedScopeValue pins the live connection path:
// the `scope` value Entra returns is stored as a list of scopes, so a grant that
// only differs in its encoding still satisfies the capability badges.
func TestTokensFromOAuthDecodesTheReportedScopeValue(t *testing.T) {
	token := (&oauth2.Token{AccessToken: "access"}).WithExtra(map[string]any{
		"scope": "https%3A%2F%2Fgraph.microsoft.com%2FFiles.ReadWrite.All%20openid",
	})
	tokens := tokensFromOAuth(token, "")
	want := []string{"https://graph.microsoft.com/Files.ReadWrite.All", "openid"}
	if strings.Join(tokens.Scopes, " ") != strings.Join(want, " ") {
		t.Fatalf("scopes = %v, want %v", tokens.Scopes, want)
	}
}

func TestDecodeGraphError(t *testing.T) {
	body := `{"error":{"code":"itemNotFound","message":"The resource could not be found."}}`
	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	err := decodeGraphError(response)
	if err == nil {
		t.Fatal("expected an error")
	}
	provider := &Provider{}
	if !provider.IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false", err)
	}
	if provider.IsNotFound(io.EOF) {
		t.Fatal("IsNotFound(EOF) = true")
	}
	if got := err.Error(); !strings.Contains(got, "itemNotFound") {
		t.Fatalf("error = %q", got)
	}
}

func TestStripTokenOnHostChange(t *testing.T) {
	first, _ := http.NewRequest(http.MethodGet, "https://graph.microsoft.com/v1.0/x/content", nil)
	first.Header.Set("Authorization", "Bearer secret")

	same, _ := http.NewRequest(http.MethodGet, "https://graph.microsoft.com/v1.0/y", nil)
	same.Header.Set("Authorization", "Bearer secret")
	if err := stripTokenOnHostChange(same, []*http.Request{first}); err != nil {
		t.Fatalf("same host redirect: %v", err)
	}
	if same.Header.Get("Authorization") == "" {
		t.Fatal("the token was dropped on a same host redirect")
	}

	other, _ := http.NewRequest(http.MethodGet, "https://tenant.sharepoint.com/y", nil)
	other.Header.Set("Authorization", "Bearer secret")
	if err := stripTokenOnHostChange(other, []*http.Request{first}); err != nil {
		t.Fatalf("cross host redirect: %v", err)
	}
	if other.Header.Get("Authorization") != "" {
		t.Fatal("the token survived a cross host redirect")
	}
}

func TestURLs(t *testing.T) {
	got := itemURL("b!a", "i1")
	if got != "https://graph.microsoft.com/v1.0/drives/b!a/items/i1" {
		t.Fatalf("itemURL = %q", got)
	}
	if got := itemURL("d1", "01ABC!def"); !strings.Contains(got, "/items/01ABC!def") {
		t.Fatalf("itemURL escaped a valid id: %q", got)
	}
	content := contentURL("d1", "", "my report.pdf")
	want := "https://graph.microsoft.com/v1.0/drives/d1/items/root:/my%20report.pdf:"
	if content != want {
		t.Fatalf("contentURL = %q, want %q", content, want)
	}
	hostile := contentURL("a/b", "../x", "y/z:?w#1")
	for _, fragment := range []string{"/a%2Fb/", "/..%2Fx:/", "y%2Fz%3A%3Fw%231"} {
		if !strings.Contains(hostile, fragment) {
			t.Fatalf("contentURL = %q, missing %q", hostile, fragment)
		}
	}
}

func TestTenantIDValidation(t *testing.T) {
	if got := tenantID(providers.Credentials{}); got != defaultTenant {
		t.Fatalf("tenantID = %q", got)
	}
	if got := tenantID(providers.Credentials{TenantID: "consumers"}); got != "consumers" {
		t.Fatalf("tenantID = %q", got)
	}
	if got := tenantID(providers.Credentials{TenantID: "evil.example.com"}); got != "evil.example.com" {
		t.Fatalf("a verified domain tenant was rejected: %q", got)
	}
	for _, bad := range []string{"..", "../common", "common/../..", "a b", "tenant%2F", ".hidden", "tenant."} {
		if got := tenantID(providers.Credentials{TenantID: bad}); got != defaultTenant {
			t.Fatalf("tenantID(%q) = %q, want %q", bad, got, defaultTenant)
		}
	}
	if got := tenantID(providers.Credentials{TenantID: "11111111-2222-3333-4444-555555555555"}); got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("tenantID with a uuid was rejected: %q", got)
	}
}

func TestConsentTenantKeepsASingleDirectory(t *testing.T) {
	for _, generic := range []string{"", "common", "consumers"} {
		if got := consentTenant(providers.Credentials{TenantID: generic}); got != organizationsTenant {
			t.Fatalf("consentTenant(%q) = %q, want %q", generic, got, organizationsTenant)
		}
	}
	for _, single := range []string{"contoso.onmicrosoft.com", "11111111-2222-3333-4444-555555555555"} {
		if got := consentTenant(providers.Credentials{TenantID: single}); got != single {
			t.Fatalf("consentTenant(%q) = %q, want it unchanged", single, got)
		}
	}
}

func TestGraphClientNeedsAToken(t *testing.T) {
	provider := New()
	if _, _, err := provider.graphSession(&providers.Tokens{}); err == nil {
		t.Fatal("expected an error for an empty token")
	}
	if _, _, err := provider.graphSession(nil); err == nil {
		t.Fatal("expected an error for a nil token set")
	}
	client, adapter, err := provider.graphSession(&providers.Tokens{AccessToken: "token"})
	if err != nil || client == nil || adapter == nil {
		t.Fatalf("graphSession = %v, %v, %v", client, adapter, err)
	}
	if _, ok := provider.adapters.Load("token"); !ok {
		t.Fatal("the adapter was not cached")
	}
	if _, err := provider.Account(context.Background(), providers.Credentials{}, &providers.Tokens{}); err == nil {
		t.Fatal("expected Account to fail without a token")
	}
}

// TestGraphRewritesTheMeSentinel pins the request adapter's middleware pipeline.
// The generated client builds Me() as /users/me-token-to-replace, and Graph's URL
// replace handler is the only thing that turns that sentinel into /me. Losing the
// handler is invisible to the compiler and only surfaces as a Graph "resource
// does not exist" error, so the outgoing URL is asserted here instead.
func TestGraphRewritesTheMeSentinel(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{
		"/v1.0/me":        `{"id":"user-1","displayName":"Ada Lovelace","userPrincipalName":"ada@contoso.com"}`,
		"/v1.0/me/drives": `{"value":[{"id":"drive-1","name":"OneDrive","driveType":"personal"}]}`,
	}}
	provider := New()
	provider.httpClient.Transport = transport
	tokens := &providers.Tokens{AccessToken: "sentinel-test-token"}

	account, err := provider.Account(context.Background(), providers.Credentials{}, tokens)
	if err != nil {
		t.Fatalf("Account = %v", err)
	}
	if account.ID != "user-1" || account.Name != "Ada Lovelace" || account.Email != "ada@contoso.com" {
		t.Errorf("Account = %+v, want the document served for /me", account)
	}

	drives, err := provider.Drives(context.Background(), providers.Credentials{}, tokens)
	if err != nil {
		t.Fatalf("Drives = %v", err)
	}
	if len(drives) != 1 || drives[0].ID != "drive-1" || drives[0].Kind != "personal" {
		t.Errorf("Drives = %+v, want the document served for /me/drives", drives)
	}

	want := []string{
		"https://graph.microsoft.com/v1.0/me",
		"https://graph.microsoft.com/v1.0/me/drives",
	}
	if len(transport.urls) != len(want) {
		t.Fatalf("the pipeline issued %d requests (%v), want %d", len(transport.urls), transport.urls, len(want))
	}
	for i, url := range transport.urls {
		if !strings.HasPrefix(url, want[i]) {
			t.Errorf("request %d went to %q, want %q", i, url, want[i])
		}
		if strings.Contains(url, "me-token-to-replace") {
			t.Errorf("request %d leaked the graph sentinel: %q", i, url)
		}
	}
	if got := transport.headers[0].Get("Authorization"); got != "Bearer sentinel-test-token" {
		t.Errorf("Authorization = %q, want the stored access token", got)
	}
}

// recordingTransport answers every request from a canned body per path while
// recording the URLs, so a test never reaches Microsoft Graph.
type recordingTransport struct {
	urls    []string
	headers []http.Header
	bodies  map[string]string
}

// RoundTrip implements http.RoundTripper.
func (t *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.urls = append(t.urls, request.URL.String())
	t.headers = append(t.headers, request.Header.Clone())
	body := t.bodies[request.URL.Path]
	if body == "" {
		body = "{}"
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}
