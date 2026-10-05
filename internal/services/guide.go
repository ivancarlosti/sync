package services

import (
	"context"
	"fmt"

	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// Values the guide asks the operator to paste into the provider console.
const (
	// GuideCopyRedirectURI is the callback URL registered with the provider.
	GuideCopyRedirectURI = "redirect_uri"
	// GuideCopyScopes is the complete permission list this instance requests, for
	// the consoles that take it in a single paste.
	GuideCopyScopes = "scopes"
	// GuideCopyPermissions is the same list as one value per permission, for the
	// consoles that only accept them one by one (Microsoft Entra).
	GuideCopyPermissions = "permissions"
	// GuideActionAdminConsent marks the step that starts the tenant-wide
	// consent flow (Microsoft Entra admin consent).
	GuideActionAdminConsent = "admin_consent"
)

// GuideStep is one step of the guided app registration. Everything the operator
// reads is an i18n key derived from the provider and the step id
// (`admin.guide.steps.<provider>.<id>.title` / `.body`), so the walkthrough is
// translated like the rest of the UI while the server owns the structure, the
// console links and the values to copy.
type GuideStep struct {
	// ID is the stable step identifier, part of its i18n keys.
	ID string `json:"id"`
	// URL is the registration-console page the step opens, when it has one.
	URL string `json:"url,omitempty"`
	// Copy names the value of this instance the step asks to paste in the
	// console: `redirect_uri`, `scopes` (the whole grant, when the console takes
	// the list at once), `permissions` (one value per permission, when it does
	// not), or empty.
	Copy string `json:"copy,omitempty"`
	// Optional marks a step a first, files-only setup can postpone.
	Optional bool `json:"optional,omitempty"`
	// Action names the API action the step offers: `admin_consent` when the
	// step is the tenant-wide consent button.
	Action string `json:"action,omitempty"`
}

// ProviderGuide is the answer of GET /api/providers/:provider/guide: everything
// an operator needs to create the OAuth client, in order, plus the state of this
// instance (is it configured, did the administrator consent yet).
type ProviderGuide struct {
	Provider models.ProviderName `json:"provider"`
	// Configured reports whether an OAuth client is ready to start a flow.
	Configured bool `json:"configured"`
	// RedirectURI is the exact URL to register with the provider.
	RedirectURI string `json:"redirect_uri"`
	// ConsoleURLs are the registration-console deep links, keyed by a stable
	// name (`entra_apps`, `admin_api`, …).
	ConsoleURLs map[string]string `json:"console_urls"`
	// Permissions is the permission table of the provider, in request order.
	Permissions []providers.Permission `json:"permissions"`
	// Scopes is the same list as the space separated grant, ready to copy.
	Scopes []string `json:"scopes"`
	// Capabilities are the features the table unlocks once granted.
	Capabilities []providers.Capability `json:"capabilities"`
	// AdminConsentRequired reports whether the provider needs a tenant-wide
	// consent step (Microsoft).
	AdminConsentRequired bool `json:"admin_consent_required"`
	// AdminConsent is the recorded tenant-wide consent of this instance.
	AdminConsent AdminConsentStatus `json:"admin_consent"`
	// Steps is the ordered walkthrough.
	Steps []GuideStep `json:"steps"`
	// Warnings are the machine readable caveats of this provider
	// (`admin.guide.warnings.<code>`).
	Warnings []string `json:"warnings"`
}

// GuideService assembles the walkthrough from the providers themselves, so the
// instructions can never describe a permission the code does not request.
type GuideService struct {
	registry *providers.Registry
	creds    *ProviderSettings
}

// NewGuideService builds the service.
func NewGuideService(registry *providers.Registry, creds *ProviderSettings) *GuideService {
	return &GuideService{registry: registry, creds: creds}
}

// Guide returns the walkthrough of one provider together with the state of this
// instance. It answers ErrNotFound for a provider this build does not ship.
func (g *GuideService) Guide(ctx context.Context, provider models.ProviderName) (ProviderGuide, error) {
	implementation, err := g.registry.Get(provider)
	if err != nil {
		return ProviderGuide{}, fmt.Errorf("%w: %s is not available in this build", ErrNotFound, provider)
	}
	info, err := g.creds.Info(ctx, provider)
	if err != nil {
		return ProviderGuide{}, err
	}
	status, err := g.creds.AdminConsent(ctx, provider)
	if err != nil {
		return ProviderGuide{}, err
	}
	permissions := providers.Catalog(implementation)
	if permissions == nil {
		permissions = []providers.Permission{}
	}
	links := providers.ConsoleURLs(implementation)
	_, consentRequired := implementation.(providers.ConsentGranter)

	return ProviderGuide{
		Provider:             provider,
		Configured:           info.Configured,
		RedirectURI:          info.RedirectURI,
		ConsoleURLs:          links,
		Permissions:          permissions,
		Scopes:               providers.ScopesOf(permissions),
		Capabilities:         declaredCapabilities(permissions),
		AdminConsentRequired: consentRequired,
		AdminConsent:         status,
		Steps:                guideSteps(provider, links),
		Warnings:             guideWarnings(provider),
	}, nil
}

// declaredCapabilities lists the capabilities a permission table declares, in
// display order.
func declaredCapabilities(permissions []providers.Permission) []providers.Capability {
	capabilities := []providers.Capability{}
	for _, capability := range providers.ProviderCapabilities {
		for _, permission := range permissions {
			if permission.Capability == capability {
				capabilities = append(capabilities, capability)
				break
			}
		}
	}
	return capabilities
}

// guideSteps is the ordered walkthrough of one provider. The console links come
// from the provider (providers.ConsoleURLs), so a moved console page is a change
// in one place.
func guideSteps(provider models.ProviderName, links map[string]string) []GuideStep {
	switch provider {
	case models.ProviderGoogle:
		return []GuideStep{
			// The project hosts the API enablement and the OAuth client.
			{ID: "project", URL: "https://console.cloud.google.com/projectcreate"},
			// Drive API for the sync; Admin SDK and Enterprise License Manager
			// for the directory scopes below.
			{ID: "apis", URL: links["admin_api"]},
			{ID: "consent", URL: "https://console.cloud.google.com/apis/credentials/consent"},
			{ID: "client", URL: "https://console.cloud.google.com/apis/credentials"},
			{ID: "redirect", URL: "https://console.cloud.google.com/apis/credentials", Copy: GuideCopyRedirectURI},
			{ID: "permissions", URL: "https://console.cloud.google.com/apis/credentials", Copy: GuideCopyScopes},
			{ID: "admin", URL: "https://admin.google.com/ac/owl/list"},
			{ID: "connect"},
		}
	case models.ProviderMicrosoft:
		return []GuideStep{
			{ID: "app", URL: links["entra_apps"]},
			{ID: "redirect", URL: links["entra_authentication"], Copy: GuideCopyRedirectURI},
			{ID: "secret", URL: links["entra_credentials"]},
			// Entra has no bulk paste: the permissions are added one by one.
			{ID: "permissions", URL: links["entra_api_permissions"], Copy: GuideCopyPermissions},
			{ID: "consent", Action: GuideActionAdminConsent},
			{ID: "role"},
			{ID: "connect"},
		}
	default:
		return []GuideStep{}
	}
}

// guideWarnings are the caveats that hold for every installation of a provider,
// in display order. They are codes: the UI renders `admin.guide.warnings.<code>`.
func guideWarnings(provider models.ProviderName) []string {
	switch provider {
	case models.ProviderGoogle:
		return []string{
			"api_enablement",
			"consent_screen_type",
			"unverified_app",
			"admin_role_required",
			"license_required",
		}
	case models.ProviderMicrosoft:
		return []string{
			"tenant_scope",
			"work_accounts_only",
			"admin_role_required",
			"license_required",
		}
	default:
		return []string{}
	}
}
