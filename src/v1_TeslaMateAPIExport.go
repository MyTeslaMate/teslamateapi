package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	exportSchemaVersion               = 1
	exportDefaultPageLimit            = 5_000
	exportMaximumPageLimit            = 10_000
	exportMaximumCarID                = 32_767
	exportDefaultQueryTimeout         = 30 * time.Second
	exportDefaultMaxConcurrentQueries = 2
)

type exportHandler struct {
	repository   exportRepository
	cursors      exportCursorCodec
	queryTimeout time.Duration
	querySlots   chan struct{}
}

type exportRuntimeConfig struct {
	QueryTimeout         time.Duration
	MaxConcurrentQueries int
}

type exportManifestEnvelope struct {
	Data exportManifestData `json:"data"`
}

type exportManifestData struct {
	SchemaVersion    int                     `json:"schema_version"`
	CarID            int                     `json:"car_id"`
	CarName          *string                 `json:"car_name"`
	CompletedBefore  string                  `json:"completed_before"`
	DefaultPageLimit int                     `json:"default_page_limit"`
	MaximumPageLimit int                     `json:"maximum_page_limit"`
	Resources        exportManifestResources `json:"resources"`
}

type exportManifestResources struct {
	DriveSamples  exportManifestResource `json:"drive_samples"`
	ChargeSamples exportManifestResource `json:"charge_samples"`
}

type exportManifestResource struct {
	Endpoint      string `json:"endpoint"`
	RowCount      int64  `json:"row_count"`
	HighWatermark int64  `json:"high_watermark"`
	Cursor        string `json:"cursor"`
}

type exportPageEnvelope struct {
	Data exportPageData `json:"data"`
}

type exportPageData struct {
	SchemaVersion int            `json:"schema_version"`
	CarID         int            `json:"car_id"`
	Resource      exportResource `json:"resource"`
	HighWatermark int64          `json:"high_watermark"`
	Items         any            `json:"items"`
	Page          exportPageInfo `json:"page"`
}

type exportPageInfo struct {
	Count      int     `json:"count"`
	HasMore    bool    `json:"has_more"`
	NextCursor *string `json:"next_cursor"`
}

func registerTeslaMateAPIExportRoutes(
	v1 *gin.RouterGroup,
	repository exportRepository,
	cursorKey []byte,
	config exportRuntimeConfig,
) {
	config = normalizeExportRuntimeConfig(config)
	handler := &exportHandler{
		repository:   repository,
		cursors:      newExportCursorCodec(cursorKey),
		queryTimeout: config.QueryTimeout,
		querySlots:   make(chan struct{}, config.MaxConcurrentQueries),
	}
	export := v1.Group("/cars/:CarID/export")
	export.Use(exportRequestIDMiddleware(), exportRecoveryMiddleware(), exportAuthMiddleware())
	export.GET("/manifest", handler.Manifest)
	export.GET("/drive-samples", handler.DriveSamples)
	export.GET("/charge-samples", handler.ChargeSamples)
}

func (handler *exportHandler) Manifest(c *gin.Context) {
	carID, ok := parseExportCarID(c)
	if !ok || !handler.requireCursorConfiguration(c) {
		return
	}

	ctx, release, ok := handler.acquireQuery(c, "TeslaMateAPIExportManifestV1")
	if !ok {
		return
	}
	defer release()
	manifest, err := handler.repository.Manifest(ctx, carID)
	err = normalizeExportQueryError(ctx, err)
	release()
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logExportError(c, "TeslaMateAPIExportManifestV1", err)
		}
		handleExportRepositoryError(c, err)
		return
	}

	driveCursor, err := handler.cursors.Encode(initialExportCursor(
		manifest,
		exportResourceDriveSamples,
		manifest.DriveSamples,
	))
	if err != nil {
		logExportError(c, "TeslaMateAPIExportManifestV1", err)
		handler.handleCursorError(c, err)
		return
	}
	chargeCursor, err := handler.cursors.Encode(initialExportCursor(
		manifest,
		exportResourceChargeSamples,
		manifest.ChargeSamples,
	))
	if err != nil {
		logExportError(c, "TeslaMateAPIExportManifestV1", err)
		handler.handleCursorError(c, err)
		return
	}

	basePath := "/api/v1/cars/" + strconv.Itoa(carID) + "/export/"
	response := exportManifestEnvelope{Data: exportManifestData{
		SchemaVersion:    exportSchemaVersion,
		CarID:            carID,
		CarName:          manifest.CarName,
		CompletedBefore:  exportTimestamp(manifest.CompletedBefore),
		DefaultPageLimit: exportDefaultPageLimit,
		MaximumPageLimit: exportMaximumPageLimit,
		Resources: exportManifestResources{
			DriveSamples: exportManifestResource{
				Endpoint:      basePath + "drive-samples",
				RowCount:      manifest.DriveSamples.RowCount,
				HighWatermark: manifest.DriveSamples.HighWatermark,
				Cursor:        driveCursor,
			},
			ChargeSamples: exportManifestResource{
				Endpoint:      basePath + "charge-samples",
				RowCount:      manifest.ChargeSamples.RowCount,
				HighWatermark: manifest.ChargeSamples.HighWatermark,
				Cursor:        chargeCursor,
			},
		},
	}}
	c.JSON(http.StatusOK, response)
	log.Printf(
		"[info] TeslaMateAPIExportManifestV1 request_id=%s path=%s executed successfully",
		exportRequestID(c),
		c.Request.URL.Path,
	)
}

func (handler *exportHandler) DriveSamples(c *gin.Context) {
	carID, cursor, limit, ok := handler.pageRequest(c, exportResourceDriveSamples)
	if !ok {
		return
	}

	ctx, release, ok := handler.acquireQuery(c, "TeslaMateAPIExportDriveSamplesV1")
	if !ok {
		return
	}
	defer release()
	samples, err := handler.repository.DriveSamples(ctx, carID, cursor, limit+1)
	err = normalizeExportQueryError(ctx, err)
	release()
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logExportError(c, "TeslaMateAPIExportDriveSamplesV1", err)
		}
		handleExportRepositoryError(c, err)
		return
	}

	hasMore := len(samples) > limit
	if hasMore {
		samples = samples[:limit]
	}
	nextCursor, ok := handler.nextCursor(c, cursor, hasMore, lastDriveSampleID(samples))
	if !ok {
		return
	}
	handler.writePage(c, carID, exportResourceDriveSamples, cursor.HighWatermark, samples, nextCursor, hasMore)
}

func (handler *exportHandler) ChargeSamples(c *gin.Context) {
	carID, cursor, limit, ok := handler.pageRequest(c, exportResourceChargeSamples)
	if !ok {
		return
	}

	ctx, release, ok := handler.acquireQuery(c, "TeslaMateAPIExportChargeSamplesV1")
	if !ok {
		return
	}
	defer release()
	samples, err := handler.repository.ChargeSamples(ctx, carID, cursor, limit+1)
	err = normalizeExportQueryError(ctx, err)
	release()
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logExportError(c, "TeslaMateAPIExportChargeSamplesV1", err)
		}
		handleExportRepositoryError(c, err)
		return
	}

	hasMore := len(samples) > limit
	if hasMore {
		samples = samples[:limit]
	}
	nextCursor, ok := handler.nextCursor(c, cursor, hasMore, lastChargeSampleID(samples))
	if !ok {
		return
	}
	handler.writePage(c, carID, exportResourceChargeSamples, cursor.HighWatermark, samples, nextCursor, hasMore)
}

func (handler *exportHandler) pageRequest(
	c *gin.Context,
	resource exportResource,
) (int, exportCursor, int, bool) {
	carID, ok := parseExportCarID(c)
	if !ok || !handler.requireCursorConfiguration(c) {
		return 0, exportCursor{}, 0, false
	}
	limit, ok := parseExportLimit(c)
	if !ok {
		return 0, exportCursor{}, 0, false
	}
	token := c.Query("cursor")
	if token == "" {
		writeExportProblem(c, exportProblemSpec{
			Status:    http.StatusBadRequest,
			Code:      "invalid_cursor",
			Title:     "Invalid cursor",
			Detail:    "A cursor from the export manifest is required.",
			Retryable: false,
		})
		return 0, exportCursor{}, 0, false
	}
	cursor, err := handler.cursors.Decode(token, resource, carID)
	if err != nil {
		handler.handleCursorError(c, err)
		return 0, exportCursor{}, 0, false
	}
	return carID, cursor, limit, true
}

func (handler *exportHandler) nextCursor(
	c *gin.Context,
	cursor exportCursor,
	hasMore bool,
	lastID int64,
) (*string, bool) {
	if !hasMore {
		return nil, true
	}
	if lastID <= cursor.AfterID || lastID > cursor.HighWatermark {
		writeExportProblem(c, exportProblemInternal())
		return nil, false
	}
	cursor.AfterID = lastID
	token, err := handler.cursors.Encode(cursor)
	if err != nil {
		logExportError(c, "TeslaMateAPIExportNextCursorV1", err)
		handler.handleCursorError(c, err)
		return nil, false
	}
	return &token, true
}

func (handler *exportHandler) writePage(
	c *gin.Context,
	carID int,
	resource exportResource,
	highWatermark int64,
	items any,
	nextCursor *string,
	hasMore bool,
) {
	count := 0
	switch values := items.(type) {
	case []exportDriveSample:
		count = len(values)
	case []exportChargeSample:
		count = len(values)
	}
	c.JSON(http.StatusOK, exportPageEnvelope{Data: exportPageData{
		SchemaVersion: exportSchemaVersion,
		CarID:         carID,
		Resource:      resource,
		HighWatermark: highWatermark,
		Items:         items,
		Page: exportPageInfo{
			Count:      count,
			HasMore:    hasMore,
			NextCursor: nextCursor,
		},
	}})
	log.Printf(
		"[info] TeslaMateAPIExportPageV1 request_id=%s path=%s resource=%s rows=%d executed successfully",
		exportRequestID(c),
		c.Request.URL.Path,
		resource,
		count,
	)
}

func (handler *exportHandler) requireCursorConfiguration(c *gin.Context) bool {
	if len(handler.cursors.key) != 0 {
		return true
	}
	writeExportProblem(c, exportProblemSpec{
		Status:    http.StatusServiceUnavailable,
		Code:      "export_unavailable",
		Title:     "Export unavailable",
		Detail:    "Export cursors are not configured on this server.",
		Retryable: false,
	})
	return false
}

func (handler *exportHandler) acquireQuery(
	c *gin.Context,
	operation string,
) (context.Context, func(), bool) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), handler.queryTimeout)
	if err := ctx.Err(); err != nil {
		cancel()
		handleExportRepositoryError(c, err)
		return nil, nil, false
	}

	select {
	case handler.querySlots <- struct{}{}:
		var once sync.Once
		return ctx, func() {
			once.Do(func() {
				<-handler.querySlots
				cancel()
			})
		}, true
	case <-ctx.Done():
		err := ctx.Err()
		cancel()
		if !errors.Is(err, context.Canceled) {
			logExportError(c, operation, err)
		}
		handleExportRepositoryError(c, err)
		return nil, nil, false
	}
}

func normalizeExportQueryError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if contextError := ctx.Err(); contextError != nil {
		return contextError
	}
	return err
}

func (handler *exportHandler) handleCursorError(c *gin.Context, err error) {
	if errors.Is(err, errExportCursorUnavailable) {
		_ = handler.requireCursorConfiguration(c)
		return
	}
	writeExportProblem(c, exportProblemSpec{
		Status:    http.StatusBadRequest,
		Code:      "invalid_cursor",
		Title:     "Invalid cursor",
		Detail:    "The export cursor is invalid or does not match this resource.",
		Retryable: false,
	})
}

func parseExportCarID(c *gin.Context) (int, bool) {
	carID, err := strconv.Atoi(c.Param("CarID"))
	if err == nil && carID > 0 && carID <= exportMaximumCarID {
		return carID, true
	}
	writeExportProblem(c, exportProblemSpec{
		Status:    http.StatusBadRequest,
		Code:      "invalid_car_id",
		Title:     "Invalid car ID",
		Detail:    "car_id must be an integer between 1 and 32767.",
		Retryable: false,
	})
	return 0, false
}

func parseExportLimit(c *gin.Context) (int, bool) {
	rawLimit := c.Query("limit")
	if rawLimit == "" {
		return exportDefaultPageLimit, true
	}
	limit, err := strconv.Atoi(rawLimit)
	if err != nil || limit <= 0 {
		writeExportProblem(c, exportProblemSpec{
			Status:    http.StatusBadRequest,
			Code:      "invalid_limit",
			Title:     "Invalid limit",
			Detail:    "limit must be a positive integer.",
			Retryable: false,
		})
		return 0, false
	}
	if limit > exportMaximumPageLimit {
		limit = exportMaximumPageLimit
	}
	return limit, true
}

func initialExportCursor(
	manifest exportManifest,
	resource exportResource,
	resourceManifest exportResourceManifest,
) exportCursor {
	return exportCursor{
		Version:             exportCursorVersion,
		CarID:               manifest.CarID,
		Resource:            resource,
		AfterID:             0,
		HighWatermark:       resourceManifest.HighWatermark,
		ParentHighWatermark: resourceManifest.ParentHighWatermark,
		CompletedBeforeUS:   manifest.CompletedBefore.UTC().UnixMicro(),
	}
}

func exportRuntimeConfigFromEnv() exportRuntimeConfig {
	return normalizeExportRuntimeConfig(exportRuntimeConfig{
		QueryTimeout:         time.Duration(getEnvAsInt("EXPORT_QUERY_TIMEOUT", 30_000)) * time.Millisecond,
		MaxConcurrentQueries: getEnvAsInt("EXPORT_MAX_CONCURRENT_QUERIES", exportDefaultMaxConcurrentQueries),
	})
}

func normalizeExportRuntimeConfig(config exportRuntimeConfig) exportRuntimeConfig {
	if config.QueryTimeout <= 0 {
		config.QueryTimeout = exportDefaultQueryTimeout
	}
	if config.MaxConcurrentQueries <= 0 {
		config.MaxConcurrentQueries = exportDefaultMaxConcurrentQueries
	}
	return config
}

func lastDriveSampleID(samples []exportDriveSample) int64 {
	if len(samples) == 0 {
		return 0
	}
	return samples[len(samples)-1].SampleID
}

func lastChargeSampleID(samples []exportChargeSample) int64 {
	if len(samples) == 0 {
		return 0
	}
	return samples[len(samples)-1].SampleID
}
