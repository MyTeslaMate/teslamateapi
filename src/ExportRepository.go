package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type exportResourceManifest struct {
	RowCount            int64
	HighWatermark       int64
	ParentHighWatermark int64
}

type exportManifest struct {
	CarID           int
	CarName         *string
	CompletedBefore time.Time
	DriveSamples    exportResourceManifest
	ChargeSamples   exportResourceManifest
}

type exportRepository interface {
	Manifest(ctx context.Context, carID int) (exportManifest, error)
	DriveSamples(
		ctx context.Context,
		carID int,
		cursor exportCursor,
		limit int,
	) ([]exportDriveSample, error)
	ChargeSamples(
		ctx context.Context,
		carID int,
		cursor exportCursor,
		limit int,
	) ([]exportChargeSample, error)
}

type sqlExportRepository struct {
	db *sql.DB
}

type exportNullInt64 struct {
	sql.NullInt64
}

func (value exportNullInt64) MarshalJSON() ([]byte, error) {
	if !value.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(value.Int64)
}

type exportNullFloat64 struct {
	sql.NullFloat64
}

func (value exportNullFloat64) MarshalJSON() ([]byte, error) {
	if !value.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(value.Float64)
}

type exportNullBool struct {
	sql.NullBool
}

func (value exportNullBool) MarshalJSON() ([]byte, error) {
	if !value.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(value.Bool)
}

type exportNullString struct {
	sql.NullString
}

func (value exportNullString) MarshalJSON() ([]byte, error) {
	if !value.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(value.String)
}

type exportDriveSample struct {
	SampleID              int64             `json:"sample_id"`
	DriveID               int64             `json:"drive_id"`
	CarID                 int               `json:"car_id"`
	RecordedAt            string            `json:"recorded_at"`
	Latitude              float64           `json:"latitude"`
	Longitude             float64           `json:"longitude"`
	SpeedKPH              exportNullInt64   `json:"speed_kph"`
	PowerKW               int64             `json:"power_kw"`
	OdometerKM            exportNullFloat64 `json:"odometer_km"`
	BatteryLevel          exportNullInt64   `json:"battery_level_percent"`
	UsableBatteryLevel    exportNullInt64   `json:"usable_battery_level_percent"`
	ElevationM            exportNullInt64   `json:"elevation_m"`
	InsideTempC           exportNullFloat64 `json:"inside_temp_c"`
	OutsideTempC          exportNullFloat64 `json:"outside_temp_c"`
	IsClimateOn           exportNullBool    `json:"is_climate_on"`
	FanStatus             exportNullInt64   `json:"fan_status"`
	DriverTempSettingC    exportNullFloat64 `json:"driver_temp_setting_c"`
	PassengerTempSettingC exportNullFloat64 `json:"passenger_temp_setting_c"`
	IsRearDefrosterOn     exportNullBool    `json:"is_rear_defroster_on"`
	IsFrontDefrosterOn    exportNullBool    `json:"is_front_defroster_on"`
	EstBatteryRangeKM     exportNullFloat64 `json:"est_battery_range_km"`
	IdealBatteryRangeKM   exportNullFloat64 `json:"ideal_battery_range_km"`
	RatedBatteryRangeKM   exportNullFloat64 `json:"rated_battery_range_km"`
	BatteryHeater         exportNullBool    `json:"battery_heater"`
	BatteryHeaterOn       exportNullBool    `json:"battery_heater_on"`
	BatteryHeaterNoPower  exportNullBool    `json:"battery_heater_no_power"`
}

type exportChargeSample struct {
	SampleID              int64             `json:"sample_id"`
	ChargeID              int64             `json:"charge_id"`
	CarID                 int               `json:"car_id"`
	RecordedAt            string            `json:"recorded_at"`
	BatteryLevel          exportNullInt64   `json:"battery_level_percent"`
	UsableBatteryLevel    exportNullInt64   `json:"usable_battery_level_percent"`
	ChargeEnergyAddedKWh  float64           `json:"charge_energy_added_kwh"`
	NotEnoughPowerToHeat  exportNullBool    `json:"not_enough_power_to_heat"`
	ChargerActualCurrentA exportNullInt64   `json:"charger_actual_current_a"`
	ChargerPhases         exportNullInt64   `json:"charger_phases"`
	ChargerPilotCurrentA  exportNullInt64   `json:"charger_pilot_current_a"`
	ChargerPowerKW        int64             `json:"charger_power_kw"`
	ChargerVoltageV       exportNullInt64   `json:"charger_voltage_v"`
	IdealBatteryRangeKM   float64           `json:"ideal_battery_range_km"`
	RatedBatteryRangeKM   exportNullFloat64 `json:"rated_battery_range_km"`
	BatteryHeater         exportNullBool    `json:"battery_heater"`
	BatteryHeaterOn       exportNullBool    `json:"battery_heater_on"`
	BatteryHeaterNoPower  exportNullBool    `json:"battery_heater_no_power"`
	ChargeCable           exportNullString  `json:"charge_cable"`
	FastChargerPresent    exportNullBool    `json:"fast_charger_present"`
	FastChargerBrand      exportNullString  `json:"fast_charger_brand"`
	FastChargerType       exportNullString  `json:"fast_charger_type"`
	OutsideTempC          exportNullFloat64 `json:"outside_temp_c"`
}

func (repository *sqlExportRepository) Manifest(ctx context.Context, carID int) (exportManifest, error) {
	tx, err := repository.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return exportManifest{}, fmt.Errorf("begin export manifest transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	manifest := exportManifest{CarID: carID}
	var carName sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT name, CURRENT_TIMESTAMP
		FROM cars
		WHERE id = $1;`, carID).Scan(&carName, &manifest.CompletedBefore)
	if errors.Is(err, sql.ErrNoRows) {
		return exportManifest{}, errExportCarNotFound
	}
	if err != nil {
		return exportManifest{}, fmt.Errorf("load export car: %w", err)
	}
	if carName.Valid {
		manifest.CarName = &carName.String
	}

	err = tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(MAX(p.id), 0),
			COALESCE(MAX(d.id), 0)
		FROM positions p
		JOIN drives d ON d.id = p.drive_id
		WHERE d.car_id = $1
			AND p.car_id = $1
			AND d.end_date IS NOT NULL
			AND d.end_date <= ($2::timestamptz AT TIME ZONE 'UTC')
			AND d.start_km IS NOT NULL
			AND d.end_km IS NOT NULL
			AND p.power IS NOT NULL;`, carID, manifest.CompletedBefore).Scan(
		&manifest.DriveSamples.RowCount,
		&manifest.DriveSamples.HighWatermark,
		&manifest.DriveSamples.ParentHighWatermark,
	)
	if err != nil {
		return exportManifest{}, fmt.Errorf("load drive sample export manifest: %w", err)
	}

	err = tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(MAX(c.id), 0),
			COALESCE(MAX(cp.id), 0)
		FROM charges c
		JOIN charging_processes cp ON cp.id = c.charging_process_id
		WHERE cp.car_id = $1
			AND cp.end_date IS NOT NULL
			AND cp.end_date <= ($2::timestamptz AT TIME ZONE 'UTC');`, carID, manifest.CompletedBefore).Scan(
		&manifest.ChargeSamples.RowCount,
		&manifest.ChargeSamples.HighWatermark,
		&manifest.ChargeSamples.ParentHighWatermark,
	)
	if err != nil {
		return exportManifest{}, fmt.Errorf("load charge sample export manifest: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return exportManifest{}, fmt.Errorf("commit export manifest transaction: %w", err)
	}
	manifest.CompletedBefore = manifest.CompletedBefore.UTC()
	return manifest, nil
}

func (repository *sqlExportRepository) DriveSamples(
	ctx context.Context,
	carID int,
	cursor exportCursor,
	limit int,
) ([]exportDriveSample, error) {
	rows, err := repository.db.QueryContext(ctx, `
		SELECT
			p.id,
			p.drive_id,
			p.car_id,
			p.date,
			p.latitude,
			p.longitude,
			p.speed,
			p.power,
			p.odometer,
			p.battery_level,
			p.usable_battery_level,
			p.elevation,
			p.inside_temp,
			p.outside_temp,
			p.is_climate_on,
			p.fan_status,
			p.driver_temp_setting,
			p.passenger_temp_setting,
			p.is_rear_defroster_on,
			p.is_front_defroster_on,
			p.est_battery_range_km,
			p.ideal_battery_range_km,
			p.rated_battery_range_km,
			p.battery_heater,
			p.battery_heater_on,
			p.battery_heater_no_power
		FROM positions p
		JOIN drives d ON d.id = p.drive_id
		WHERE d.car_id = $1
			AND p.car_id = $1
			AND p.id > $2
			AND p.id <= $3
			AND d.id <= $4
			AND d.end_date IS NOT NULL
			AND d.end_date <= ($5::timestamptz AT TIME ZONE 'UTC')
			AND d.start_km IS NOT NULL
			AND d.end_km IS NOT NULL
			AND p.power IS NOT NULL
		ORDER BY p.id ASC
		LIMIT $6;`,
		carID,
		cursor.AfterID,
		cursor.HighWatermark,
		cursor.ParentHighWatermark,
		time.UnixMicro(cursor.CompletedBeforeUS).UTC(),
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query drive sample export: %w", err)
	}
	defer rows.Close()

	samples := make([]exportDriveSample, 0, limit)
	for rows.Next() {
		var sample exportDriveSample
		var recordedAt time.Time
		if err := rows.Scan(
			&sample.SampleID,
			&sample.DriveID,
			&sample.CarID,
			&recordedAt,
			&sample.Latitude,
			&sample.Longitude,
			&sample.SpeedKPH,
			&sample.PowerKW,
			&sample.OdometerKM,
			&sample.BatteryLevel,
			&sample.UsableBatteryLevel,
			&sample.ElevationM,
			&sample.InsideTempC,
			&sample.OutsideTempC,
			&sample.IsClimateOn,
			&sample.FanStatus,
			&sample.DriverTempSettingC,
			&sample.PassengerTempSettingC,
			&sample.IsRearDefrosterOn,
			&sample.IsFrontDefrosterOn,
			&sample.EstBatteryRangeKM,
			&sample.IdealBatteryRangeKM,
			&sample.RatedBatteryRangeKM,
			&sample.BatteryHeater,
			&sample.BatteryHeaterOn,
			&sample.BatteryHeaterNoPower,
		); err != nil {
			return nil, fmt.Errorf("scan drive sample export: %w", err)
		}
		sample.RecordedAt = exportTimestamp(recordedAt)
		samples = append(samples, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate drive sample export: %w", err)
	}
	return samples, nil
}

func (repository *sqlExportRepository) ChargeSamples(
	ctx context.Context,
	carID int,
	cursor exportCursor,
	limit int,
) ([]exportChargeSample, error) {
	rows, err := repository.db.QueryContext(ctx, `
		SELECT
			c.id,
			c.charging_process_id,
			cp.car_id,
			c.date,
			c.battery_level,
			c.usable_battery_level,
			c.charge_energy_added,
			c.not_enough_power_to_heat,
			c.charger_actual_current,
			c.charger_phases,
			c.charger_pilot_current,
			c.charger_power,
			c.charger_voltage,
			c.ideal_battery_range_km,
			c.rated_battery_range_km,
			c.battery_heater,
			c.battery_heater_on,
			c.battery_heater_no_power,
			c.conn_charge_cable,
			c.fast_charger_present,
			c.fast_charger_brand,
			c.fast_charger_type,
			c.outside_temp
		FROM charges c
		JOIN charging_processes cp ON cp.id = c.charging_process_id
		WHERE cp.car_id = $1
			AND c.id > $2
			AND c.id <= $3
			AND cp.id <= $4
			AND cp.end_date IS NOT NULL
			AND cp.end_date <= ($5::timestamptz AT TIME ZONE 'UTC')
		ORDER BY c.id ASC
		LIMIT $6;`,
		carID,
		cursor.AfterID,
		cursor.HighWatermark,
		cursor.ParentHighWatermark,
		time.UnixMicro(cursor.CompletedBeforeUS).UTC(),
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query charge sample export: %w", err)
	}
	defer rows.Close()

	samples := make([]exportChargeSample, 0, limit)
	for rows.Next() {
		var sample exportChargeSample
		var recordedAt time.Time
		if err := rows.Scan(
			&sample.SampleID,
			&sample.ChargeID,
			&sample.CarID,
			&recordedAt,
			&sample.BatteryLevel,
			&sample.UsableBatteryLevel,
			&sample.ChargeEnergyAddedKWh,
			&sample.NotEnoughPowerToHeat,
			&sample.ChargerActualCurrentA,
			&sample.ChargerPhases,
			&sample.ChargerPilotCurrentA,
			&sample.ChargerPowerKW,
			&sample.ChargerVoltageV,
			&sample.IdealBatteryRangeKM,
			&sample.RatedBatteryRangeKM,
			&sample.BatteryHeater,
			&sample.BatteryHeaterOn,
			&sample.BatteryHeaterNoPower,
			&sample.ChargeCable,
			&sample.FastChargerPresent,
			&sample.FastChargerBrand,
			&sample.FastChargerType,
			&sample.OutsideTempC,
		); err != nil {
			return nil, fmt.Errorf("scan charge sample export: %w", err)
		}
		sample.RecordedAt = exportTimestamp(recordedAt)
		samples = append(samples, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate charge sample export: %w", err)
	}
	return samples, nil
}

func exportTimestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
