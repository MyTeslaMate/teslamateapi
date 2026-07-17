package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExportCursorRoundTripAndRestart(t *testing.T) {
	key := exportCursorKey([]byte("stable-test-secret"))
	firstCodec := newExportCursorCodec(key)
	cursor := exportCursor{
		Version:             exportCursorVersion,
		CarID:               7,
		Resource:            exportResourceDriveSamples,
		AfterID:             123,
		HighWatermark:       999,
		ParentHighWatermark: 42,
		CompletedBeforeUS:   time.Now().UTC().UnixMicro(),
	}

	token, err := firstCodec.Encode(cursor)
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	restartedCodec := newExportCursorCodec(key)
	decoded, err := restartedCodec.Decode(token, exportResourceDriveSamples, 7)
	if err != nil {
		t.Fatalf("decode cursor after codec restart: %v", err)
	}
	if decoded != cursor {
		t.Fatalf("decoded cursor mismatch: got %#v want %#v", decoded, cursor)
	}
}

func TestExportCursorRejectsInvalidTokens(t *testing.T) {
	codec := newExportCursorCodec(exportCursorKey([]byte("test-secret")))
	cursor := exportCursor{
		Version:             exportCursorVersion,
		CarID:               1,
		Resource:            exportResourceChargeSamples,
		AfterID:             10,
		HighWatermark:       20,
		ParentHighWatermark: 3,
		CompletedBeforeUS:   1_700_000_000_000_000,
	}
	token, err := codec.Encode(cursor)
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	tampered := token[:len(token)-1] + differentCursorCharacter(token[len(token)-1])
	tests := []struct {
		name     string
		token    string
		resource exportResource
		carID    int
	}{
		{name: "empty", token: "", resource: exportResourceChargeSamples, carID: 1},
		{name: "truncated", token: strings.TrimSuffix(token, token[len(token)-8:]), resource: exportResourceChargeSamples, carID: 1},
		{name: "tampered", token: tampered, resource: exportResourceChargeSamples, carID: 1},
		{name: "wrong resource", token: token, resource: exportResourceDriveSamples, carID: 1},
		{name: "wrong car", token: token, resource: exportResourceChargeSamples, carID: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := codec.Decode(test.token, test.resource, test.carID)
			if !errors.Is(err, errExportCursorInvalid) {
				t.Fatalf("expected invalid cursor, got %v", err)
			}
		})
	}

	wrongCodec := newExportCursorCodec(exportCursorKey([]byte("different-secret")))
	if _, err := wrongCodec.Decode(token, exportResourceChargeSamples, 1); !errors.Is(err, errExportCursorInvalid) {
		t.Fatalf("wrong key: expected invalid cursor, got %v", err)
	}
}

func TestExportCursorRejectsInvalidState(t *testing.T) {
	codec := newExportCursorCodec(exportCursorKey([]byte("test-secret")))
	tests := []exportCursor{
		{Version: 2, CarID: 1, Resource: exportResourceDriveSamples, CompletedBeforeUS: 1},
		{Version: 1, CarID: 0, Resource: exportResourceDriveSamples, CompletedBeforeUS: 1},
		{Version: 1, CarID: 1, Resource: "unknown", CompletedBeforeUS: 1},
		{Version: 1, CarID: 1, Resource: exportResourceDriveSamples, AfterID: 2, HighWatermark: 1, CompletedBeforeUS: 1},
		{Version: 1, CarID: 1, Resource: exportResourceDriveSamples, AfterID: -1, CompletedBeforeUS: 1},
		{Version: 1, CarID: 1, Resource: exportResourceDriveSamples, CompletedBeforeUS: 0},
	}
	for _, cursor := range tests {
		if _, err := codec.Encode(cursor); !errors.Is(err, errExportCursorInvalid) {
			t.Fatalf("expected invalid cursor for %#v, got %v", cursor, err)
		}
	}
}

func TestExportCursorRequiresConfiguredKey(t *testing.T) {
	codec := newExportCursorCodec(nil)
	_, err := codec.Encode(exportCursor{
		Version:           exportCursorVersion,
		CarID:             1,
		Resource:          exportResourceDriveSamples,
		CompletedBeforeUS: 1,
	})
	if !errors.Is(err, errExportCursorUnavailable) {
		t.Fatalf("expected unavailable cursor codec, got %v", err)
	}
}

func differentCursorCharacter(value byte) string {
	if value == 'A' {
		return "B"
	}
	return "A"
}
