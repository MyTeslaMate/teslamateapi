package main

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

func TestExportProblemUsesStatusAndMachineReadableContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(exportRequestIDMiddleware())
	router.GET("/problem", func(c *gin.Context) {
		writeExportProblem(c, exportProblemSpec{
			Status:    http.StatusBadRequest,
			Code:      "invalid_cursor",
			Title:     "Invalid cursor",
			Detail:    "The export cursor is invalid.",
			Retryable: false,
		})
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/problem", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want %d", recorder.Code, http.StatusBadRequest)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("content type: got %q", contentType)
	}
	requestID := recorder.Header().Get(exportRequestIDHeader)
	if len(requestID) != 32 {
		t.Fatalf("request ID length: got %d", len(requestID))
	}
	var problem exportProblem
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Status != recorder.Code || problem.Code != "invalid_cursor" || problem.RequestID != requestID {
		t.Fatalf("unexpected problem: %#v", problem)
	}
	if problem.Type != exportProblemTypeBase+"invalid-cursor" {
		t.Fatalf("problem type: got %q", problem.Type)
	}
}

func TestExportRecoveryRedactsPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(exportRequestIDMiddleware(), exportRecoveryMiddleware())
	router.GET("/panic", func(_ *gin.Context) {
		panic("database password must stay private")
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d", recorder.Code)
	}
	if string(recorder.Body.Bytes()) == "" || containsString(recorder.Body.String(), "password") {
		t.Fatalf("panic detail leaked: %s", recorder.Body.String())
	}
}

func TestTransientExportDatabaseErrors(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{name: "bad connection", err: driver.ErrBadConn, transient: true},
		{name: "connection exception", err: &pq.Error{Code: "08006"}, transient: true},
		{name: "serialization", err: &pq.Error{Code: "40001"}, transient: true},
		{name: "resource", err: &pq.Error{Code: "53300"}, transient: true},
		{name: "admin shutdown", err: &pq.Error{Code: "57P01"}, transient: true},
		{name: "schema error", err: &pq.Error{Code: "42P01"}, transient: false},
		{name: "generic", err: errors.New("generic"), transient: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isTransientExportDatabaseError(test.err); got != test.transient {
				t.Fatalf("transient: got %t want %t", got, test.transient)
			}
		})
	}
}

func containsString(value, substring string) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}
