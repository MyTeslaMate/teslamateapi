package main

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type fakeExportRepository struct {
	manifestFn      func(context.Context, int) (exportManifest, error)
	driveSamplesFn  func(context.Context, int, exportCursor, int) ([]exportDriveSample, error)
	chargeSamplesFn func(context.Context, int, exportCursor, int) ([]exportChargeSample, error)
}

func (repository *fakeExportRepository) Manifest(
	ctx context.Context,
	carID int,
) (exportManifest, error) {
	if repository.manifestFn == nil {
		return exportManifest{}, errors.New("unexpected manifest call")
	}
	return repository.manifestFn(ctx, carID)
}

func (repository *fakeExportRepository) DriveSamples(
	ctx context.Context,
	carID int,
	cursor exportCursor,
	limit int,
) ([]exportDriveSample, error) {
	if repository.driveSamplesFn == nil {
		return nil, errors.New("unexpected drive samples call")
	}
	return repository.driveSamplesFn(ctx, carID, cursor, limit)
}

func (repository *fakeExportRepository) ChargeSamples(
	ctx context.Context,
	carID int,
	cursor exportCursor,
	limit int,
) ([]exportChargeSample, error) {
	if repository.chargeSamplesFn == nil {
		return nil, errors.New("unexpected charge samples call")
	}
	return repository.chargeSamplesFn(ctx, carID, cursor, limit)
}

func TestExportManifestCreatesBoundedResourceCursors(t *testing.T) {
	setExportAuthDisabled(t)
	completedBefore := time.Date(2026, time.July, 16, 12, 30, 45, 123_456_000, time.UTC)
	carName := "Example car"
	repository := &fakeExportRepository{
		manifestFn: func(_ context.Context, carID int) (exportManifest, error) {
			if carID != 1 {
				t.Fatalf("car ID: got %d", carID)
			}
			return exportManifest{
				CarID:           carID,
				CarName:         &carName,
				CompletedBefore: completedBefore,
				DriveSamples: exportResourceManifest{
					RowCount:            12,
					HighWatermark:       20,
					ParentHighWatermark: 3,
				},
				ChargeSamples: exportResourceManifest{
					RowCount:            7,
					HighWatermark:       9,
					ParentHighWatermark: 2,
				},
			}, nil
		},
	}
	key := exportCursorKey([]byte("manifest-test-key"))
	recorder := performExportRequest(
		newExportTestRouter(repository, key),
		httptest.NewRequest(http.MethodGet, "/api/v1/cars/1/export/manifest", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(recorder.Header().Get(exportRequestIDHeader)) != 32 {
		t.Fatalf("missing request ID: %q", recorder.Header().Get(exportRequestIDHeader))
	}

	var response exportManifestEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if response.Data.CarID != 1 || response.Data.CompletedBefore != exportTimestamp(completedBefore) {
		t.Fatalf("unexpected manifest data: %#v", response.Data)
	}
	if response.Data.Resources.DriveSamples.RowCount != 12 {
		t.Fatalf("drive row count: got %d", response.Data.Resources.DriveSamples.RowCount)
	}

	codec := newExportCursorCodec(key)
	driveCursor, err := codec.Decode(
		response.Data.Resources.DriveSamples.Cursor,
		exportResourceDriveSamples,
		1,
	)
	if err != nil {
		t.Fatalf("decode drive cursor: %v", err)
	}
	if driveCursor.HighWatermark != 20 ||
		driveCursor.ParentHighWatermark != 3 ||
		driveCursor.CompletedBeforeUS != completedBefore.UnixMicro() {
		t.Fatalf("unexpected drive cursor: %#v", driveCursor)
	}
	chargeCursor, err := codec.Decode(
		response.Data.Resources.ChargeSamples.Cursor,
		exportResourceChargeSamples,
		1,
	)
	if err != nil {
		t.Fatalf("decode charge cursor: %v", err)
	}
	if chargeCursor.HighWatermark != 9 || chargeCursor.ParentHighWatermark != 2 {
		t.Fatalf("unexpected charge cursor: %#v", chargeCursor)
	}
}

func TestExportDriveSamplesPagesAndAdvancesCursor(t *testing.T) {
	setExportAuthDisabled(t)
	key := exportCursorKey([]byte("page-test-key"))
	codec := newExportCursorCodec(key)
	cursor := validTestExportCursor(exportResourceDriveSamples)
	token, err := codec.Encode(cursor)
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	repository := &fakeExportRepository{
		driveSamplesFn: func(_ context.Context, carID int, received exportCursor, limit int) ([]exportDriveSample, error) {
			if carID != 1 || received != cursor {
				t.Fatalf("unexpected request: car=%d cursor=%#v", carID, received)
			}
			if limit != 3 {
				t.Fatalf("repository limit: got %d want 3", limit)
			}
			return []exportDriveSample{
				{SampleID: 11, DriveID: 3, CarID: 1},
				{SampleID: 12, DriveID: 3, CarID: 1},
				{SampleID: 13, DriveID: 3, CarID: 1},
			}, nil
		},
	}
	requestPath := "/api/v1/cars/1/export/drive-samples?limit=2&cursor=" + url.QueryEscape(token)
	recorder := performExportRequest(
		newExportTestRouter(repository, key),
		httptest.NewRequest(http.MethodGet, requestPath, nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data struct {
			Resource exportResource `json:"resource"`
			Items    []struct {
				SampleID int64 `json:"sample_id"`
			} `json:"items"`
			Page exportPageInfo `json:"page"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if response.Data.Resource != exportResourceDriveSamples || len(response.Data.Items) != 2 {
		t.Fatalf("unexpected page: %#v", response.Data)
	}
	if !response.Data.Page.HasMore || response.Data.Page.NextCursor == nil {
		t.Fatalf("missing next cursor: %#v", response.Data.Page)
	}
	next, err := codec.Decode(*response.Data.Page.NextCursor, exportResourceDriveSamples, 1)
	if err != nil {
		t.Fatalf("decode next cursor: %v", err)
	}
	if next.AfterID != 12 || next.HighWatermark != cursor.HighWatermark {
		t.Fatalf("unexpected next cursor: %#v", next)
	}
}

func TestExportDriveSamplesTraversesBoundaryWithoutGaps(t *testing.T) {
	setExportAuthDisabled(t)
	key := exportCursorKey([]byte("traversal-test-key"))
	codec := newExportCursorCodec(key)
	cursor := validTestExportCursor(exportResourceDriveSamples)
	cursor.AfterID = 0
	cursor.HighWatermark = 5
	token, err := codec.Encode(cursor)
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	dataset := []exportDriveSample{
		{SampleID: 1, DriveID: 1, CarID: 1},
		{SampleID: 2, DriveID: 1, CarID: 1},
		{SampleID: 4, DriveID: 2, CarID: 1},
		{SampleID: 5, DriveID: 2, CarID: 1},
		{SampleID: 6, DriveID: 3, CarID: 1},
	}
	repository := &fakeExportRepository{
		driveSamplesFn: func(_ context.Context, _ int, received exportCursor, limit int) ([]exportDriveSample, error) {
			result := make([]exportDriveSample, 0, limit)
			for _, sample := range dataset {
				if sample.SampleID > received.AfterID && sample.SampleID <= received.HighWatermark {
					result = append(result, sample)
					if len(result) == limit {
						break
					}
				}
			}
			return result, nil
		},
	}
	router := newExportTestRouter(repository, key)
	var received []int64
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		path := "/api/v1/cars/1/export/drive-samples?limit=2&cursor=" + url.QueryEscape(token)
		recorder := performExportRequest(router, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("page %d status: got %d body=%s", pageNumber, recorder.Code, recorder.Body.String())
		}
		var response struct {
			Data struct {
				Items []struct {
					SampleID int64 `json:"sample_id"`
				} `json:"items"`
				Page exportPageInfo `json:"page"`
			} `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode page %d: %v", pageNumber, err)
		}
		for _, sample := range response.Data.Items {
			received = append(received, sample.SampleID)
		}
		if !response.Data.Page.HasMore {
			if response.Data.Page.NextCursor != nil {
				t.Fatalf("final page has a cursor: %#v", response.Data.Page)
			}
			break
		}
		if response.Data.Page.NextCursor == nil {
			t.Fatalf("page %d has no continuation cursor", pageNumber)
		}
		token = *response.Data.Page.NextCursor
	}

	want := []int64{1, 2, 4, 5}
	if len(received) != len(want) {
		t.Fatalf("received IDs: got %v want %v", received, want)
	}
	for index := range want {
		if received[index] != want[index] {
			t.Fatalf("received IDs: got %v want %v", received, want)
		}
	}
}

func TestExportChargeSamplesReturnsEmptyFinalPage(t *testing.T) {
	setExportAuthDisabled(t)
	key := exportCursorKey([]byte("empty-page-key"))
	codec := newExportCursorCodec(key)
	cursor := validTestExportCursor(exportResourceChargeSamples)
	token, err := codec.Encode(cursor)
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}
	repository := &fakeExportRepository{
		chargeSamplesFn: func(_ context.Context, _ int, _ exportCursor, _ int) ([]exportChargeSample, error) {
			return []exportChargeSample{}, nil
		},
	}
	path := "/api/v1/cars/1/export/charge-samples?cursor=" + url.QueryEscape(token)
	recorder := performExportRequest(newExportTestRouter(repository, key), httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data struct {
			Items []json.RawMessage `json:"items"`
			Page  exportPageInfo    `json:"page"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(response.Data.Items) != 0 || response.Data.Page.HasMore || response.Data.Page.NextCursor != nil {
		t.Fatalf("unexpected final page: %#v", response.Data)
	}
}

func TestExportRequestValidation(t *testing.T) {
	setExportAuthDisabled(t)
	key := exportCursorKey([]byte("validation-key"))
	codec := newExportCursorCodec(key)
	driveToken, err := codec.Encode(validTestExportCursor(exportResourceDriveSamples))
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}
	chargeToken, err := codec.Encode(validTestExportCursor(exportResourceChargeSamples))
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	tests := []struct {
		name string
		path string
		code string
	}{
		{name: "invalid car text", path: "/api/v1/cars/abc/export/manifest", code: "invalid_car_id"},
		{name: "invalid car zero", path: "/api/v1/cars/0/export/manifest", code: "invalid_car_id"},
		{name: "car exceeds database type", path: "/api/v1/cars/32768/export/manifest", code: "invalid_car_id"},
		{name: "missing cursor", path: "/api/v1/cars/1/export/drive-samples", code: "invalid_cursor"},
		{name: "invalid limit", path: "/api/v1/cars/1/export/drive-samples?limit=-1&cursor=" + driveToken, code: "invalid_limit"},
		{name: "malformed cursor", path: "/api/v1/cars/1/export/drive-samples?cursor=bad", code: "invalid_cursor"},
		{name: "cross resource cursor", path: "/api/v1/cars/1/export/drive-samples?cursor=" + chargeToken, code: "invalid_cursor"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := performExportRequest(
				newExportTestRouter(&fakeExportRepository{}, key),
				httptest.NewRequest(http.MethodGet, test.path, nil),
			)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d body=%s", recorder.Code, recorder.Body.String())
			}
			problem := decodeExportProblem(t, recorder)
			if problem.Code != test.code {
				t.Fatalf("code: got %q want %q", problem.Code, test.code)
			}
		})
	}
}

func TestExportLimitIsCapped(t *testing.T) {
	setExportAuthDisabled(t)
	key := exportCursorKey([]byte("limit-key"))
	codec := newExportCursorCodec(key)
	token, err := codec.Encode(validTestExportCursor(exportResourceDriveSamples))
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}
	repository := &fakeExportRepository{
		driveSamplesFn: func(_ context.Context, _ int, _ exportCursor, limit int) ([]exportDriveSample, error) {
			if limit != exportMaximumPageLimit+1 {
				t.Fatalf("repository limit: got %d", limit)
			}
			return []exportDriveSample{}, nil
		},
	}
	path := "/api/v1/cars/1/export/drive-samples?limit=999999&cursor=" + token
	recorder := performExportRequest(newExportTestRouter(repository, key), httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestExportRepositoryErrorsMapWithoutLeakingDetails(t *testing.T) {
	setExportAuthDisabled(t)
	key := exportCursorKey([]byte("error-key"))
	tests := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
	}{
		{name: "missing car", err: errExportCarNotFound, status: http.StatusNotFound, code: "car_not_found"},
		{name: "transient", err: driver.ErrBadConn, status: http.StatusServiceUnavailable, code: "database_unavailable", retryable: true},
		{name: "internal", err: errors.New("secret SQL detail"), status: http.StatusInternalServerError, code: "internal_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := &fakeExportRepository{
				manifestFn: func(_ context.Context, _ int) (exportManifest, error) {
					return exportManifest{}, test.err
				},
			}
			recorder := performExportRequest(
				newExportTestRouter(repository, key),
				httptest.NewRequest(http.MethodGet, "/api/v1/cars/1/export/manifest", nil),
			)
			if recorder.Code != test.status {
				t.Fatalf("status: got %d body=%s", recorder.Code, recorder.Body.String())
			}
			problem := decodeExportProblem(t, recorder)
			if problem.Code != test.code || problem.Retryable != test.retryable {
				t.Fatalf("unexpected problem: %#v", problem)
			}
			if strings.Contains(recorder.Body.String(), "secret SQL detail") {
				t.Fatalf("internal detail leaked: %s", recorder.Body.String())
			}
		})
	}
}

func TestExportRequiresAuthentication(t *testing.T) {
	t.Setenv("API_TOKEN_DISABLE", "false")
	previousToken := envToken
	envToken = strings.Repeat("a", 32)
	t.Cleanup(func() { envToken = previousToken })
	key := exportCursorKey([]byte("auth-key"))
	repository := &fakeExportRepository{
		manifestFn: func(_ context.Context, carID int) (exportManifest, error) {
			return exportManifest{CarID: carID, CompletedBefore: time.Now().UTC()}, nil
		},
	}
	router := newExportTestRouter(repository, key)

	unauthorized := performExportRequest(
		router,
		httptest.NewRequest(http.MethodGet, "/api/v1/cars/1/export/manifest", nil),
	)
	if unauthorized.Code != http.StatusUnauthorized || decodeExportProblem(t, unauthorized).Code != "unauthorized" {
		t.Fatalf("unexpected unauthorized response: %d %s", unauthorized.Code, unauthorized.Body.String())
	}
	if challenge := unauthorized.Header().Get("WWW-Authenticate"); challenge != "Bearer" {
		t.Fatalf("authentication challenge: got %q", challenge)
	}

	authorizedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/cars/1/export/manifest", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer "+envToken)
	authorized := performExportRequest(router, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status: got %d body=%s", authorized.Code, authorized.Body.String())
	}
}

func TestExportUnavailableWithoutCursorSecret(t *testing.T) {
	setExportAuthDisabled(t)
	recorder := performExportRequest(
		newExportTestRouter(&fakeExportRepository{}, nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/cars/1/export/manifest", nil),
	)
	if recorder.Code != http.StatusServiceUnavailable || decodeExportProblem(t, recorder).Code != "export_unavailable" {
		t.Fatalf("unexpected response: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestExportQueryTimeoutReturnsRetryableProblem(t *testing.T) {
	setExportAuthDisabled(t)
	key := exportCursorKey([]byte("timeout-key"))
	repository := &fakeExportRepository{
		manifestFn: func(ctx context.Context, _ int) (exportManifest, error) {
			<-ctx.Done()
			return exportManifest{}, ctx.Err()
		},
	}
	recorder := performExportRequest(
		newExportTestRouterWithConfig(repository, key, exportRuntimeConfig{
			QueryTimeout:         20 * time.Millisecond,
			MaxConcurrentQueries: 1,
		}),
		httptest.NewRequest(http.MethodGet, "/api/v1/cars/1/export/manifest", nil),
	)
	problem := decodeExportProblem(t, recorder)
	if recorder.Code != http.StatusServiceUnavailable || problem.Code != "request_timeout" || !problem.Retryable {
		t.Fatalf("unexpected timeout response: %d %#v", recorder.Code, problem)
	}
}

func TestExportQueryConcurrencyWaitIsBounded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/cars/1/export/manifest", nil)
	handler := &exportHandler{
		queryTimeout: 10 * time.Millisecond,
		querySlots:   make(chan struct{}, 1),
	}
	handler.querySlots <- struct{}{}

	ctx, release, ok := handler.acquireQuery(c, "test")
	if ok || ctx != nil || release != nil {
		t.Fatal("query acquired a full concurrency slot")
	}
	problem := decodeExportProblem(t, recorder)
	if recorder.Code != http.StatusServiceUnavailable || problem.Code != "request_timeout" {
		t.Fatalf("unexpected wait timeout: %d %#v", recorder.Code, problem)
	}
}

func TestExportCanceledRequestDoesNotReachRepository(t *testing.T) {
	setExportAuthDisabled(t)
	key := exportCursorKey([]byte("cancel-key"))
	codec := newExportCursorCodec(key)
	token, err := codec.Encode(validTestExportCursor(exportResourceDriveSamples))
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}
	repositoryCalled := false
	repository := &fakeExportRepository{
		driveSamplesFn: func(ctx context.Context, _ int, _ exportCursor, _ int) ([]exportDriveSample, error) {
			repositoryCalled = true
			return nil, ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/cars/1/export/drive-samples?cursor="+token,
		nil,
	).WithContext(ctx)
	recorder := performExportRequest(newExportTestRouter(repository, key), request)
	if repositoryCalled {
		t.Fatal("repository received an already canceled request")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("canceled request wrote a response: %s", recorder.Body.String())
	}
}

func TestLegacyErrorResponseRemainsHTTP200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/legacy", nil)
	TeslaMateAPIHandleErrorResponse(c, "legacy", "legacy error", "driver detail")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "{\"error\":\"legacy error\"}" {
		t.Fatalf("legacy response changed: %d %s", recorder.Code, recorder.Body.String())
	}
}

func newExportTestRouter(repository exportRepository, key []byte) *gin.Engine {
	return newExportTestRouterWithConfig(repository, key, exportRuntimeConfig{
		QueryTimeout:         exportDefaultQueryTimeout,
		MaxConcurrentQueries: exportDefaultMaxConcurrentQueries,
	})
}

func newExportTestRouterWithConfig(
	repository exportRepository,
	key []byte,
	config exportRuntimeConfig,
) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	v1 := router.Group("/api/v1")
	registerTeslaMateAPIExportRoutes(v1, repository, key, config)
	return router
}

func performExportRequest(router http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func setExportAuthDisabled(t *testing.T) {
	t.Helper()
	t.Setenv("API_TOKEN_DISABLE", "true")
}

func validTestExportCursor(resource exportResource) exportCursor {
	return exportCursor{
		Version:             exportCursorVersion,
		CarID:               1,
		Resource:            resource,
		AfterID:             10,
		HighWatermark:       100,
		ParentHighWatermark: 20,
		CompletedBeforeUS:   time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC).UnixMicro(),
	}
}

func decodeExportProblem(t *testing.T, recorder *httptest.ResponseRecorder) exportProblem {
	t.Helper()
	var problem exportProblem
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v body=%s", err, recorder.Body.String())
	}
	return problem
}
