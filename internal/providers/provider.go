// Package providers defines the storage abstraction Sync synchronises over.
//
// Two implementations exist today (providers/google for Google Drive and
// providers/microsoft for Microsoft Graph / OneDrive / SharePoint). Everything
// above this package — the sync engine, the handlers, the UI — only knows the
// interfaces declared here, which is what makes intra-provider jobs
// (Google → Google, Microsoft → Microsoft) work with the same code path.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// ErrNotFound marks a remote item that no longer exists. The sync engine treats
// it as a deletion instead of an error.
var ErrNotFound = errors.New("remote item not found")

// ErrUnsupported marks an operation a provider cannot express (e.g. downloading
// a native Google document, which has no single file representation).
var ErrUnsupported = errors.New("operation not supported by this provider")

// ErrInvalidIdentifier marks an identifier a caller handed to a provider that
// the provider refuses to put into a request. Remote ids are opaque tokens the
// provider itself hands out (see DriveRoot for the synthetic ones); a value
// carrying a path separator, a query delimiter or whitespace is not one, and is
// refused instead of being interpolated into a request (see
// docs/providers.md — identifiers).
var ErrInvalidIdentifier = errors.New("invalid provider identifier")

// ErrRateLimited marks a provider refusal that means "too many requests": the
// provider throttled the call and the retries (DoWithRetry) did not clear it
// within their budget. Waiting is the fix, which is why the layers above answer
// "try again later" instead of a server fault.
var ErrRateLimited = errors.New("provider is rate limiting the account")

// Credentials is the OAuth client used against one provider. Values come from
// the environment or from Admin > Providers (database override wins).
type Credentials struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	// TenantID is Microsoft only: common | organizations | consumers | <uuid>.
	TenantID string
}

// Configured reports whether a flow can be started with these credentials.
func (c Credentials) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURI != ""
}

// Tokens is the OAuth token set as returned by the provider. RefreshToken is
// empty when the provider did not rotate it; callers must then keep the stored
// one.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Expiry       time.Time
	Scopes       []string
}

// Expired reports whether the access token is expired, accounting for a two
// minute clock skew before every provider call.
func (t *Tokens) Expired(now time.Time) bool {
	return !t.Expiry.IsZero() && now.Add(2*time.Minute).After(t.Expiry)
}

// TimeToLive returns the remaining validity, never negative.
func (t *Tokens) TimeToLive(now time.Time) time.Duration {
	if t.Expiry.IsZero() {
		return 0
	}
	if remaining := t.Expiry.Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}

// AccountKind names the family of account behind a connection, when the provider
// can tell them apart: a personal Microsoft account (an MSA, `@outlook.com`) on
// one side, a work or school account on the other. The zero value means "not
// known" and is not a third family — every check below keeps the whole permission
// table applicable for it, which is exactly the behaviour that existed before the
// kind was reported at all.
//
// It is deliberately not the Drive.Kind vocabulary: a drive is `personal` or
// `business`, an account is `personal` or `work`.
type AccountKind string

const (
	// AccountKindUnknown is the kind of an account no signal could classify.
	AccountKindUnknown AccountKind = ""
	// AccountKindPersonal is a personal Microsoft account (an MSA).
	AccountKindPersonal AccountKind = "personal"
	// AccountKindWork is a work or school account, i.e. an account of a tenant.
	AccountKindWork AccountKind = "work"
)

// Account is the remote identity behind a token set.
type Account struct {
	ID        string
	Email     string
	Name      string
	AvatarURL string
	// Kind is the family of the account, when the provider can tell: it decides
	// which permissions are applicable to it (see Permission.Kinds). Empty means
	// the provider did not say, and nothing is filtered out then.
	Kind AccountKind
	// OrgID is the organisation the account belongs to, when the provider can
	// tell: the Entra tenant id for Microsoft, the Workspace hosted domain for
	// Google. It groups the administrator accounts of one organisation under a
	// single tenant/domain-wide activity feed, so the audit-log collector reads
	// that feed once (see providers.ActivityLogSource). Empty means unknown, and
	// the collector falls back to the token claim or the e-mail domain.
	OrgID string
}

// AccountClassifier is implemented by providers that can name the kind of the
// account a token set belongs to from the token itself, without calling the
// provider API. Microsoft reads the tenant claim of the access token, which is
// what an account connected before the kind existed is classified with — no
// network round trip, no write (see services.TokenManager.AccountKind).
//
// The answer is a display hint, never an authorization decision: the claim is
// read, not signature-checked, and every real operation still goes through the
// provider with the token it was handed.
type AccountClassifier interface {
	AccountKindOf(tokens *Tokens) AccountKind
}

// OrgIdentifier is implemented by providers that can name the organisation
// (tenant or domain) a token set belongs to from the token itself, without a
// provider call — Microsoft reads the `tid` claim of its access tokens. The
// audit-log collector uses it to group the administrator accounts of one
// organisation under a single tenant-wide feed, even for an account that was
// connected before the organisation was recorded on the row.
type OrgIdentifier interface {
	OrgIDOf(tokens *Tokens) string
}

// OrganisationNamer is implemented by providers that can turn an organisation id
// into the name a person recognises: the display name of an Entra tenant, the
// primary domain (and the organisation name of the Workspace profile) of a Google
// customer.
//
// It is what makes a failed feed identifiable. An audit-log failure is stored
// against (provider, organisation, stream), so without this the operator reads a
// bare tenant id in the banner and has to look up which of their tenants it is —
// and, in the common case of one tenant waiting for a consent and the rest
// reading fine, which of several tenants needs the grant.
//
// Two rules keep it honest. It is optional, so a provider that cannot name its
// organisation simply answers nothing and the caller falls back to the id. And it
// is deliberately cheap: the answer is a display label read from an endpoint the
// connection already grants (see docs/audit-logs.md), never a new consent, and
// the collector caches it for hours (see services.AuditLogService).
type OrganisationNamer interface {
	// OrganisationName answers the display name and the domain of the
	// organisation behind tokens. An empty name is not an error: it means the
	// provider could not resolve one, which is a label the caller may skip.
	OrganisationName(ctx context.Context, creds Credentials, tokens *Tokens) (name, domain string, err error)
}

// ActivityEvent is one record of a provider's activity feed, already normalised
// onto the vocabulary of models.AuditLogAction / models.AuditLogTarget. It is
// what an ActivityLogSource returns and what the collector stores as a
// models.AuditLogEvent; keeping it provider agnostic is what lets one screen and
// one filter engine cover both clouds.
type ActivityEvent struct {
	// Source names the feed the record came from (directory, drive, unified,
	// admin, login, token, …). Together with RemoteID it is the identity of the
	// event, so two feeds may use unrelated id spaces.
	Source   string
	RemoteID string

	Action models.AuditLogAction
	Target models.AuditLogTarget

	ActorEmail string
	ActorName  string
	ActorID    string
	// ActorKind is the nature of the principal (models.AuditLogActorKind): a
	// person, a workload or nobody at all. An application name belongs in
	// ActorName, never in ActorEmail.
	ActorKind models.AuditLogActorKind
	// ActorOnBehalfOf names the person a workload acted for, when the vendor
	// says so (a device registration is a service acting for the human who
	// enrolled it). Both stay empty when the record does not say.
	ActorOnBehalfOf     string
	ActorOnBehalfOfName string

	TargetName string
	TargetID   string
	DriveID    string
	SiteID     string

	// OccurredAt is when the vendor says the event happened.
	OccurredAt time.Time
	// Raw is the vendor record, kept for fields the normaliser did not map.
	Raw json.RawMessage
}

// ActivityLogSource is the optional capability behind the Audit Logs screen:
// providers able to read their organisation-wide activity feed. It is
// implemented the same way as SiteBrowser / SiteResolver, so a provider that
// cannot answer one simply does not implement it and the collector skips it.
//
// The feed is tenant/domain scoped, never per user: one call reads every event
// of the organisation the tokens belong to. A provider rarely exposes one feed,
// though — Microsoft answers directory changes from Graph and file activity from
// the Office 365 management activity API, Google answers one application of the
// Reports API per call — so a *stream* names the feed being read. The collector
// stores one cursor per (provider, organisation, stream), which is what lets one
// feed stay healthy while another one of the same tenant is waiting for a
// consent, and what keeps the failures of one stream out of the other.
type ActivityLogSource interface {
	// ActivityLogStreams lists the feeds this provider reads, using the same
	// names the events carry in ActivityEvent.Source. The collector calls
	// ActivityLogs once per stream and keeps one cursor per stream.
	ActivityLogStreams() []string

	// ActivityLogScopes lists the extra scopes the collector needs, so the guide
	// and the Accounts screen can ask an operator to reconnect an account that
	// was granted before the feed existed.
	ActivityLogScopes() []string

	// ActivityLogs reads one feed (stream) of the organisation behind tokens,
	// starting after cursor, and answers the normalised events plus the cursor of
	// the next pass. cursor is empty on the first call; next may be empty when
	// the provider has no meaningful resume token, in which case the collector
	// keeps the previous position.
	ActivityLogs(ctx context.Context, creds Credentials, tokens *Tokens, stream, cursor string) (events []ActivityEvent, next string, err error)
}

// ActivityLogNormaliser is the optional capability behind the repair pass of the
// activity feed: it maps one *stored* vendor record back onto the shared
// vocabulary, without a network read. The collector keeps the vendor document of
// every event in AuditLogEvent.Raw precisely so a normaliser that learned a field
// later can re-read the history instead of leaving it wrong forever (see
// plan 2026-11-07-audit-log-actor-attribution.md).
//
// A provider that does not implement it simply keeps its stored rows as they are:
// the repair pass skips its events, which is why the interface is optional.
type ActivityLogNormaliser interface {
	// NormaliseActivityEvent parses one vendor record of the named stream.
	// It answers false for a record it cannot read, which leaves the stored row
	// untouched instead of erasing what the previous release mapped.
	NormaliseActivityEvent(source string, raw json.RawMessage) (ActivityEvent, bool)
}

// ActivityActorEnricher is the optional capability that gives an event a better
// actor than the one its record carries: vendors log a change as made by a
// workload ("Device Registration Service") while the person behind it is only
// recoverable from another object the event points at (the device, and its
// registered owners).
//
// The collector calls it on the events of a pass and on the rows of a repair
// pass, so a batch is always the unit of work: an implementation deduplicates
// inside the batch, and a failure — a throttle, a refusal, an object that no
// longer exists — must be logged and skipped, never returned: attribution is an
// improvement, and an unreachable enrichment must not cost the pass.
type ActivityActorEnricher interface {
	// EnrichActivityActors fills the actor fields of the events it can, and
	// answers how many it improved.
	EnrichActivityActors(ctx context.Context, tokens *Tokens, events []ActivityEvent) int
}

// Drive is a named root the operator can browse and pick a folder from. For
// Google it is "My Drive" or a shared drive; for Microsoft it is a personal
// OneDrive or a SharePoint document library.
type Drive struct {
	ID   string
	Name string
	// Kind is one of: personal, business, shared, document_library, site.
	// personal and business are the two flavours of the account's own OneDrive
	// (a personal account vs a work/school one) and are grouped together by
	// the folder picker; document_library and site are SharePoint libraries;
	// shared is a Google Shared Drive.
	Kind string
	// Owner is an optional human readable owner (Microsoft shared drives).
	Owner string
}

// Item is a file or folder inside a drive. Path is filled in by the sync engine
// (relative to the synchronised root) and is empty for direct API responses.
type Item struct {
	ID         string
	Name       string
	Path       string
	ParentID   string
	IsDir      bool
	Size       int64
	ModifiedAt time.Time
	MimeType   string
	// Hash is the provider checksum (Google md5Checksum, Graph quickXorHash).
	// It is empty when the provider does not expose one for the item.
	Hash string
	// NativeDoc marks Google Docs/Sheets/Slides and other items that must be
	// exported instead of downloaded. Sync V1 skips them (documented).
	NativeDoc bool
	// Shortcut marks Google Drive shortcuts / Graph links, also skipped in V1.
	Shortcut bool
}

// Dir reports whether the item is a folder.
func (i Item) Dir() bool { return i.IsDir }

// Transfer is a streaming download of a single file.
type Transfer struct {
	Body       io.ReadCloser
	Size       int64
	MimeType   string
	ModifiedAt time.Time
	Hash       string
}

// UploadRequest describes one file upload. When ExistingID is set the provider
// overwrites that item, otherwise it creates a new file inside ParentID.
type UploadRequest struct {
	DriveID    string
	ParentID   string
	ExistingID string
	Name       string
	Size       int64
	MimeType   string
	ModifiedAt time.Time
	Body       io.Reader
}

// Provider is implemented once per cloud storage service.
type Provider interface {
	// Name is the value stored in connected_accounts.provider.
	Name() models.ProviderName

	// Scopes are the OAuth scopes requested when connecting an account.
	Scopes() []string

	// AuthCodeURL builds the authorization endpoint URL for the code flow.
	AuthCodeURL(creds Credentials, state, codeChallenge string) string

	// Exchange trades an authorization code (with its PKCE verifier) for tokens.
	Exchange(ctx context.Context, creds Credentials, code, codeVerifier string) (*Tokens, error)

	// Refresh renews an access token from a stored refresh token.
	Refresh(ctx context.Context, creds Credentials, refreshToken string) (*Tokens, error)

	// Account returns the remote identity behind the tokens.
	Account(ctx context.Context, creds Credentials, tokens *Tokens) (*Account, error)

	// Drives lists every root the account can synchronise.
	Drives(ctx context.Context, creds Credentials, tokens *Tokens) ([]Drive, error)

	// Children lists the direct children of folderID (use DriveRoot for the
	// root folder of a drive).
	Children(ctx context.Context, creds Credentials, tokens *Tokens, driveID, folderID string) ([]Item, error)

	// Item resolves a single item by id.
	Item(ctx context.Context, creds Credentials, tokens *Tokens, driveID, itemID string) (*Item, error)

	// Download streams a file.
	Download(ctx context.Context, creds Credentials, tokens *Tokens, driveID, itemID string) (*Transfer, error)

	// Upload creates or overwrites a file and returns the stored item.
	Upload(ctx context.Context, creds Credentials, tokens *Tokens, req UploadRequest) (*Item, error)

	// CreateFolder creates a folder inside parentID.
	CreateFolder(ctx context.Context, creds Credentials, tokens *Tokens, driveID, parentID, name string) (*Item, error)

	// Delete removes an item permanently.
	Delete(ctx context.Context, creds Credentials, tokens *Tokens, driveID, itemID string) error

	// IsNotFound reports whether err is the provider's "gone" error, so the
	// engine can treat a vanished file as a deletion.
	IsNotFound(err error) bool

	// IsUnauthorized reports whether err is a rejected-access-token error (HTTP
	// 401), so a run that outlived the token snapshot it loaded at its start can
	// renew it and retry instead of failing every remaining call. A genuine
	// permission problem (403 and friends) must not be classified here.
	IsUnauthorized(err error) bool
}

// Capability names a class of provider operations the operator can recognise in
// the UI ("this account can manage licences"). Capabilities are derived from the
// scope string the provider returned, so a connection made before a scope was
// added reports what it is missing instead of claiming a feature it cannot use.
type Capability string

const (
	// CapabilityFiles covers browsing and transferring files (the sync engine).
	CapabilityFiles Capability = "files"
	// CapabilityUsers covers reading and managing user accounts.
	CapabilityUsers Capability = "users"
	// CapabilityGroups covers groups and Microsoft distribution lists.
	CapabilityGroups Capability = "groups"
	// CapabilityMembers covers group membership.
	CapabilityMembers Capability = "members"
	// CapabilityTeams covers reading the Microsoft Teams of a tenant and their
	// membership (the teams and team members report sections).
	CapabilityTeams Capability = "teams"
	// CapabilityPlanner covers reading the Planner plans of the groups of a
	// tenant (the planner plans report section).
	CapabilityPlanner Capability = "planner"
	// CapabilityDomains covers the verified domains of a tenant.
	CapabilityDomains Capability = "domains"
	// CapabilityOrgUnits covers organisational units and their settings.
	CapabilityOrgUnits Capability = "orgunits"
	// CapabilityRoles covers the administrative role catalogue (read-only).
	CapabilityRoles Capability = "roles"
	// CapabilityLicenses covers subscription and licence assignment.
	CapabilityLicenses Capability = "licenses"
	// CapabilityLogs covers reading the organisation-wide activity feed behind
	// the Audit Logs screen (see ActivityLogSource).
	CapabilityLogs Capability = "logs"
)

// ProviderCapabilities lists every capability in the order the UI displays them.
var ProviderCapabilities = []Capability{
	CapabilityFiles,
	CapabilityUsers,
	CapabilityGroups,
	CapabilityMembers,
	CapabilityTeams,
	CapabilityPlanner,
	CapabilityDomains,
	CapabilityOrgUnits,
	CapabilityRoles,
	CapabilityLicenses,
	CapabilityLogs,
}

// capabilityExclusive names the capabilities only one provider declares, exactly
// as reportSectionExclusive does for the report sections: the vocabulary is
// shared, the products behind it are not. Google has no Teams and no Planner, so a
// Workspace account is never offered them — not in the guide, not as a badge, and
// above all not as a missing permission a reconnection could add. A capability
// absent from this map is part of every provider's vocabulary.
var capabilityExclusive = map[Capability]models.ProviderName{
	CapabilityTeams:   models.ProviderMicrosoft,
	CapabilityPlanner: models.ProviderMicrosoft,
}

// CapabilitiesFor answers the capabilities in the vocabulary of one provider, in
// display order.
func CapabilitiesFor(name models.ProviderName) []Capability {
	capabilities := make([]Capability, 0, len(ProviderCapabilities))
	for _, capability := range ProviderCapabilities {
		if owner, exclusive := capabilityExclusive[capability]; exclusive && owner != name {
			continue
		}
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

// Permission documents one requested scope: why it is requested, which feature
// needs it, and whether a tenant administrator has to grant it for everybody.
// The slice a provider returns is its single source of truth: Scopes() builds
// the authorization request from it, GET /api/providers/:provider/guide renders
// it and the capability badges are derived from it.
type Permission struct {
	// Capability is the feature class this scope unlocks.
	Capability Capability `json:"capability"`
	// Title is the i18n key suffix of the human readable description
	// (`admin.capability.<title>`); it lets one capability be described by
	// several scopes without repeating the text.
	Title string `json:"title"`
	// Scope is the exact string sent to the provider.
	Scope string `json:"scope"`
	// AdminConsent is true when the scope can only be granted tenant-wide
	// (Google: administrator identity + API controls allowlist; Microsoft:
	// admin consent), which the guide has to explain before the flow starts.
	AdminConsent bool `json:"admin_consent"`
	// Informational marks a scope that is requested and documented but never
	// used to prove its capability. The OpenID Connect scopes (`openid`,
	// `profile`, `email`, `offline_access`) are the case in point: they buy no
	// feature of their own, and the identity platform does not have to report
	// them back — Microsoft Entra ID grants them while the token response only
	// carries the Graph resource scopes, percent-encoded
	// (`"scope": "https%3A%2F%2Fgraph.microsoft.com%2Fmail.read"`, see
	// SplitScopes) — so requiring them would report a freshly connected account
	// as missing permissions it already holds.
	Informational bool `json:"informational,omitempty"`
	// Kinds lists the account kinds that can hold this scope; empty means every
	// kind, which is the common case. A scope only a work or school account can
	// hold (the Microsoft Graph directory permissions) is what
	// ApplicablePermissions moves out of the table of a personal account, so the
	// accounts screen reports those capabilities as not applicable instead of
	// reporting a grant that can never be completed.
	Kinds []AccountKind `json:"kinds,omitempty"`
}

// ScopeCatalog is implemented by providers that publish their permission table.
// It is optional so the interface above stays the only mandatory contract.
type ScopeCatalog interface {
	// Permissions returns every scope the provider requests, in request order.
	Permissions() []Permission
	// ConsoleURLs returns the registration-console deep links specific to this
	// provider (API enablement pages), keyed by a stable name.
	ConsoleURLs() map[string]string
}

// Catalog returns the permission table of a provider, or nil when the provider
// does not publish one.
func Catalog(provider Provider) []Permission {
	catalog, ok := provider.(ScopeCatalog)
	if !ok {
		return nil
	}
	return catalog.Permissions()
}

// ConsoleURLs returns the provider specific console links, never nil.
func ConsoleURLs(provider Provider) map[string]string {
	links := map[string]string{}
	if catalog, ok := provider.(ScopeCatalog); ok {
		for key, url := range catalog.ConsoleURLs() {
			links[key] = url
		}
	}
	return links
}

// ScopesOf returns the scope strings of a permission table, in order.
func ScopesOf(permissions []Permission) []string {
	scopes := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		scopes = append(scopes, permission.Scope)
	}
	return scopes
}

// SplitScopes splits a scope string into its values. Both the space separated
// form and the percent-encoded one the Microsoft identity platform uses in the
// `scope` value of a token response
// (`"scope": "https%3A%2F%2Fgraph.microsoft.com%2Fmail.read"`) describe the same
// grant, so a stored or reported value is always read as a list: without the
// decoding an Entra grant would be one unreadable scope, and every capability
// would look ungranted. A value that is not encoded is returned untouched.
func SplitScopes(raw string) []string {
	if decoded, err := url.QueryUnescape(raw); err == nil {
		raw = decoded
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// CapabilitiesOf maps a granted scope list onto the capabilities of a permission
// table. Matching is case-insensitive and a capability is only reported when
// every scope it declares was granted, so an incomplete or stale grant never
// claims a feature.
func CapabilitiesOf(permissions []Permission, granted []string) []Capability {
	satisfied := []Capability{}
	for _, capability := range ProviderCapabilities {
		if HasCapability(permissions, granted, capability) {
			satisfied = append(satisfied, capability)
		}
	}
	return satisfied
}

// HasCapability reports whether a grant satisfies every scope of a capability.
// The declarations marked Informational are requested for the protocol, not for
// the feature, so a grant that does not carry them still satisfies the
// capability (see Permission.Informational).
func HasCapability(permissions []Permission, granted []string, capability Capability) bool {
	if !DeclaresCapability(permissions, capability) {
		return false
	}
	for _, permission := range permissions {
		if permission.Capability != capability || permission.Informational {
			continue
		}
		if !containsScope(granted, permission.Scope) {
			return false
		}
	}
	return true
}

// DeclaresCapability reports whether a permission table has any declaration for a
// capability, whatever the grant is. A table without a declaration for it cannot
// prove it (see HasCapability), which is what lets a capability be reported as
// outside an account's reach rather than missing.
func DeclaresCapability(permissions []Permission, capability Capability) bool {
	for _, permission := range permissions {
		if permission.Capability == capability {
			return true
		}
	}
	return false
}

// MissingCapabilities returns the capabilities of a table that a grant does not
// satisfy, in display order.
func MissingCapabilities(permissions []Permission, granted []string) []Capability {
	missing := []Capability{}
	for _, capability := range ProviderCapabilities {
		if HasCapability(permissions, granted, capability) {
			continue
		}
		missing = append(missing, capability)
	}
	return missing
}

// CapabilityReport is what a permission table says about one account, in display
// order: what the account holds, what it is missing and what it can never apply
// to. Only Missing is actionable.
type CapabilityReport struct {
	// Granted is satisfied by the stored grant.
	Granted []Capability
	// Missing is declared by the applicable permissions and not covered by the
	// grant, so a reconnection can still add it.
	Missing []Capability
	// NotApplicable is outside what this kind of account can hold at all: no
	// reconnection changes that (see AccountKind).
	NotApplicable []Capability
}

// ReportCapabilities builds the capability report of an account of one provider
// from the split of its permission table (ApplicablePermissions) and the scopes
// that were granted.
//
// The two halves of the table are read together because a capability can be
// declared by both. CapabilityFiles on a personal Microsoft account is the case in
// point: it holds through OneDrive even though the SharePoint library scope behind
// the same capability can never be granted, so it stays granted instead of being
// reported twice — once green and once greyed out. A capability neither half
// declares is reported missing, which is what an unpublished capability always
// was; the ones outside the vocabulary of this provider are not reported at all
// (see capabilityExclusive).
func ReportCapabilities(provider models.ProviderName, applicable, notApplicable []Permission, granted []string) CapabilityReport {
	report := CapabilityReport{
		Granted:       []Capability{},
		Missing:       []Capability{},
		NotApplicable: []Capability{},
	}
	for _, capability := range CapabilitiesFor(provider) {
		switch {
		case !DeclaresCapability(applicable, capability) && DeclaresCapability(notApplicable, capability):
			report.NotApplicable = append(report.NotApplicable, capability)
		case HasCapability(applicable, granted, capability):
			report.Granted = append(report.Granted, capability)
		default:
			report.Missing = append(report.Missing, capability)
		}
	}
	return report
}

// ApplicablePermissions splits a permission table for an account of a given kind:
// the permissions the account can hold, and the ones it can never hold. The
// second list is what the accounts screen renders as "not applicable" — a
// personal Microsoft account cannot be granted the directory permissions, so
// reporting them as missing would promise a reconnect that cannot deliver them.
//
// Both slices are never nil, and an unknown kind keeps the whole table
// applicable: an account the provider could not classify behaves exactly as it
// did before the kind existed.
func ApplicablePermissions(permissions []Permission, kind AccountKind) (applicable, notApplicable []Permission) {
	applicable = make([]Permission, 0, len(permissions))
	notApplicable = []Permission{}
	for _, permission := range permissions {
		if AppliesToKind(permission, kind) {
			applicable = append(applicable, permission)
			continue
		}
		notApplicable = append(notApplicable, permission)
	}
	return applicable, notApplicable
}

// AppliesToKind reports whether an account of this kind can hold a scope. A
// permission that names no kind applies to every kind, and so does every
// permission of the unknown kind.
func AppliesToKind(permission Permission, kind AccountKind) bool {
	if kind == AccountKindUnknown || len(permission.Kinds) == 0 {
		return true
	}
	for _, candidate := range permission.Kinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

// normalizeScope lowercases a scope and drops the Graph resource prefix, so a
// grant reported as "https://graph.microsoft.com/User.Read" and one reported as
// "User.Read" are recognised as the same permission.
func normalizeScope(scope string) string {
	value := strings.ToLower(strings.TrimSpace(scope))
	return strings.TrimPrefix(value, graphResource)
}

// graphResource is the resource prefix of a Microsoft Graph delegated scope. Both
// forms appear in token responses, so the scope matching above is prefix
// tolerant instead of the permission tables having to declare every variant.
const graphResource = "https://graph.microsoft.com/"

// GrantsScope reports whether a grant carries one scope, ignoring case, surrounding
// whitespace and (for Microsoft Graph) the resource prefix.
//
// It answers a different question than HasCapability: whether an optional read a
// provider performs on top of its permission table is allowed by the grant in hand
// (see the device-owner enrichment of the Microsoft audit feed). A capability asks
// for *every* permission it declares, which is the right question for a badge and
// the wrong one for an enrichment that any of several scopes authorises.
func GrantsScope(granted []string, scope string) bool {
	return containsScope(granted, scope)
}

// containsScope reports whether a granted scope list holds a scope, ignoring case,
// surrounding whitespace and (for Microsoft Graph) the resource prefix.
func containsScope(granted []string, scope string) bool {
	wanted := normalizeScope(scope)
	for _, item := range granted {
		if normalizeScope(item) == wanted {
			return true
		}
	}
	return false
}

// AuthError is a refusal of the provider's authorization server that the
// operator can act on. Code is the OAuth error code (`access_denied`,
// `invalid_scope`, `consent_required`, …) and Description is the raw text the
// provider sent, which the UI only shows as technical detail.
type AuthError struct {
	Provider    models.ProviderName
	Code        string
	Description string
}

// Error renders a diagnostic message that never contains a token.
func (e *AuthError) Error() string {
	description := strings.TrimSpace(e.Description)
	if description == "" {
		return fmt.Sprintf("%s: the authorization server refused the request (%s)", e.Provider, e.Code)
	}
	return fmt.Sprintf("%s: the authorization server refused the request (%s): %s", e.Provider, e.Code, description)
}

// NewAuthError normalises an authorization-server refusal, dropping an empty or
// unhelpful description so the message stays readable in the UI.
func NewAuthError(provider models.ProviderName, code, description string) *AuthError {
	code = strings.TrimSpace(code)
	if code == "" {
		code = "unknown_error"
	}
	return &AuthError{
		Provider:    provider,
		Code:        code,
		Description: strings.TrimSpace(description),
	}
}

// consentCodes are the OAuth error codes meaning "the permissions were not
// granted": a missing tenant-wide admin consent, a denied prompt or a scope the
// application may not use. They all map to services.ErrConsent instead of the
// generic validation error the caller would otherwise report.
var consentCodes = map[string]bool{
	"access_denied":          true,
	"admin_consent_required": true,
	"consent_required":       true,
	"interaction_required":   true,
	"invalid_scope":          true,
	"unauthorized_client":    true,
}

// IsConsentRequired reports whether err is a provider refusal the operator fixes
// by granting the requested permissions (and, for Microsoft, the tenant-wide
// admin consent) rather than by retrying.
func IsConsentRequired(err error) bool {
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		return false
	}
	return consentCodes[strings.ToLower(strings.TrimSpace(authErr.Code))]
}

// ConsentGranter is implemented by providers whose permissions must be granted
// once for the whole tenant (the Microsoft Entra "admin consent" endpoint).
type ConsentGranter interface {
	// AdminConsentURL builds the tenant-wide consent URL for the credentials.
	AdminConsentURL(creds Credentials, state string) string
}

// DriveRoot is the well known id of the root folder of a drive, understood by
// both providers (Google "root", Graph "root").
const DriveRoot = "root"

// NativeMimePrefix marks Google native documents that have no binary body.
const NativeMimePrefix = "application/vnd.google-apps."

// SiteBrowser is implemented by providers able to discover extra drives (for
// example SharePoint document libraries for Microsoft Graph).
type SiteBrowser interface {
	SearchSites(ctx context.Context, creds Credentials, tokens *Tokens, query string) ([]Drive, error)
}

// SiteResolver is implemented by providers able to turn an operator supplied
// location — a provider URL, a server-relative path or a remote id — into a
// browsable drive. It backs the manual fallback of the folder picker, for the
// roots a keyword search does not surface (see SiteBrowser). A malformed
// location is refused with ErrInvalidIdentifier, and one that names no drive the
// account can reach with ErrNotFound.
type SiteResolver interface {
	ResolveSite(ctx context.Context, creds Credentials, tokens *Tokens, location string) (Drive, error)
}
