package microsoft

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	graphmodels "github.com/microsoftgraph/msgraph-sdk-go/models"

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
