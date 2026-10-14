// This file holds the billing schemas: the line-item vocabulary a report can be
// labelled with. Sync ships the presets of the clouds operators most often bill
// against, the bare-bytes default and a fully custom mapping, so the same traffic
// numbers can be read in the words of the invoice they end up in.
package services

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The ids of the supported billing schemas.
const (
	// BillingSchemaNone is the default: bare bytes, no vocabulary at all.
	BillingSchemaNone = "none"
	// BillingSchemaOCI labels the report with the Oracle Cloud line items.
	BillingSchemaOCI = "oci"
	// BillingSchemaAWS labels the report with the AWS data-transfer line items.
	BillingSchemaAWS = "aws"
	// BillingSchemaGCP labels the report with the Google Cloud line items.
	BillingSchemaGCP = "gcp"
	// BillingSchemaCustom is the mapping the operator typed in Admin > Settings.
	BillingSchemaCustom = "custom"
)

// billingGiB is the divisor of a priced report: cloud invoices price GiB.
const billingGiB = 1024 * 1024 * 1024

// billingMaxRate bounds the configured price of one GiB, so a typo cannot turn a
// report into nonsense.
const billingMaxRate = 100000

// BillingSchema is the line-item vocabulary of the billing report: what the two
// quantity columns are called, in which unit they are expressed and how the cost
// column is labelled. It is driven by the `billing_schema` settings key.
type BillingSchema struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
	// The four labels are the column headers of the report (and of the UI).
	InboundLabel    string `json:"inbound_label"`
	OutboundLabel   string `json:"outbound_label"`
	OperationsLabel string `json:"operations_label"`
	CostLabel       string `json:"cost_label"`
	// Unit is what the inbound/outbound columns are expressed in (`B` or `GiB`).
	Unit string `json:"unit"`
	// Currency, RateInPerGiB and RateOutPerGiB drive the cost columns. The two
	// rates are the configured prices of one GiB in each direction and replace
	// each other: inbound is what was read from a source account (0 is the
	// common case, ingress is free) and outbound what was written to a
	// destination account. With both at 0 the report stays in bytes and carries
	// no cost at all.
	Currency      string  `json:"currency,omitempty"`
	RateInPerGiB  float64 `json:"rate_in_per_gib"`
	RateOutPerGiB float64 `json:"rate_out_per_gib"`
	// DefaultRateInPerGiB and DefaultRateOutPerGiB are the indicative list
	// prices the preset ships with. They are never used to price a report: the
	// settings screen offers them through "Reset to the schema defaults", so an
	// operator applies a price list on purpose instead of inheriting one.
	DefaultRateInPerGiB  float64 `json:"default_rate_in_per_gib"`
	DefaultRateOutPerGiB float64 `json:"default_rate_out_per_gib"`
	// Custom marks the operator's own mapping.
	Custom bool `json:"custom"`
}

// billingCustomDocument is the JSON stored under billing_schema_custom: every
// field is optional and falls back to the preset it is based on.
type billingCustomDocument struct {
	Label           string `json:"label"`
	InboundLabel    string `json:"inbound_label"`
	OutboundLabel   string `json:"outbound_label"`
	OperationsLabel string `json:"operations_label"`
	CostLabel       string `json:"cost_label"`
	Unit            string `json:"unit"`
}

// BillingSchemas returns every schema in the order the settings screen shows
// them: the default, the three cloud presets and the custom placeholder.
func BillingSchemas() []BillingSchema {
	return []BillingSchema{
		{
			ID:              BillingSchemaNone,
			Label:           "None (bytes)",
			Hint:            "Report the traffic in bytes, with no provider vocabulary. No price: the report stays unpriced until one is typed.",
			InboundLabel:    "Inbound bytes",
			OutboundLabel:   "Outbound bytes",
			OperationsLabel: "Operations",
			CostLabel:       "Cost",
			Unit:            "B",
		},
		{
			ID:              BillingSchemaOCI,
			Label:           "Oracle Cloud (OCI)",
			Hint:            "Ingress and egress, as they appear on an OCI invoice. Indicative defaults: ingress free, egress 0.0085 per GiB.",
			InboundLabel:    "Ingress (GiB)",
			OutboundLabel:   "Egress (GiB)",
			OperationsLabel: "Requests",
			CostLabel:       "Cost",
			Unit:            "GiB",
			// Indicative list price (USD per GiB): OCI does not bill ingress and
			// bills egress from the first GiB. It is a starting point an operator
			// applies on purpose, not a price this instance inherits.
			DefaultRateOutPerGiB: 0.0085,
		},
		{
			ID:                   BillingSchemaAWS,
			Label:                "Amazon Web Services",
			Hint:                 "Data transfer in and out, as on an AWS bill. Indicative defaults: ingress free, egress 0.09 per GiB.",
			InboundLabel:         "Data transfer in (GiB)",
			OutboundLabel:        "Data transfer out (GiB)",
			OperationsLabel:      "Requests",
			CostLabel:            "Data transfer cost",
			Unit:                 "GiB",
			DefaultRateOutPerGiB: 0.09,
		},
		{
			ID:                   BillingSchemaGCP,
			Label:                "Google Cloud",
			Hint:                 "Ingress and egress, as on a GCP bill. Indicative defaults: ingress free, egress 0.12 per GiB.",
			InboundLabel:         "Ingress (GiB)",
			OutboundLabel:        "Egress (GiB)",
			OperationsLabel:      "Operations",
			CostLabel:            "Network cost",
			Unit:                 "GiB",
			DefaultRateOutPerGiB: 0.12,
		},
		{
			ID:              BillingSchemaCustom,
			Label:           "Custom",
			Hint:            "Type the line-item labels you need in the JSON document below. No default price: type your own in and out rates.",
			InboundLabel:    "Inbound bytes",
			OutboundLabel:   "Outbound bytes",
			OperationsLabel: "Operations",
			CostLabel:       "Cost",
			Unit:            "B",
			Custom:          true,
		},
	}
}

// BillingSchemaIDs returns the ids of BillingSchemas, which is what the settings
// validation checks a submitted value against.
func BillingSchemaIDs() []string {
	schemas := BillingSchemas()
	ids := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		ids = append(ids, schema.ID)
	}
	return ids
}

// ResolveBillingSchema returns the schema a report uses: the preset selected in
// Admin > Settings with the configured rates and currency applied, or the
// operator's custom mapping. An unknown value falls back to bare bytes, so a
// report always renders.
//
// The preset's indicative defaults are left untouched: they are what the
// settings screen offers to reset to, they never price a report on their own.
func ResolveBillingSchema(settings AppSettings) BillingSchema {
	schema := schemaOrDefault(strings.ToLower(strings.TrimSpace(settings.BillingSchema)))
	if schema.ID == BillingSchemaCustom {
		if custom, err := ParseBillingSchemaDocument(settings.BillingSchemaCustom, schema); err == nil {
			schema = custom
		}
	}
	schema.RateInPerGiB = settings.BillingRateInPerGiB
	schema.RateOutPerGiB = settings.BillingRateOutPerGiB
	schema.Currency = strings.ToUpper(strings.TrimSpace(settings.BillingCurrency))
	return schema
}

// ParseBillingSchemaDocument turns the stored custom document into a schema,
// defaulting every missing field to `fallback`. An empty document is the fallback
// itself; a malformed one is an error the settings form reports.
func ParseBillingSchemaDocument(document string, fallback BillingSchema) (BillingSchema, error) {
	if strings.TrimSpace(document) == "" {
		return fallback, nil
	}
	var parsed billingCustomDocument
	if err := json.Unmarshal([]byte(document), &parsed); err != nil {
		return BillingSchema{}, fmt.Errorf("%w: the custom billing schema is not valid JSON: %v", ErrValidation, err)
	}
	schema := fallback
	schema.Custom = true
	apply := func(target *string, value string) {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			*target = trimmed
		}
	}
	apply(&schema.Label, parsed.Label)
	apply(&schema.InboundLabel, parsed.InboundLabel)
	apply(&schema.OutboundLabel, parsed.OutboundLabel)
	apply(&schema.OperationsLabel, parsed.OperationsLabel)
	apply(&schema.CostLabel, parsed.CostLabel)
	apply(&schema.Unit, parsed.Unit)
	return schema, nil
}

// ValidateBillingSettings checks the billing settings keys, so a report can never
// be asked to render a vocabulary that does not exist. It returns the normalised
// schema id, document and currency.
func ValidateBillingSettings(schemaID, document, currency string, rateIn, rateOut float64) (string, string, string, error) {
	id := strings.ToLower(strings.TrimSpace(schemaID))
	if id == "" {
		id = BillingSchemaNone
	}
	if !containsString(BillingSchemaIDs(), id) {
		return "", "", "", fmt.Errorf("%w: the billing schema must be one of %s",
			ErrValidation, strings.Join(BillingSchemaIDs(), ", "))
	}
	if id == BillingSchemaCustom {
		if _, err := ParseBillingSchemaDocument(document, schemaOrDefault(BillingSchemaCustom)); err != nil {
			return "", "", "", err
		}
	}
	if rateIn < 0 || rateIn > billingMaxRate {
		return "", "", "", fmt.Errorf("%w: the inbound price per GiB must be between 0 and %d", ErrValidation, billingMaxRate)
	}
	if rateOut < 0 || rateOut > billingMaxRate {
		return "", "", "", fmt.Errorf("%w: the outbound price per GiB must be between 0 and %d", ErrValidation, billingMaxRate)
	}
	code := strings.ToUpper(strings.TrimSpace(currency))
	if code == "" {
		code = "USD"
	}
	if len(code) != 3 {
		return "", "", "", fmt.Errorf("%w: the currency must be a three letter code", ErrValidation)
	}
	return id, document, code, nil
}

// Priced reports whether the report carries a cost at all: with both rates at 0
// the cost columns stay out of the screen and out of the CSV.
func (s BillingSchema) Priced() bool {
	return s.RateInPerGiB > 0 || s.RateOutPerGiB > 0
}

// CostInbound prices the bytes read from a source account. The second return is
// false when no inbound rate is configured, which is the usual case: ingress is
// free on the clouds this report is modelled after.
func (s BillingSchema) CostInbound(bytes int64) (float64, bool) {
	return priceBytes(bytes, s.RateInPerGiB)
}

// CostOutbound prices the bytes written to a destination account.
func (s BillingSchema) CostOutbound(bytes int64) (float64, bool) {
	return priceBytes(bytes, s.RateOutPerGiB)
}

// priceBytes is the one multiplication behind both directions: a byte count
// times the price of one GiB, and false when that price is 0.
func priceBytes(bytes int64, ratePerGiB float64) (float64, bool) {
	if ratePerGiB <= 0 {
		return 0, false
	}
	return float64(bytes) / billingGiB * ratePerGiB, true
}

// Quantity renders a byte count in the schema unit: whole bytes for the default
// schema, GiB with six decimals for the cloud ones.
func (s BillingSchema) Quantity(bytes int64) string {
	if s.Unit == "" || strings.EqualFold(s.Unit, "B") {
		return strconv.FormatInt(bytes, 10)
	}
	return strconv.FormatFloat(float64(bytes)/billingGiB, 'f', 6, 64)
}

// schemaOrDefault returns the preset with the id, or the bare-bytes default.
func schemaOrDefault(id string) BillingSchema {
	presets := BillingSchemas()
	for _, preset := range presets {
		if preset.ID == id {
			return preset
		}
	}
	return presets[0]
}

// DefaultBillingSchemaDocument is what the custom field starts as: the bare-bytes
// vocabulary, ready to be edited.
func DefaultBillingSchemaDocument() string {
	encoded, err := json.MarshalIndent(billingCustomDocument{
		Label:           "Custom",
		InboundLabel:    "Inbound bytes",
		OutboundLabel:   "Outbound bytes",
		OperationsLabel: "Operations",
		CostLabel:       "Cost",
		Unit:            "B",
	}, "", "  ")
	if err != nil {
		// Marshalling a fixed struct of strings cannot fail; this is belt and
		// braces.
		return "{}"
	}
	return string(encoded)
}
