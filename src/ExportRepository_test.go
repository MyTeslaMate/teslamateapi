package main

import (
	"database/sql"
	"encoding/json"
	"testing"
)

func TestExportNullableValuesPreserveNull(t *testing.T) {
	present := exportDriveSample{
		SpeedKPH: exportNullInt64{sql.NullInt64{Int64: 42, Valid: true}},
	}
	missing := exportChargeSample{
		ChargeCable: exportNullString{sql.NullString{Valid: false}},
	}
	presentJSON, err := json.Marshal(present)
	if err != nil {
		t.Fatalf("marshal present sample: %v", err)
	}
	missingJSON, err := json.Marshal(missing)
	if err != nil {
		t.Fatalf("marshal missing sample: %v", err)
	}
	if !containsString(string(presentJSON), `"speed_kph":42`) {
		t.Fatalf("present number was not preserved: %s", presentJSON)
	}
	if !containsString(string(missingJSON), `"charge_cable":null`) {
		t.Fatalf("SQL NULL was not preserved: %s", missingJSON)
	}
}
