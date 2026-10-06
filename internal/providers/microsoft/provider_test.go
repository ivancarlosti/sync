package microsoft

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	graphmodels "github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
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
// recording the URLs, so a test never reaches Microsoft Graph. `statuses` makes a
// path fail with a chosen status, which is what the wildcard fallback needs.
type recordingTransport struct {
	urls     []string
	headers  []http.Header
	bodies   map[string]string
	statuses map[string]int
}

// RoundTrip implements http.RoundTripper.
func (t *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.urls = append(t.urls, request.URL.String())
	t.headers = append(t.headers, request.Header.Clone())
	body := t.bodies[request.URL.Path]
	if body == "" {
		body = "{}"
	}
	status := http.StatusOK
	if code, ok := t.statuses[request.URL.Path]; ok {
		status = code
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

// TestSearchSitesSearchesForTheWildcard pins the fix of the empty SharePoint
// picker: with no keyword the search must reach the delegated `$search=*`
// listing, which returns the sites of the tenant, instead of the application-only
// plain collection that returns at most the root site.
func TestSearchSitesSearchesForTheWildcard(t *testing.T) {
	transport := &recordingTransport{bodies: map[string]string{"/v1.0/sites": `{"value":[]}`}}
	provider := New()
	provider.httpClient.Transport = transport

	if _, err := provider.SearchSites(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "search-token"}, ""); err != nil {
		t.Fatalf("SearchSites = %v", err)
	}
	if len(transport.urls) != 1 {
		t.Fatalf("issued %d requests (%v), want a single listing", len(transport.urls), transport.urls)
	}
	query := transport.urls[0]
	if !strings.Contains(query, "/sites?") || !strings.Contains(query, "search=") {
		t.Errorf("request went to %q, want a $search listing", query)
	}
	if !strings.Contains(query, "%2A") && !strings.Contains(query, "*") {
		t.Errorf("request went to %q, want the wildcard query", query)
	}
}

// TestSearchSitesFallsBackToTheCollection covers the tenant that refuses the
// wildcard: the listing is retried without $search (the tenant root site) instead
// of emptying the picker, and only a double failure surfaces an error.
func TestSearchSitesFallsBackToTheCollection(t *testing.T) {
	transport := &recordingTransport{statuses: map[string]int{"/v1.0/sites": http.StatusBadRequest}}
	provider := New()
	provider.httpClient.Transport = transport

	if _, err := provider.SearchSites(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "search-token"}, ""); err == nil {
		t.Fatal("expected the double failure to surface")
	}
	if len(transport.urls) != 2 {
		t.Fatalf("issued %d requests (%v), want the wildcard then the plain listing", len(transport.urls), transport.urls)
	}
	if strings.Contains(transport.urls[0], "search=") && strings.Contains(transport.urls[1], "search=") {
		t.Errorf("requests %v, want the fallback to drop $search", transport.urls)
	}
}

// TestSiteIDFromLocation pins the normalisation behind the manual SharePoint URL
// field: every form an operator may paste becomes the composite site id of
// GET /sites/{id}, and anything that is not a location is refused as an invalid
// identifier (which the API answers as 400).
func TestSiteIDFromLocation(t *testing.T) {
	valid := map[string]string{
		"https://contoso.sharepoint.com/sites/marketing":                                  "contoso.sharepoint.com:/sites/marketing",
		"https://contoso.sharepoint.com/sites/marketing/":                                 "contoso.sharepoint.com:/sites/marketing",
		"https://contoso.sharepoint.com/sites/Marketing%20Team/Docs":                      "contoso.sharepoint.com:/sites/Marketing%20Team",
		"https://contoso.sharepoint.com/teams/eng/Shared%20Documents/Forms/AllItems.aspx": "contoso.sharepoint.com:/teams/eng",
		"https://contoso.sharepoint.com":                                                  "contoso.sharepoint.com",
		"contoso.sharepoint.com:/sites/marketing":                                         "contoso.sharepoint.com:/sites/marketing",
		"contoso.sharepoint.com,g1,g2":                                                    "contoso.sharepoint.com,g1,g2",
		"contoso.sharepoint.com":                                                          "contoso.sharepoint.com",
	}
	for raw, want := range valid {
		got, err := siteIDFromLocation(raw)
		if err != nil || got != want {
			t.Errorf("siteIDFromLocation(%q) = %q, %v, want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "marketing", "not a url", "https://"} {
		if _, err := siteIDFromLocation(raw); !errors.Is(err, providers.ErrInvalidIdentifier) {
			t.Errorf("siteIDFromLocation(%q) error = %v, want ErrInvalidIdentifier", raw, err)
		}
	}
}

// TestResolveSiteReadsTheDefaultLibrary drives the manual fallback through the
// real Graph request builders behind a canned transport: the pasted URL becomes
// the composite site id of GET /sites/{id}, and the default library of that site
// is what comes back, labelled for the picker.
func TestResolveSiteReadsTheDefaultLibrary(t *testing.T) {
	const siteID = "contoso.sharepoint.com,aaa-111,bbb-222"
	transport := &recordingTransport{bodies: map[string]string{
		"/v1.0/sites/contoso.sharepoint.com:/sites/marketing": `{"id":"` + siteID + `","displayName":"Marketing","webUrl":"https://contoso.sharepoint.com/sites/marketing"}`,
		"/v1.0/sites/" + siteID + "/drive":                    `{"id":"b!library","name":"Documents","driveType":"documentLibrary"}`,
	}}
	provider := New()
	provider.httpClient.Transport = transport

	drive, err := provider.ResolveSite(context.Background(), providers.Credentials{},
		&providers.Tokens{AccessToken: "resolve-token"}, "https://contoso.sharepoint.com/sites/marketing/Documents")
	if err != nil {
		t.Fatalf("ResolveSite = %v", err)
	}
	if drive.ID != "b!library" || drive.Kind != "site" || drive.Name != "Marketing" ||
		drive.Owner != "https://contoso.sharepoint.com/sites/marketing" {
		t.Errorf("drive = %+v, want the site's default library", drive)
	}
	want := "https://graph.microsoft.com/v1.0/sites/contoso.sharepoint.com:/sites/marketing"
	if len(transport.urls) != 2 {
		t.Fatalf("issued %d requests (%v), want the site then its library", len(transport.urls), transport.urls)
	}
	// Kiota percent-encodes the composite site id and Graph decodes it back, so
	// the decoded path is what has to name the site.
	target, err := url.Parse(transport.urls[0])
	if err != nil {
		t.Fatalf("parsing the first request URL: %v", err)
	}
	if got := "https://graph.microsoft.com" + target.Path; got != want {
		t.Errorf("first request went to %q, want %q", transport.urls[0], want)
	}
	if !strings.Contains(target.RawQuery, "select") {
		t.Errorf("first request carried %q, want the site projection", target.RawQuery)
	}
}

// TestIsUnauthorized pins the detection of a rejected access token: the raw
// transfer path keeps the 401 status, the SDK path exposes it through the Graph
// error code, and a permission failure is never mistaken for a dead token.
func TestIsUnauthorized(t *testing.T) {
	provider := New()
	if !provider.IsUnauthorized(&graphError{Status: http.StatusUnauthorized}) {
		t.Error("IsUnauthorized must accept the raw 401")
	}
	if provider.IsUnauthorized(&graphError{Status: http.StatusForbidden}) {
		t.Error("IsUnauthorized must not treat a 403 as a dead token")
	}

	odata := odataerrors.NewODataError()
	main := odataerrors.NewMainError()
	main.SetCode(ptr("InvalidAuthenticationToken"))
	odata.SetErrorEscaped(main)
	if !provider.IsUnauthorized(odata) {
		t.Error("IsUnauthorized must accept the SDK InvalidAuthenticationToken")
	}

	denied := odataerrors.NewODataError()
	deniedMain := odataerrors.NewMainError()
	deniedMain.SetCode(ptr("accessDenied"))
	denied.SetErrorEscaped(deniedMain)
	if provider.IsUnauthorized(denied) {
		t.Error("IsUnauthorized must not treat accessDenied as a dead token")
	}
	if provider.IsUnauthorized(nil) {
		t.Error("IsUnauthorized(nil) = true, want false")
	}
}
