// Command reportprobe runs one Google Workspace directory report against a real
// tenant, without Sync's database, OAuth connection or web server.
//
// A directory report is the hardest thing Sync asks of the Admin SDK and of the
// Enterprise License Manager API: the licence column alone is a walk over the whole
// directory, one batch of per-user lookups per user, and when a section comes back
// short there is nowhere to look. The run row stores counters, the CSV stores the
// answer, and the reason a user's licence cell is empty is a log line an operator
// reads hours later. This command runs the collector itself
// (google.Provider.CollectReport, the call ReportService makes) with a token minted
// here, writes every section as the CSV the ZIP carries, and prints what the two
// licence columns of the users section actually say: how many users hold a licence,
// how many the API answered with none, and how many it never answered for at all.
//
// It is a development tool, not a second product. It holds no state, it is not part
// of the image (the Dockerfile builds ./cmd/server), it never writes a token
// anywhere, and it deletes nothing.
//
//	# the scopes a domain-wide delegation grant must list
//	go run ./cmd/reportprobe -scopes
//
//	# a service account with domain-wide delegation, impersonating an administrator
//	# (the key of your GAM installation works: put it in .local/service-account.json)
//	go run ./cmd/reportprobe -subject admin@example.com
//
//	# the OAuth client of the connection plus the refresh token it stores
//	go run ./cmd/reportprobe -client-id ... -client-secret ... -refresh-token ...
//
//	# a token that was minted elsewhere (gcloud auth print-access-token)
//	go run ./cmd/reportprobe -access-token "$(gcloud auth print-access-token)"
//
// The sections default to users,licenses — the two a licence gap needs — and the
// CSVs land in .local/report.
package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/providers"
	"github.com/ivancarlosti/sync/internal/providers/google"
)

// googleTokenURL is Google's OAuth 2.0 token endpoint, the value the
// service-account key of a Google Cloud project carries in its `token_uri`.
const googleTokenURL = "https://oauth2.googleapis.com/token"

// toolClient issues the two token calls of this command. The collector has a
// client of its own (google.New), with the transport a report needs.
var toolClient = &http.Client{Timeout: time.Minute}

// options is the command line.
type options struct {
	serviceAccount string
	subject        string
	accessToken    string
	clientID       string
	clientSecret   string
	refreshToken   string
	sections       string
	out            string
	askScopes      string
	timeout        time.Duration
	debug          bool
	scopes         bool
}

func main() {
	opts := options{}
	flag.StringVar(&opts.serviceAccount, "service-account", filepath.Join(".local", "service-account.json"),
		"path of the service-account JSON key the report is read with (domain-wide delegation)")
	flag.StringVar(&opts.subject, "subject", "",
		"administrator address the service account impersonates; required with -service-account")
	flag.StringVar(&opts.accessToken, "access-token", "",
		"use this OAuth access token instead of minting one (gcloud auth print-access-token)")
	flag.StringVar(&opts.clientID, "client-id", "", "OAuth client id, to read a refresh token with")
	flag.StringVar(&opts.clientSecret, "client-secret", "", "OAuth client secret of that client")
	flag.StringVar(&opts.refreshToken, "refresh-token", "",
		"refresh token of the administrator account, instead of a service account")
	flag.StringVar(&opts.askScopes, "ask-scopes", "",
		"comma or space separated scopes to ask for, when the delegation grant authorises "+
			"only some of them (default: every scope of the provider, see -scopes)")
	flag.StringVar(&opts.sections, "sections", "users,licenses", "comma separated sections to collect")
	flag.StringVar(&opts.out, "out", filepath.Join(".local", "report"),
		"directory the section CSVs and the summary are written to")
	flag.DurationVar(&opts.timeout, "timeout", 30*time.Minute, "bound of the whole run")
	flag.BoolVar(&opts.debug, "debug", false,
		"log every part the collector reads, at debug level (what a licence gap needs)")
	flag.BoolVar(&opts.scopes, "scopes", false, "print the OAuth scopes a grant needs and exit")
	flag.Parse()

	level := slog.LevelInfo
	if opts.debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if opts.scopes {
		fmt.Println(strings.Join(google.New().Scopes(), " "))
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	if err := run(ctx, opts); err != nil {
		fmt.Fprintln(os.Stderr, describe(err))
		os.Exit(1)
	}
}

// describe prefixes an error with the command name once, whichever layer built the
// message: a failure of the report itself is already prefixed, a plain one from the
// standard library is not.
func describe(err error) string {
	message := err.Error()
	if strings.HasPrefix(message, "reportprobe: ") {
		return message
	}
	return "reportprobe: " + message
}

// run reads the sections with a token of the credentials the caller gave, writes
// them next to the summary and prints the summary.
func run(ctx context.Context, opts options) error {
	sections, err := sectionsOf(opts.sections)
	if err != nil {
		return err
	}
	tokens, err := token(ctx, opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(opts.out, 0o750); err != nil {
		return fmt.Errorf("reportprobe: cannot create %s: %w", opts.out, err)
	}

	names := make([]string, 0, len(sections))
	for _, section := range sections {
		names = append(names, string(section))
	}
	slog.Info("collecting the report", "sections", strings.Join(names, ","), "out", opts.out)

	started := time.Now()
	tables, err := google.New().CollectReport(ctx, providers.Credentials{}, tokens, sections,
		func(progress providers.ReportProgress) {
			slog.Debug("collecting", "section", string(progress.Section), "rows", progress.Rows)
		})
	elapsed := time.Since(started)
	if err != nil {
		return fmt.Errorf("reportprobe: the report failed after %s: %w", elapsed.Round(time.Millisecond), err)
	}
	if err := writeTables(opts.out, tables); err != nil {
		return err
	}

	summary := summarise(tables, elapsed)
	fmt.Println(summary)
	path := filepath.Join(opts.out, "summary.txt")
	if err := os.WriteFile(path, []byte(summary), 0o600); err != nil {
		return fmt.Errorf("reportprobe: cannot write %s: %w", path, err)
	}
	return nil
}

// sectionsOf validates the requested sections against the ones the Google
// collector has, so a typo fails here instead of as a missing CSV.
func sectionsOf(raw string) ([]providers.ReportSection, error) {
	requested, err := providers.ReportSectionsFrom(strings.Split(raw, ","))
	if err != nil {
		return nil, fmt.Errorf("reportprobe: %w", err)
	}
	allowed := make(map[providers.ReportSection]bool, len(providers.ReportSections))
	for _, section := range google.New().ReportSections() {
		allowed[section] = true
	}
	chosen := make([]providers.ReportSection, 0, len(requested))
	for _, section := range requested {
		if !allowed[section] {
			return nil, fmt.Errorf("reportprobe: the google collector has no %q section", section)
		}
		chosen = append(chosen, section)
	}
	return chosen, nil
}

// askScopes answers the scopes the delegated token is asked for: the ones given on
// the command line when a grant authorises a subset of them, and every scope of the
// provider otherwise (the set a connection is made with). The token endpoint refuses
// the whole exchange when one asked scope is not part of the domain-wide delegation
// grant, so a grant narrower than the provider needs `-ask-scopes`.
func askScopes(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(fields) == 0 {
		return google.New().Scopes()
	}
	return fields
}

// token answers the access token the report is read with, from whichever
// credential the caller gave: a service account with domain-wide delegation (the
// GAM arrangement), the OAuth client and refresh token a connection stores, or a
// token minted elsewhere. An access token given on the command line is used as it
// is, because it is the one credential here with no other source.
func token(ctx context.Context, opts options) (*providers.Tokens, error) {
	if value := strings.TrimSpace(opts.accessToken); value != "" {
		slog.Info("using the access token given on the command line")
		return &providers.Tokens{AccessToken: value, TokenType: "Bearer"}, nil
	}
	if strings.TrimSpace(opts.refreshToken) != "" {
		return tokenFromRefresh(ctx, opts)
	}
	return tokenFromServiceAccount(ctx, opts)
}

// serviceAccountKey is the part of a service-account JSON key this command reads.
// It is a subset of what Google Cloud hands out on purpose: the project id, the
// client id and the rest of the document are not needed to sign an assertion.
type serviceAccountKey struct {
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
}

// audience answers the endpoint the assertion is signed for, which is the
// `token_uri` of the key (Google rotates it per project).
func (k serviceAccountKey) audience() string {
	if tokenURI := strings.TrimSpace(k.TokenURI); tokenURI != "" {
		return tokenURI
	}
	return googleTokenURL
}

// tokenFromServiceAccount mints a delegated access token: the assertion names the
// administrator in `sub` and the scopes of a directory report, which is exactly
// what an operator authorises in Admin console > Security > API controls >
// Domain-wide delegation for the client id of this service account.
func tokenFromServiceAccount(ctx context.Context, opts options) (*providers.Tokens, error) {
	key, err := readServiceAccount(opts.serviceAccount)
	if err != nil {
		return nil, err
	}
	subject := strings.TrimSpace(opts.subject)
	if subject == "" {
		return nil, fmt.Errorf("reportprobe: -subject is the administrator the service account impersonates " +
			"(the Admin SDK answers a bare service account with nothing)")
	}
	assertion, err := signAssertion(key, subject, askScopes(opts.askScopes), key.audience(), time.Now())
	if err != nil {
		return nil, err
	}
	slog.Info("asking for a delegated access token",
		"service_account", key.ClientEmail, "subject", subject,
		"scopes", strings.Join(askScopes(opts.askScopes), " "))
	return exchangeToken(ctx, key.audience(), url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	})
}

// tokenFromRefresh reads the token of a connection: the refresh token of an
// administrator plus the OAuth client that issued it.
func tokenFromRefresh(ctx context.Context, opts options) (*providers.Tokens, error) {
	if strings.TrimSpace(opts.clientID) == "" || strings.TrimSpace(opts.clientSecret) == "" {
		return nil, fmt.Errorf("reportprobe: -refresh-token needs the -client-id and -client-secret that issued it")
	}
	slog.Info("refreshing the access token of the connection")
	return exchangeToken(ctx, googleTokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {strings.TrimSpace(opts.clientID)},
		"client_secret": {strings.TrimSpace(opts.clientSecret)},
		"refresh_token": {strings.TrimSpace(opts.refreshToken)},
	})
}

// readServiceAccount reads the JSON key from disk and refuses one that cannot
// sign an assertion, which is the mistake worth its own message (a key of a
// workload identity pool has no private_key).
func readServiceAccount(path string) (serviceAccountKey, error) {
	var key serviceAccountKey
	payload, err := os.ReadFile(path)
	if err != nil {
		return key, fmt.Errorf("reportprobe: cannot read the service-account key of %s: %w "+
			"(put it there, or pass -service-account)", path, err)
	}
	if err := json.Unmarshal(payload, &key); err != nil {
		return key, fmt.Errorf("reportprobe: %s is not a service-account JSON key: %w", path, err)
	}
	if strings.TrimSpace(key.ClientEmail) == "" || strings.TrimSpace(key.PrivateKey) == "" {
		return key, fmt.Errorf("reportprobe: %s carries no client_email/private_key", path)
	}
	return key, nil
}

// signAssertion builds and signs the RS256 JWT of a domain-wide delegation grant.
// The claims are the documented set: the service account issues it (`iss`), the
// administrator is impersonated (`sub`), and the scopes are asked for in one space
// separated string.
func signAssertion(key serviceAccountKey, subject string, scopes []string, audience string, now time.Time) (string, error) {
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	if id := strings.TrimSpace(key.PrivateKeyID); id != "" {
		header["kid"] = id
	}
	claims := map[string]any{
		"iss":   key.ClientEmail,
		"sub":   subject,
		"scope": strings.Join(scopes, " "),
		"aud":   audience,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	signing, err := signingInput(header, claims)
	if err != nil {
		return "", err
	}
	private, err := privateKeyOf(key.PrivateKey)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("reportprobe: cannot sign the token assertion: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// signingInput answers the two encoded segments the signature is computed over.
func signingInput(header, claims map[string]any) (string, error) {
	encodedHeader, err := jsonSegment(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := jsonSegment(claims)
	if err != nil {
		return "", err
	}
	return encodedHeader + "." + encodedClaims, nil
}

// jsonSegment encodes one segment of a JWT: the base64url of its JSON, without
// padding, which is the form the token endpoint expects.
func jsonSegment(value map[string]any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("reportprobe: cannot encode a token assertion segment: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

// privateKeyOf parses the PKCS#8 PEM a service-account key carries.
func privateKeyOf(pemKey string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, fmt.Errorf("reportprobe: the private_key of the service account is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("reportprobe: the private_key of the service account is not PKCS#8: %w", err)
	}
	private, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("reportprobe: the private_key of the service account is not RSA")
	}
	return private, nil
}

// tokenAnswer is the token endpoint's document, answered or refused.
type tokenAnswer struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int    `json:"expires_in"`
	Scope            string `json:"scope"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// exchangeToken posts one grant to the token endpoint and answers the token. A
// refusal is reported with the reason the endpoint gave, which is what tells an
// unauthorised scope from a revoked refresh token.
func exchangeToken(ctx context.Context, endpoint string, form url.Values) (*providers.Tokens, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("reportprobe: cannot build the token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := toolClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("reportprobe: the token endpoint did not answer: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reportprobe: cannot read the token answer: %w", err)
	}
	var answer tokenAnswer
	if err := json.Unmarshal(payload, &answer); err != nil {
		return nil, fmt.Errorf("reportprobe: the token endpoint answered http %d and not a token document",
			response.StatusCode)
	}
	if answer.Error != "" {
		return nil, fmt.Errorf("reportprobe: the token endpoint refused the grant (http %d): %s: %s",
			response.StatusCode, answer.Error, answer.ErrorDescription)
	}
	if answer.AccessToken == "" {
		return nil, fmt.Errorf("reportprobe: the token endpoint answered http %d without an access token",
			response.StatusCode)
	}
	tokens := &providers.Tokens{
		AccessToken: answer.AccessToken,
		TokenType:   answer.TokenType,
		Scopes:      providers.SplitScopes(answer.Scope),
	}
	if answer.ExpiresIn > 0 {
		tokens.Expiry = time.Now().Add(time.Duration(answer.ExpiresIn) * time.Second)
	}
	return tokens, nil
}

// writeTables writes one CSV per section, and a text file for a section that
// answered with a reason instead of a table. A failed section is not fatal: a
// directory report is read for the sections that did answer, and the reason of the
// one that did not is exactly what the operator needs (a refused scope, a domain
// that does not have the API).
func writeTables(dir string, tables []providers.ReportTable) error {
	for _, table := range tables {
		if table.Failed() {
			path := filepath.Join(dir, string(table.Section)+".error.txt")
			if err := os.WriteFile(path, []byte(table.Error+"\n"), 0o600); err != nil {
				return fmt.Errorf("reportprobe: cannot write %s: %w", path, err)
			}
			slog.Warn("a section answered with a reason instead of a table",
				"section", table.Section, "error", table.Error)
			continue
		}
		if err := writeTable(dir, table); err != nil {
			return err
		}
	}
	return nil
}

// writeTable writes one section as the CSV the ZIP carries: the UTF-8 BOM that
// makes a spreadsheet open an accented name correctly, the column titles as the
// header, and one row per record (see handlers.writeReportCSV).
func writeTable(dir string, table providers.ReportTable) error {
	path := filepath.Join(dir, string(table.Section)+".csv")
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("reportprobe: cannot create %s: %w", path, err)
	}
	defer file.Close()
	if _, err := file.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return fmt.Errorf("reportprobe: cannot write the BOM of %s: %w", path, err)
	}
	out := csv.NewWriter(file)
	header := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		header = append(header, column.Title)
	}
	if err := out.Write(header); err != nil {
		return fmt.Errorf("reportprobe: cannot write the header of %s: %w", path, err)
	}
	for _, row := range table.Rows {
		if err := out.Write(row); err != nil {
			return fmt.Errorf("reportprobe: cannot write a row of %s: %w", path, err)
		}
	}
	out.Flush()
	if err := out.Error(); err != nil {
		return fmt.Errorf("reportprobe: cannot flush %s: %w", path, err)
	}
	slog.Info("section written", "section", table.Section, "rows", len(table.Rows),
		"truncated", table.Truncated, "path", path)
	return nil
}

// summarise answers what the run says about the directory: the size of every
// section, whether it was cut, and — for the users section — the three readings of
// its licence columns.
func summarise(tables []providers.ReportTable, elapsed time.Duration) string {
	var out strings.Builder
	fmt.Fprintf(&out, "google directory report — %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&out, "collected in %s\n\n", elapsed.Round(time.Millisecond))
	for _, table := range tables {
		if table.Failed() {
			fmt.Fprintf(&out, "%-18s failed: %s\n", table.Section, table.Error)
			continue
		}
		note := ""
		if table.Truncated {
			note = " (truncated: the tail of the section is missing)"
		}
		fmt.Fprintf(&out, "%-18s %d rows%s\n", table.Section, len(table.Rows), note)
	}
	for _, table := range tables {
		if table.Section == providers.ReportSectionUsers && !table.Failed() {
			out.WriteString(licenceCells(table))
		}
	}
	return out.String()
}

// licenceCells is the reason this command exists: it reads the two licence columns
// of the users section and tells the three answers apart — the users that hold a
// licence, the users the API answered with none, and the users it never answered
// at all (the empty cells a throttled per-user lookup leaves behind, which is what
// this tool was written to expose).
func licenceCells(table providers.ReportTable) string {
	nameIndex, licenceIndex, countIndex := -1, -1, -1
	for i, column := range table.Columns {
		switch column.Key {
		case "primary_email":
			nameIndex = i
		case "licenses":
			licenceIndex = i
		case "license_count":
			countIndex = i
		}
	}
	if nameIndex < 0 || licenceIndex < 0 || countIndex < 0 {
		return ""
	}
	var holds, none, unknown []string
	for _, row := range table.Rows {
		address := cell(row, nameIndex)
		licences, count := cell(row, licenceIndex), cell(row, countIndex)
		switch {
		case licences == "" && count == "":
			unknown = append(unknown, address)
		case count == "0":
			none = append(none, address)
		default:
			holds = append(holds, address)
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "\nlicence cells of the users section (%d users):\n", len(table.Rows))
	fmt.Fprintf(&out, "  %d hold at least one licence\n", len(holds))
	fmt.Fprintf(&out, "  %d the API answered with none (license count 0)\n", len(none))
	fmt.Fprintf(&out, "  %d left blank: the lookup never answered\n", len(unknown))
	if len(none) > 0 {
		fmt.Fprintf(&out, "  count 0: %s\n", strings.Join(none, ", "))
	}
	if len(unknown) > 0 {
		fmt.Fprintf(&out, "  blank:   %s\n", strings.Join(unknown, ", "))
	}
	return out.String()
}

// cell answers one cell of a row, or the empty string when the row is shorter than
// the header (a stored report can predate a column).
func cell(row providers.ReportRow, index int) string {
	if index < 0 || index >= len(row) {
		return ""
	}
	return row[index]
}
