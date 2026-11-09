package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
)

// testServiceAccount answers a key whose private part is a real (throwaway) RSA
// key, so an assertion can be signed and verified the way the token endpoint
// would.
func testServiceAccount(t *testing.T) serviceAccountKey {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating the test key: %v", err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("encoding the test key: %v", err)
	}
	return serviceAccountKey{
		PrivateKeyID: "key-of-the-test",
		PrivateKey:   string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})),
		ClientEmail:  "report@project.iam.gserviceaccount.com",
		TokenURI:     googleTokenURL,
	}
}

// segment decodes one base64url segment of a JWT into its JSON object.
func segment(t *testing.T, value string) map[string]any {
	t.Helper()
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("decoding a token segment: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("the token segment is not JSON: %v", err)
	}
	return decoded
}

// TestSignAssertionImpersonatesTheSubject is the contract of a domain-wide
// delegation grant: the service account issues the assertion, the administrator is
// impersonated, the report scopes travel with it and the signature verifies with
// the public half of the key.
func TestSignAssertionImpersonatesTheSubject(t *testing.T) {
	key := testServiceAccount(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	scopes := []string{"scope.one", "scope.two"}

	assertion, err := signAssertion(key, "admin@example.com", scopes, googleTokenURL, now)
	if err != nil {
		t.Fatalf("signing the assertion: %v", err)
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d segments, want 3", len(parts))
	}

	header := segment(t, parts[0])
	if header["alg"] != "RS256" || header["typ"] != "JWT" || header["kid"] != key.PrivateKeyID {
		t.Fatalf("header = %v, want alg RS256, typ JWT and the key id", header)
	}
	claims := segment(t, parts[1])
	for field, want := range map[string]string{
		"iss":   key.ClientEmail,
		"sub":   "admin@example.com",
		"scope": "scope.one scope.two",
		"aud":   googleTokenURL,
	} {
		if got, _ := claims[field].(string); got != want {
			t.Fatalf("claims[%q] = %q, want %q", field, got, want)
		}
	}
	if got, _ := claims["iat"].(float64); int64(got) != now.Unix() {
		t.Fatalf("claims[iat] = %v, want %d", claims["iat"], now.Unix())
	}
	if got, _ := claims["exp"].(float64); int64(got) != now.Add(time.Hour).Unix() {
		t.Fatalf("claims[exp] = %v, want %d", claims["exp"], now.Add(time.Hour).Unix())
	}

	signer, err := privateKeyOf(key.PrivateKey)
	if err != nil {
		t.Fatalf("parsing the test key back: %v", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decoding the signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&signer.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("the signature does not verify: %v", err)
	}
}

// TestReadServiceAccountRefusesAKeyThatCannotSign keeps the three shapes of a bad
// key at three messages: a path that does not exist, a document that is not JSON,
// and a key without a private part (a workload identity pool key).
func TestReadServiceAccountRefusesAKeyThatCannotSign(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(valid, []byte(`{"client_email":"a@b.c","private_key":"x"}`), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	notJSON := filepath.Join(dir, "not-json.json")
	if err := os.WriteFile(notJSON, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	noKey := filepath.Join(dir, "no-key.json")
	if err := os.WriteFile(noKey, []byte(`{"client_email":"a@b.c"}`), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	if key, err := readServiceAccount(valid); err != nil || key.ClientEmail != "a@b.c" {
		t.Fatalf("readServiceAccount(valid) = %+v, %v", key, err)
	}
	for name, path := range map[string]string{
		"missing":  filepath.Join(dir, "absent.json"),
		"not json": notJSON,
		"no key":   noKey,
	} {
		if _, err := readServiceAccount(path); err == nil {
			t.Fatalf("%s: readServiceAccount answered no error", name)
		}
	}
}

// TestExchangeTokenReadsTheAnswer holds the token endpoint to its two answers: a
// document with the token, and a refusal that must reach the operator with the
// reason it gave (`invalid_grant` is a revoked refresh token, not a bug here).
func TestExchangeTokenReadsTheAnswer(t *testing.T) {
	var form url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parsing the token request: %v", err)
		}
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ya29.of-the-test","expires_in":3600,` +
			`"token_type":"Bearer","scope":"scope.one scope.two"}`))
	}))
	defer server.Close()

	tokens, err := exchangeToken(context.Background(), server.URL, url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {"signed"},
	})
	if err != nil {
		t.Fatalf("exchangeToken: %v", err)
	}
	if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || form.Get("assertion") != "signed" {
		t.Fatalf("the token endpoint received %v", form)
	}
	if tokens.AccessToken != "ya29.of-the-test" || tokens.TokenType != "Bearer" {
		t.Fatalf("tokens = %+v, want the access token of the answer", tokens)
	}
	if len(tokens.Scopes) != 2 || tokens.Scopes[1] != "scope.two" {
		t.Fatalf("scopes = %v, want the two of the answer", tokens.Scopes)
	}
	if !tokens.Expiry.After(time.Now()) {
		t.Fatalf("expiry = %v, want a token that is still valid", tokens.Expiry)
	}
}

// TestExchangeTokenReportsTheRefusal is the case an operator meets first: the
// grant is refused and the reason has to survive to the message.
func TestExchangeTokenReportsTheRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
	}))
	defer server.Close()

	_, err := exchangeToken(context.Background(), server.URL, url.Values{"grant_type": {"refresh_token"}})
	if err == nil {
		t.Fatal("exchangeToken answered no error for a refused grant")
	}
	if !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("error = %q, want the reason the endpoint gave", err)
	}
}

// TestSectionsOfKeepsTheCollectorsVocabulary: a typo fails here, and the default
// pair of a licence gap is accepted in the order the vocabulary defines.
func TestSectionsOfKeepsTheCollectorsVocabulary(t *testing.T) {
	got, err := sectionsOf("users, licenses")
	if err != nil {
		t.Fatalf("sectionsOf: %v", err)
	}
	if len(got) != 2 || got[0] != providers.ReportSectionUsers || got[1] != providers.ReportSectionLicenses {
		t.Fatalf("sectionsOf = %v, want users then licenses", got)
	}
	if _, err := sectionsOf("users,teleport"); err == nil {
		t.Fatal("sectionsOf accepted a section the vocabulary does not have")
	}
	if _, err := sectionsOf("teams"); err == nil {
		t.Fatal("sectionsOf accepted a section the google collector does not have")
	}
}

// TestLicenceCellsTellsTheThreeAnswersApart is the whole point of the tool: a user
// that holds a licence, a user the API answered with none, and a user whose lookup
// never answered (the empty cells a throttled read leaves) must not be read as the
// same thing.
func TestLicenceCellsTellsTheThreeAnswersApart(t *testing.T) {
	table := providers.ReportTable{
		Section: providers.ReportSectionUsers,
		Columns: []providers.ReportColumn{
			{Key: "primary_email", Title: "Primary email"},
			{Key: "licenses", Title: "Licenses"},
			{Key: "license_count", Title: "License count"},
		},
		Rows: []providers.ReportRow{
			{"adriana@example.com", "Google Workspace Business Standard", "1"},
			{"rogerio@example.com", "", "0"},
			{"anne@example.com", "", ""},
			{"bruno@example.com", "", ""},
		},
	}

	got := licenceCells(table)
	for _, want := range []string{
		"(4 users)",
		"1 hold at least one licence",
		"1 the API answered with none",
		"2 left blank",
		"count 0: rogerio@example.com",
		"blank:   anne@example.com, bruno@example.com",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("licenceCells is missing %q:\n%s", want, got)
		}
	}
}

// TestWriteTablesWritesEveryAnswer holds the two shapes of a section: a table
// becomes the CSV the ZIP carries (BOM, column titles, one row per record) and a
// section that answered with a reason becomes a text file, because that reason is
// what the operator has to read.
func TestWriteTablesWritesEveryAnswer(t *testing.T) {
	dir := t.TempDir()
	tables := []providers.ReportTable{
		{
			Section: providers.ReportSectionUsers,
			Columns: []providers.ReportColumn{
				{Key: "primary_email", Title: "Primary email"},
				{Key: "license_count", Title: "License count"},
			},
			Rows: []providers.ReportRow{
				{"adriana@example.com", "1"},
				{"rogerio@example.com", ""},
			},
			Truncated: true,
		},
		{
			Section: providers.ReportSectionLicenses,
			Error:   "google licensing api: http 403 (subscriptionNotFound): the domain does not answer",
		},
	}
	if err := writeTables(dir, tables); err != nil {
		t.Fatalf("writeTables: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(dir, "users.csv"))
	if err != nil {
		t.Fatalf("reading the users CSV: %v", err)
	}
	if !strings.HasPrefix(string(written), "\xEF\xBB\xBF") {
		t.Fatal("the users CSV does not start with a UTF-8 BOM")
	}
	body := strings.TrimPrefix(string(written), "\xEF\xBB\xBF")
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n")), "\n")
	if len(lines) != 3 {
		t.Fatalf("users CSV has %d lines, want a header and two rows:\n%s", len(lines), body)
	}
	if lines[0] != "Primary email,License count" || lines[1] != "adriana@example.com,1" {
		t.Fatalf("users CSV = %q", lines[:2])
	}
	if lines[2] != "rogerio@example.com," {
		t.Fatalf("the blank licence cell of a known user was not written as empty: %q", lines[2])
	}

	reason, err := os.ReadFile(filepath.Join(dir, "licenses.error.txt"))
	if err != nil {
		t.Fatalf("reading the reason of the failed section: %v", err)
	}
	if !strings.Contains(string(reason), "subscriptionNotFound") {
		t.Fatalf("the reason of the failed section = %q", reason)
	}
	if _, err := os.Stat(filepath.Join(dir, "licenses.csv")); !os.IsNotExist(err) {
		t.Fatal("a failed section must not leave an empty CSV behind")
	}
}
