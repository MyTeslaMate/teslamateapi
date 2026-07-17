package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

const (
	exportRequestIDHeader     = "X-Request-ID"
	exportRequestIDContextKey = "teslamateapi_export_request_id"
	exportProblemTypeBase     = "https://github.com/MyTeslaMate/teslamateapi/blob/main/docs/export-api.md#problem-"
)

var (
	errExportCarNotFound = errors.New("export car not found")
	exportRequestCounter atomic.Uint64
)

type exportProblem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Instance  string `json:"instance"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	RequestID string `json:"request_id"`
}

type exportProblemSpec struct {
	Status    int
	Code      string
	Title     string
	Detail    string
	Retryable bool
}

func exportRequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := newExportRequestID()
		c.Set(exportRequestIDContextKey, requestID)
		c.Header(exportRequestIDHeader, requestID)
		c.Next()
	}
}

func exportRecoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf(
					"[error] TeslaMateAPIExport request_id=%s path=%s recovered panic type=%T",
					exportRequestID(c),
					c.Request.URL.Path,
					recovered,
				)
				if !c.Writer.Written() {
					writeExportProblem(c, exportProblemInternal())
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}

func exportAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		validToken, _ := validateAuthToken(c)
		if validToken {
			c.Next()
			return
		}

		c.Header("WWW-Authenticate", "Bearer")
		writeExportProblem(c, exportProblemSpec{
			Status:    http.StatusUnauthorized,
			Code:      "unauthorized",
			Title:     "Unauthorized",
			Detail:    "A valid API token is required.",
			Retryable: false,
		})
		c.Abort()
	}
}

func writeExportProblem(c *gin.Context, spec exportProblemSpec) {
	if c.Writer.Written() {
		return
	}
	requestID := exportRequestID(c)
	problem := exportProblem{
		Type:      exportProblemTypeBase + strings.ReplaceAll(spec.Code, "_", "-"),
		Title:     spec.Title,
		Status:    spec.Status,
		Detail:    spec.Detail,
		Instance:  "urn:teslamateapi:request:" + requestID,
		Code:      spec.Code,
		Retryable: spec.Retryable,
		RequestID: requestID,
	}
	c.Header("Content-Type", "application/problem+json")
	c.JSON(spec.Status, problem)
}

func handleExportRepositoryError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, errExportCarNotFound):
		writeExportProblem(c, exportProblemSpec{
			Status:    http.StatusNotFound,
			Code:      "car_not_found",
			Title:     "Car not found",
			Detail:    "No car exists for the requested car_id.",
			Retryable: false,
		})
	case errors.Is(err, context.DeadlineExceeded):
		writeExportProblem(c, exportProblemSpec{
			Status:    http.StatusServiceUnavailable,
			Code:      "request_timeout",
			Title:     "Request timed out",
			Detail:    "The export query did not finish in time.",
			Retryable: true,
		})
	case isTransientExportDatabaseError(err):
		writeExportProblem(c, exportProblemSpec{
			Status:    http.StatusServiceUnavailable,
			Code:      "database_unavailable",
			Title:     "Database unavailable",
			Detail:    "The database is temporarily unavailable.",
			Retryable: true,
		})
	default:
		writeExportProblem(c, exportProblemInternal())
	}
}

func exportProblemInternal() exportProblemSpec {
	return exportProblemSpec{
		Status:    http.StatusInternalServerError,
		Code:      "internal_error",
		Title:     "Internal error",
		Detail:    "The export could not be completed.",
		Retryable: false,
	}
}

func isTransientExportDatabaseError(err error) bool {
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	var postgresError *pq.Error
	if !errors.As(err, &postgresError) {
		return false
	}
	code := string(postgresError.Code)
	if len(code) >= 2 {
		switch code[:2] {
		case "08", "40", "53":
			return true
		}
	}
	switch code {
	case "55P03", "57P01", "57P02", "57P03":
		return true
	default:
		return false
	}
}

func exportRequestID(c *gin.Context) string {
	if requestID, ok := c.Get(exportRequestIDContextKey); ok {
		if value, ok := requestID.(string); ok && value != "" {
			return value
		}
	}
	requestID := newExportRequestID()
	c.Set(exportRequestIDContextKey, requestID)
	c.Header(exportRequestIDHeader, requestID)
	return requestID
}

func newExportRequestID() string {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err == nil {
		return hex.EncodeToString(random)
	}
	fallback := fmt.Sprintf("%d:%d", time.Now().UnixNano(), exportRequestCounter.Add(1))
	sum := exportCursorKey([]byte(fallback))
	return hex.EncodeToString(sum[:16])
}

func logExportError(c *gin.Context, operation string, err error) {
	message := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(err.Error())
	log.Printf(
		"[error] %s request_id=%s path=%s error=%s",
		operation,
		exportRequestID(c),
		c.Request.URL.Path,
		message,
	)
}
