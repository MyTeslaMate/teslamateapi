package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestSQLExportRepositoryReadOnlyIntegration(t *testing.T) {
	dsn := os.Getenv("TESLAMATEAPI_TEST_DSN")
	if dsn == "" {
		t.Skip("set TESLAMATEAPI_TEST_DSN to run read-only PostgreSQL integration tests")
	}
	carID := 1
	if rawCarID := os.Getenv("TESLAMATEAPI_TEST_CAR_ID"); rawCarID != "" {
		parsed, err := strconv.Atoi(rawCarID)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid TESLAMATEAPI_TEST_CAR_ID %q", rawCarID)
		}
		carID = parsed
	}

	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	var readOnly string
	if err := database.QueryRowContext(ctx, "SHOW default_transaction_read_only").Scan(&readOnly); err != nil {
		t.Fatalf("verify read-only connection: %v", err)
	}
	if readOnly != "on" {
		t.Fatalf("integration DSN must set default_transaction_read_only=on, got %q", readOnly)
	}

	repository := &sqlExportRepository{db: database}
	manifest, err := repository.Manifest(ctx, carID)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.CarID != carID || manifest.CompletedBefore.IsZero() {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}

	assertDriveSamplePages(t, ctx, repository, manifest)
	assertChargeSamplePages(t, ctx, repository, manifest)
	assertCompletionCutoff(t, ctx, repository, manifest)

	var unknownCarID int
	if err := database.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) + 1 FROM cars").Scan(&unknownCarID); err != nil {
		t.Fatalf("find unknown car ID: %v", err)
	}
	if _, err := repository.Manifest(ctx, unknownCarID); !errors.Is(err, errExportCarNotFound) {
		t.Fatalf("unknown car: got %v", err)
	}
}

func TestExportHandlerPostgreSQLDeadlineIntegration(t *testing.T) {
	dsn := os.Getenv("TESLAMATEAPI_TEST_DSN")
	if dsn == "" {
		t.Skip("set TESLAMATEAPI_TEST_DSN to run PostgreSQL deadline integration tests")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	setExportAuthDisabled(t)
	repository := &fakeExportRepository{
		manifestFn: func(ctx context.Context, _ int) (exportManifest, error) {
			_, err := database.ExecContext(ctx, "SELECT pg_sleep(1)")
			return exportManifest{}, err
		},
	}
	router := newExportTestRouterWithConfig(
		repository,
		exportCursorKey([]byte("postgres-timeout-key")),
		exportRuntimeConfig{QueryTimeout: 20 * time.Millisecond, MaxConcurrentQueries: 1},
	)
	recorder := performExportRequest(
		router,
		httptest.NewRequest(http.MethodGet, "/api/v1/cars/1/export/manifest", nil),
	)
	problem := decodeExportProblem(t, recorder)
	if recorder.Code != http.StatusServiceUnavailable || problem.Code != "request_timeout" || !problem.Retryable {
		t.Fatalf("unexpected PostgreSQL timeout response: %d %#v", recorder.Code, problem)
	}
}

func TestSQLExportRepositoryFullTraversal(t *testing.T) {
	if os.Getenv("TESLAMATEAPI_TEST_FULL_EXPORT") != "true" {
		t.Skip("set TESLAMATEAPI_TEST_FULL_EXPORT=true with a read-only DSN to verify every bounded row")
	}
	dsn := os.Getenv("TESLAMATEAPI_TEST_DSN")
	if dsn == "" {
		t.Fatal("TESLAMATEAPI_TEST_DSN is required")
	}
	carID := 1
	if rawCarID := os.Getenv("TESLAMATEAPI_TEST_CAR_ID"); rawCarID != "" {
		parsed, err := strconv.Atoi(rawCarID)
		if err != nil || parsed <= 0 || parsed > exportMaximumCarID {
			t.Fatalf("invalid TESLAMATEAPI_TEST_CAR_ID %q", rawCarID)
		}
		carID = parsed
	}

	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping PostgreSQL: %v", err)
	}
	var readOnly string
	if err := database.QueryRowContext(ctx, "SHOW default_transaction_read_only").Scan(&readOnly); err != nil {
		t.Fatalf("verify read-only connection: %v", err)
	}
	if readOnly != "on" {
		t.Fatalf("full traversal DSN must set default_transaction_read_only=on, got %q", readOnly)
	}

	repository := &sqlExportRepository{db: database}
	manifest, err := repository.Manifest(ctx, carID)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	driveCount := traverseAllDriveSamples(t, ctx, repository, manifest)
	if driveCount != manifest.DriveSamples.RowCount {
		t.Fatalf("drive traversal count: got %d want %d", driveCount, manifest.DriveSamples.RowCount)
	}
	chargeCount := traverseAllChargeSamples(t, ctx, repository, manifest)
	if chargeCount != manifest.ChargeSamples.RowCount {
		t.Fatalf("charge traversal count: got %d want %d", chargeCount, manifest.ChargeSamples.RowCount)
	}
}

func traverseAllDriveSamples(
	t *testing.T,
	ctx context.Context,
	repository *sqlExportRepository,
	manifest exportManifest,
) int64 {
	t.Helper()
	cursor := initialExportCursor(manifest, exportResourceDriveSamples, manifest.DriveSamples)
	var count int64
	var lastID int64
	for page := 0; ; page++ {
		samples, err := repository.DriveSamples(ctx, manifest.CarID, cursor, exportMaximumPageLimit+1)
		if err != nil {
			t.Fatalf("drive traversal page %d: %v", page, err)
		}
		hasMore := len(samples) > exportMaximumPageLimit
		if hasMore {
			samples = samples[:exportMaximumPageLimit]
		}
		assertDriveSampleOrder(t, samples, cursor)
		count += int64(len(samples))
		if len(samples) > 0 {
			lastID = samples[len(samples)-1].SampleID
		}
		if !hasMore {
			if count > 0 && lastID != manifest.DriveSamples.HighWatermark {
				t.Fatalf("final drive sample ID: got %d want %d", lastID, manifest.DriveSamples.HighWatermark)
			}
			return count
		}
		cursor.AfterID = samples[len(samples)-1].SampleID
	}
}

func traverseAllChargeSamples(
	t *testing.T,
	ctx context.Context,
	repository *sqlExportRepository,
	manifest exportManifest,
) int64 {
	t.Helper()
	cursor := initialExportCursor(manifest, exportResourceChargeSamples, manifest.ChargeSamples)
	var count int64
	var lastID int64
	for page := 0; ; page++ {
		samples, err := repository.ChargeSamples(ctx, manifest.CarID, cursor, exportMaximumPageLimit+1)
		if err != nil {
			t.Fatalf("charge traversal page %d: %v", page, err)
		}
		hasMore := len(samples) > exportMaximumPageLimit
		if hasMore {
			samples = samples[:exportMaximumPageLimit]
		}
		assertChargeSampleOrder(t, samples, cursor)
		count += int64(len(samples))
		if len(samples) > 0 {
			lastID = samples[len(samples)-1].SampleID
		}
		if !hasMore {
			if count > 0 && lastID != manifest.ChargeSamples.HighWatermark {
				t.Fatalf("final charge sample ID: got %d want %d", lastID, manifest.ChargeSamples.HighWatermark)
			}
			return count
		}
		cursor.AfterID = samples[len(samples)-1].SampleID
	}
}

func assertCompletionCutoff(
	t *testing.T,
	ctx context.Context,
	repository *sqlExportRepository,
	manifest exportManifest,
) {
	t.Helper()
	driveCursor := initialExportCursor(manifest, exportResourceDriveSamples, manifest.DriveSamples)
	driveCursor.CompletedBeforeUS = 1
	driveSamples, err := repository.DriveSamples(ctx, manifest.CarID, driveCursor, 1)
	if err != nil {
		t.Fatalf("drive completion cutoff: %v", err)
	}
	if len(driveSamples) != 0 {
		t.Fatalf("drive completion cutoff returned sample %d", driveSamples[0].SampleID)
	}

	chargeCursor := initialExportCursor(manifest, exportResourceChargeSamples, manifest.ChargeSamples)
	chargeCursor.CompletedBeforeUS = 1
	chargeSamples, err := repository.ChargeSamples(ctx, manifest.CarID, chargeCursor, 1)
	if err != nil {
		t.Fatalf("charge completion cutoff: %v", err)
	}
	if len(chargeSamples) != 0 {
		t.Fatalf("charge completion cutoff returned sample %d", chargeSamples[0].SampleID)
	}
}

func assertDriveSamplePages(
	t *testing.T,
	ctx context.Context,
	repository *sqlExportRepository,
	manifest exportManifest,
) {
	t.Helper()
	cursor := initialExportCursor(manifest, exportResourceDriveSamples, manifest.DriveSamples)
	first, err := repository.DriveSamples(ctx, manifest.CarID, cursor, 5)
	if err != nil {
		t.Fatalf("first drive sample page: %v", err)
	}
	assertDriveSampleOrder(t, first, cursor)
	if len(first) == 0 {
		if manifest.DriveSamples.RowCount != 0 {
			t.Fatalf("empty drive page with manifest row count %d", manifest.DriveSamples.RowCount)
		}
		return
	}
	cursor.AfterID = first[len(first)-1].SampleID
	second, err := repository.DriveSamples(ctx, manifest.CarID, cursor, 5)
	if err != nil {
		t.Fatalf("second drive sample page: %v", err)
	}
	assertDriveSampleOrder(t, second, cursor)
	if len(second) > 0 && second[0].SampleID <= first[len(first)-1].SampleID {
		t.Fatalf("drive pages overlap: first=%d second=%d", first[len(first)-1].SampleID, second[0].SampleID)
	}
}

func assertChargeSamplePages(
	t *testing.T,
	ctx context.Context,
	repository *sqlExportRepository,
	manifest exportManifest,
) {
	t.Helper()
	cursor := initialExportCursor(manifest, exportResourceChargeSamples, manifest.ChargeSamples)
	first, err := repository.ChargeSamples(ctx, manifest.CarID, cursor, 5)
	if err != nil {
		t.Fatalf("first charge sample page: %v", err)
	}
	assertChargeSampleOrder(t, first, cursor)
	if len(first) == 0 {
		if manifest.ChargeSamples.RowCount != 0 {
			t.Fatalf("empty charge page with manifest row count %d", manifest.ChargeSamples.RowCount)
		}
		return
	}
	cursor.AfterID = first[len(first)-1].SampleID
	second, err := repository.ChargeSamples(ctx, manifest.CarID, cursor, 5)
	if err != nil {
		t.Fatalf("second charge sample page: %v", err)
	}
	assertChargeSampleOrder(t, second, cursor)
	if len(second) > 0 && second[0].SampleID <= first[len(first)-1].SampleID {
		t.Fatalf("charge pages overlap: first=%d second=%d", first[len(first)-1].SampleID, second[0].SampleID)
	}
}

func assertDriveSampleOrder(t *testing.T, samples []exportDriveSample, cursor exportCursor) {
	t.Helper()
	lastID := cursor.AfterID
	for _, sample := range samples {
		if sample.CarID != cursor.CarID || sample.SampleID <= lastID || sample.SampleID > cursor.HighWatermark {
			t.Fatalf("drive sample outside cursor bounds: %#v cursor=%#v", sample, cursor)
		}
		lastID = sample.SampleID
	}
}

func assertChargeSampleOrder(t *testing.T, samples []exportChargeSample, cursor exportCursor) {
	t.Helper()
	lastID := cursor.AfterID
	for _, sample := range samples {
		if sample.CarID != cursor.CarID || sample.SampleID <= lastID || sample.SampleID > cursor.HighWatermark {
			t.Fatalf("charge sample outside cursor bounds: %#v cursor=%#v", sample, cursor)
		}
		lastID = sample.SampleID
	}
}
