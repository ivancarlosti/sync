// This file covers the traffic accounting of a run (billing.go), the SQL report
// behind it (store.go) and the billing schemas (billing_schema.go).

package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ivancarlosti/sync/internal/models"
)

// trafficOf indexes the traffic rows of a run by direction.
func trafficOf(t *testing.T, store *Store, runID uint) map[string]models.TrafficEntry {
	t.Helper()
	rows, err := store.GetRunTraffic(context.Background(), runID)
	if err != nil {
		t.Fatalf("loading the traffic of run %d: %v", runID, err)
	}
	byDirection := make(map[string]models.TrafficEntry, len(rows))
	for _, row := range rows {
		byDirection[row.Direction] = row
	}
	return byDirection
}

// TestRunRecordsTraffic follows a copy through the engine and checks what landed
// in traffic_entries: one inbound row for the account read from and one outbound
// row for the account written to, with the payload-free operations counted on the
// side that received them.
func TestRunRecordsTraffic(t *testing.T) {
	base := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	env := newTestEnv(t, models.DirectionGoogleToMicrosoft, models.ConflictNewestWins)
	env.google.add("report.txt", "hello world", base) // 11 bytes
	env.google.add("docs/notes.md", "# notes", base)  // 7 bytes

	run := env.run(t)
	rows := trafficOf(t, env.store, run.ID)
	if len(rows) != 2 {
		t.Fatalf("traffic rows = %+v, want one per direction", rows)
	}

	accounts, err := env.store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("listing the accounts: %v", err)
	}
	source, destination := uint(0), uint(0)
	for _, account := range accounts {
		switch models.ProviderName(account.Provider) {
		case models.ProviderGoogle:
			source = account.ID
		case models.ProviderMicrosoft:
			destination = account.ID
		}
	}

	inbound := rows[string(models.TrafficInbound)]
	if inbound.AccountID != source || inbound.Bytes != 18 || inbound.Files != 2 || inbound.Operations != 0 {
		t.Errorf("inbound = %+v, want 18 bytes and 2 files through the source account %d", inbound, source)
	}
	outbound := rows[string(models.TrafficOutbound)]
	if outbound.AccountID != destination || outbound.Bytes != 18 || outbound.Files != 2 || outbound.Operations != 1 {
		t.Errorf("outbound = %+v, want 18 bytes, 2 files and the created folder through %d", outbound, destination)
	}
	// The buckets are the UTC day and month of the run start and are shared by the
	// two rows, so a run never spans two invoices.
	wantDay := run.StartedAt.UTC().Format(models.TrafficDayFormat)
	wantMonth := run.StartedAt.UTC().Format(models.TrafficMonthFormat)
	for _, row := range rows {
		if row.Day != wantDay || row.Month != wantMonth {
			t.Errorf("row = %+v, want the %s / %s buckets of the run start", row, wantDay, wantMonth)
		}
		if row.JobID != env.job.ID || row.RunID != run.ID {
			t.Errorf("row = %+v, want the job and the run that produced it", row)
		}
	}
}

// TestTrafficFlushIsAbsoluteAndIdempotent checks the two properties the flush
// relies on: the counters are totals (never increments) and writing them twice is
// harmless, which is what makes a throttled flush safe.
func TestTrafficFlushIsAbsoluteAndIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := &models.ConnectedAccount{ID: 7, Provider: string(models.ProviderGoogle), Email: "ops@example.test"}
	started := time.Date(2026, 10, 13, 8, 30, 0, 0, time.UTC)
	service := NewSyncService(store, nil, nil, nil)
	service.now = func() time.Time { return started.Add(2 * time.Hour) }

	engine := &engine{
		service: service,
		job:     &models.SyncJob{ID: 3},
		run:     &models.SyncRun{ID: 11, JobID: 3, StartedAt: started},
	}
	engine.addTraffic(account, models.TrafficInbound, 1024, 1, 0)
	engine.flushTraffic(ctx, true) // the first flush writes the row

	engine.addTraffic(account, models.TrafficInbound, 1024, 1, 0)
	engine.flushTraffic(ctx, true)

	rows := trafficOf(t, store, 11)
	if len(rows) != 1 {
		t.Fatalf("traffic rows = %d, want the single (run, account, direction) row", len(rows))
	}
	row := rows[string(models.TrafficInbound)]
	if row.Bytes != 2048 || row.Files != 2 {
		t.Fatalf("row = %+v, want the accumulated 2048 bytes of 2 files", row)
	}
	if row.Day != "2026-10-13" || row.Month != "2026-10" {
		t.Fatalf("row = %+v, want the buckets of the run start", row)
	}

	// A second run of the same job keeps its own row.
	engine.run = &models.SyncRun{ID: 12, JobID: 3, StartedAt: started}
	engine.traffic = map[trafficKey]trafficCounter{}
	engine.addTraffic(account, models.TrafficOutbound, 10, 1, 2)
	engine.flushTraffic(ctx, true)
	if row := trafficOf(t, store, 12)[string(models.TrafficOutbound)]; row.Bytes != 10 || row.Operations != 2 {
		t.Fatalf("row = %+v, want 10 bytes and 2 operations", row)
	}
	if got := len(trafficOf(t, store, 11)); got != 1 {
		t.Fatalf("the first run now has %d rows, want 1: a new run must not touch another one", got)
	}
}

// seedTraffic builds a small history: two days, two accounts, two runs.
func seedTraffic(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	day := func(value string) time.Time {
		parsed, err := time.Parse(models.TrafficDayFormat, value)
		if err != nil {
			t.Fatalf("parsing the day %q: %v", value, err)
		}
		return parsed.UTC()
	}
	rows := []models.TrafficEntry{
		{
			RunID: 1, JobID: 10, AccountID: 1, AccountEmail: "a@example.test", Provider: "google",
			Direction: string(models.TrafficInbound), Bytes: 100, Files: 1,
			Day: "2026-10-01", Month: "2026-10", StartedAt: day("2026-10-01"),
		},
		{
			RunID: 1, JobID: 10, AccountID: 2, AccountEmail: "b@example.test", Provider: "microsoft",
			Direction: string(models.TrafficOutbound), Bytes: 100, Files: 1, Operations: 1,
			Day: "2026-10-01", Month: "2026-10", StartedAt: day("2026-10-01"),
		},
		{
			RunID: 2, JobID: 11, AccountID: 1, AccountEmail: "a@example.test", Provider: "google",
			Direction: string(models.TrafficInbound), Bytes: 50, Files: 2,
			Day: "2026-11-02", Month: "2026-11", StartedAt: day("2026-11-02"),
		},
	}
	if err := store.SaveTraffic(ctx, rows); err != nil {
		t.Fatalf("saving the traffic: %v", err)
	}
}

// TestBillingSummaryGroups checks every grouping, the window filter and the
// per-run breakdown the run detail reads.
func TestBillingSummaryGroups(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedTraffic(t, store)

	days, err := store.BillingSummary(ctx, BillingFilter{GroupBy: BillingByDay})
	if err != nil {
		t.Fatalf("grouping by day: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %+v, want two buckets", days)
	}
	// Newest first, and the directions keep their own totals.
	if days[0].Day != "2026-11-02" || days[1].Day != "2026-10-01" {
		t.Fatalf("days = %+v, want 2026-11-02 then 2026-10-01", days)
	}
	first := days[1]
	if first.Inbound.Bytes != 100 || first.Outbound.Bytes != 100 || first.Total.Bytes != 200 {
		t.Fatalf("2026-10-01 = %+v, want 100 in, 100 out and 200 total", first)
	}
	if first.Outbound.Operations != 1 || first.Outbound.Files != 1 || first.Inbound.Files != 1 {
		t.Fatalf("2026-10-01 = %+v, want the file and operation counters", first)
	}
	if first.Total.Runs != 2 {
		t.Fatalf("runs = %d, want the two directions of the single run of that day", first.Total.Runs)
	}

	months, err := store.BillingSummary(ctx, BillingFilter{GroupBy: BillingByMonth})
	if err != nil {
		t.Fatalf("grouping by month: %v", err)
	}
	if len(months) != 2 || months[0].Month != "2026-11" || months[0].Total.Bytes != 50 {
		t.Fatalf("months = %+v, want 2026-11 with 50 bytes", months)
	}

	accounts, err := store.BillingSummary(ctx, BillingFilter{GroupBy: BillingByAccount})
	if err != nil {
		t.Fatalf("grouping by account: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v, want two", accounts)
	}
	for _, row := range accounts {
		if row.AccountID == 1 && (row.AccountEmail != "a@example.test" || row.Total.Bytes != 150) {
			t.Fatalf("account 1 = %+v, want 150 bytes on a@example.test", row)
		}
	}

	runs, err := store.BillingSummary(ctx, BillingFilter{GroupBy: BillingByRun})
	if err != nil {
		t.Fatalf("grouping by run: %v", err)
	}
	if len(runs) != 2 || runs[1].RunID != 1 || runs[1].JobID != 10 || runs[1].Total.Bytes != 200 {
		t.Fatalf("runs = %+v, want run 1 of job 10 with 200 bytes", runs)
	}

	// The window is inclusive on both days and the account filter narrows it.
	windowed, err := store.BillingSummary(ctx, BillingFilter{
		GroupBy:   BillingByDay,
		From:      time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		AccountID: 2,
	})
	if err != nil {
		t.Fatalf("windowing the report: %v", err)
	}
	if len(windowed) != 1 || windowed[0].Day != "2026-10-01" || windowed[0].Outbound.Bytes != 100 {
		t.Fatalf("windowed = %+v, want only the October outbound row", windowed)
	}

	if _, err := store.BillingSummary(ctx, BillingFilter{GroupBy: "weekly"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown grouping: err = %v, want ErrValidation", err)
	}

	// The per-run listing carries the individual rows.
	individual, err := store.ListTrafficRuns(ctx, BillingFilter{JobID: 10})
	if err != nil {
		t.Fatalf("listing the traffic: %v", err)
	}
	if len(individual) != 2 || individual[0].RunID != 1 || individual[1].RunID != 1 {
		t.Fatalf("traffic = %+v, want the two rows of job 10", individual)
	}

	// An empty keep is a no-op, so nothing is lost by a disabled pruner.
	if err := store.PruneRuns(ctx, 0); err != nil {
		t.Fatalf("pruning with keep=0 must be a no-op: %v", err)
	}
	if rows, err := store.ListTrafficRuns(ctx, BillingFilter{}); err != nil || len(rows) != 3 {
		t.Fatalf("traffic = %+v (%v), want the three rows kept", rows, err)
	}
}

// TestBillingSchemaResolution checks the presets, the custom document and the
// cost/quantity maths of a report.
func TestBillingSchemaResolution(t *testing.T) {
	settings := AppSettings{
		BillingSchema:        BillingSchemaOCI,
		BillingRateInPerGiB:  0.001,
		BillingRateOutPerGiB: 0.01,
		BillingCurrency:      "eur",
	}
	schema := ResolveBillingSchema(settings)
	if schema.ID != BillingSchemaOCI || schema.Unit != "GiB" || schema.Currency != "EUR" {
		t.Fatalf("schema = %+v, want the OCI preset priced in EUR", schema)
	}
	if schema.RateInPerGiB != 0.001 || schema.RateOutPerGiB != 0.01 {
		t.Fatalf("rates = %v/%v, want the configured 0.001/0.01", schema.RateInPerGiB, schema.RateOutPerGiB)
	}
	// The preset ships indicative defaults: they are offered by the settings
	// screen and never price a report on their own.
	if schema.DefaultRateOutPerGiB != 0.0085 || schema.DefaultRateInPerGiB != 0 {
		t.Fatalf("defaults = %v/%v, want the OCI indicative list price", schema.DefaultRateInPerGiB, schema.DefaultRateOutPerGiB)
	}
	if !schema.Priced() {
		t.Fatal("a priced schema must report Priced()")
	}
	if cost, ok := schema.CostInbound(1024 * 1024 * 1024); !ok || cost != 0.001 {
		t.Fatalf("inbound cost = %v (%t), want 0.001 for 1 GiB", cost, ok)
	}
	if cost, ok := schema.CostOutbound(2 * 1024 * 1024 * 1024); !ok || cost != 0.02 {
		t.Fatalf("outbound cost = %v (%t), want 0.02 for 2 GiB", cost, ok)
	}
	if quantity := schema.Quantity(1024 * 1024 * 1024); quantity != "1.000000" {
		t.Fatalf("quantity = %q, want the GiB rendering", quantity)
	}

	// The default report stays in bytes and has no cost at all.
	bare := ResolveBillingSchema(AppSettings{})
	if bare.ID != BillingSchemaNone || bare.Unit != "B" {
		t.Fatalf("bare = %+v, want the bare-bytes default", bare)
	}
	if bare.Priced() {
		t.Fatal("a report without a rate must not be Priced()")
	}
	if _, ok := bare.CostOutbound(4096); ok {
		t.Fatal("a report without a rate must not carry a cost")
	}
	if _, ok := bare.CostInbound(4096); ok {
		t.Fatal("a report without an inbound rate must not carry an inbound cost")
	}
	if quantity := bare.Quantity(4096); quantity != "4096" {
		t.Fatalf("quantity = %q, want the raw byte count", quantity)
	}

	// One direction may be priced while the other is free: that is the shape of
	// every cloud invoice this ships a preset for.
	outboundOnly := ResolveBillingSchema(AppSettings{BillingRateOutPerGiB: 0.09})
	if !outboundOnly.Priced() {
		t.Fatal("an outbound-only price must be Priced()")
	}
	if _, ok := outboundOnly.CostInbound(1024 * 1024 * 1024); ok {
		t.Fatal("a free direction must not carry a cost")
	}

	// An unknown value falls back to the default instead of failing a report.
	if fallback := ResolveBillingSchema(AppSettings{BillingSchema: "weekly"}); fallback.ID != BillingSchemaNone {
		t.Fatalf("fallback = %+v, want the bare-bytes default", fallback)
	}

	// The custom document overrides only the fields it carries.
	custom := ResolveBillingSchema(AppSettings{
		BillingSchema:        BillingSchemaCustom,
		BillingSchemaCustom:  `{"label": "Acme", "inbound_label": "Ingress GiB", "unit": "GiB"}`,
		BillingRateInPerGiB:  0.25,
		BillingRateOutPerGiB: 0.5,
		BillingCurrency:      "usd",
	})
	if custom.Label != "Acme" || custom.InboundLabel != "Ingress GiB" || custom.Unit != "GiB" {
		t.Fatalf("custom = %+v, want the document values", custom)
	}
	if custom.OutboundLabel == "" || !custom.Custom {
		t.Fatalf("custom = %+v, want the omitted fields to keep their default", custom)
	}
	if custom.RateInPerGiB != 0.25 || custom.RateOutPerGiB != 0.5 || custom.Currency != "USD" {
		t.Fatalf("custom = %+v, want the configured rates and currency", custom)
	}
	if custom.DefaultRateOutPerGiB != 0 || custom.DefaultRateInPerGiB != 0 {
		t.Fatalf("custom = %+v, want no indicative default", custom)
	}

	// A malformed document is a validation error the settings form reports.
	if _, err := ParseBillingSchemaDocument("{not json", schema); !errors.Is(err, ErrValidation) {
		t.Fatalf("malformed document: err = %v, want ErrValidation", err)
	}
	if _, _, _, err := ValidateBillingSettings("weekly", "", "USD", 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown schema: err = %v, want ErrValidation", err)
	}
	if _, _, _, err := ValidateBillingSettings(BillingSchemaOCI, "", "EUROS", 0, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad currency: err = %v, want ErrValidation", err)
	}
	if _, _, _, err := ValidateBillingSettings(BillingSchemaOCI, "", "USD", -1, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("negative inbound rate: err = %v, want ErrValidation", err)
	}
	if _, _, _, err := ValidateBillingSettings(BillingSchemaOCI, "", "USD", 0, billingMaxRate+1); !errors.Is(err, ErrValidation) {
		t.Fatalf("outbound rate above the bound: err = %v, want ErrValidation", err)
	}
	if id, _, currency, err := ValidateBillingSettings("", "", "", 0, 0); err != nil || id != BillingSchemaNone || currency != "USD" {
		t.Fatalf("defaults = %q/%q (%v), want none/USD", id, currency, err)
	}
	if len(BillingSchemaIDs()) != 5 || len(BillingSchemas()) != 5 {
		t.Fatalf("schemas = %v, want the five presets", BillingSchemaIDs())
	}
}
