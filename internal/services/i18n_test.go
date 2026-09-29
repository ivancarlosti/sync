package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ivancarlosti/sync/internal/config"
	"github.com/ivancarlosti/sync/internal/models"
	"github.com/ivancarlosti/sync/internal/providers"
)

// guideUIKeys are the catalog keys the setup guide screen reads directly (the
// step, warning and capability keys are derived from the API answer and checked
// too). They live here so a renamed key fails `go test` instead of rendering the
// key itself in the browser.
var guideUIKeys = []string{
	"nav.adminGuide",
	"admin.guide.title",
	"admin.guide.subtitle",
	"admin.guide.providers",
	"admin.guide.intro.google",
	"admin.guide.intro.microsoft",
	"admin.guide.statusReady",
	"admin.guide.statusMissing",
	"admin.guide.redirectTitle",
	"admin.guide.redirectHint",
	"admin.guide.permissionsTitle",
	"admin.guide.permissionsHint",
	"admin.guide.capabilitiesTitle",
	"admin.guide.warningsTitle",
	"admin.guide.console",
	"admin.guide.credentialsTitle",
	"admin.guide.credentialsHint",
	"admin.guide.credentialsAction",
	"admin.guide.consentTitle",
	"admin.guide.consentGranted",
	"admin.guide.consentMissing",
	"admin.guide.consentAction",
	"admin.guide.consentDone",
	"admin.guide.consentError",
	"admin.guide.connectTitle",
	"admin.guide.connectHint",
	"admin.guide.connectAction",
	"admin.guide.step",
	"admin.guide.optional",
	"admin.guide.docs",
	"admin.guide.loadError",
	"accounts.capabilities",
	"accounts.missingCapabilities",
	"accounts.reconnect",
	"accounts.reconnectHint",
	"accounts.error_consent_required",
}

// TestCatalogsAreInParity is the Go side of web/scripts/check-i18n.mjs: the seven
// catalogs carry exactly the same keys, so a translation is never forgotten.
func TestCatalogsAreInParity(t *testing.T) {
	catalogs := loadCatalogs(t)
	reference := ""
	var expected []string
	for _, file := range sortedNames(catalogs) {
		keys := make([]string, 0, len(catalogs[file]))
		for key := range catalogs[file] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if reference == "" {
			reference, expected = file, keys
			continue
		}
		if strings.Join(keys, "\n") != strings.Join(expected, "\n") {
			t.Fatalf("%s does not carry the same keys as %s", file, reference)
		}
	}
}

// TestCapabilityLabelsAreDistinct guards the capability vocabulary of the UI: two
// capabilities must not share one label, which would make the badges useless.
func TestCapabilityLabelsAreDistinct(t *testing.T) {
	catalogs := loadCatalogs(t)
	seen := map[string]providers.Capability{}
	for _, capability := range providers.ProviderCapabilities {
		label := catalogs["en-US"]["admin.capability."+string(capability)]
		if label == "" {
			t.Fatalf("capability %q has no label", capability)
		}
		if other, exists := seen[label]; exists {
			t.Fatalf("capabilities %q and %q share the label %q", other, capability, label)
		}
		seen[label] = capability
	}
}

// loadCatalogs reads every shipped SPA catalog as a flat `dotted.key -> value`
// map and checks that all of them are present.
func loadCatalogs(t *testing.T) map[string]map[string]string {
	t.Helper()
	entries, err := os.ReadDir(localesDir)
	if err != nil {
		t.Fatalf("reading %s: %v", localesDir, err)
	}
	catalogs := map[string]map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(localesDir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		var nested map[string]any
		if err := json.Unmarshal(raw, &nested); err != nil {
			t.Fatalf("%s is not valid JSON: %v", entry.Name(), err)
		}
		flat := map[string]string{}
		flattenCatalog(nested, "", flat)
		catalogs[strings.TrimSuffix(entry.Name(), ".json")] = flat
	}
	if len(catalogs) != len(config.SupportedLocales) {
		t.Fatalf("found %d catalogs, the configuration ships %d", len(catalogs), len(config.SupportedLocales))
	}
	for _, locale := range config.SupportedLocales {
		if _, ok := catalogs[locale]; !ok {
			t.Fatalf("the catalog of %s is missing", locale)
		}
	}
	return catalogs
}

// flattenCatalog turns a nested catalog into `dotted.key -> value` leaves.
func flattenCatalog(value map[string]any, prefix string, out map[string]string) {
	for key, entry := range value {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if nested, ok := entry.(map[string]any); ok {
			flattenCatalog(nested, path, out)
			continue
		}
		text, ok := entry.(string)
		if !ok {
			continue
		}
		out[path] = text
	}
}

// sortedNames returns the catalog files in a stable order.
func sortedNames(catalogs map[string]map[string]string) []string {
	names := make([]string, 0, len(catalogs))
	for name := range catalogs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestGuideKeysAreTranslatedEverywhere is the drift net of this feature: every
// key the guide API makes the UI render (steps, warnings, capability labels) plus
// every key the screen reads itself must exist in all seven catalogs, non-empty.
func TestGuideKeysAreTranslatedEverywhere(t *testing.T) {
	guide, _ := guideFixture(t)
	catalogs := loadCatalogs(t)
	ctx := context.Background()

	keys := append([]string{}, guideUIKeys...)
	for _, name := range []models.ProviderName{models.ProviderGoogle, models.ProviderMicrosoft} {
		built, err := guide.Guide(ctx, name)
		if err != nil {
			t.Fatalf("Guide(%s) error = %v", name, err)
		}
		for _, step := range built.Steps {
			base := "admin.guide.steps." + string(name) + "." + step.ID
			keys = append(keys, base+".title", base+".body")
		}
		for _, warning := range built.Warnings {
			keys = append(keys, "admin.guide.warnings."+warning)
		}
		for _, capability := range built.Capabilities {
			keys = append(keys, "admin.capability."+string(capability))
		}
	}

	sort.Strings(keys)
	for _, file := range sortedNames(catalogs) {
		catalog := catalogs[file]
		for _, key := range keys {
			value, found := catalog[key]
			if !found {
				t.Errorf("%s: missing key %q", file, key)
				continue
			}
			if strings.TrimSpace(value) == "" {
				t.Errorf("%s: key %q is empty", file, key)
			}
		}
	}
}
