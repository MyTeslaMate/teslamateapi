# Export API

The export API provides bounded, resumable access to drive and charge samples. It is intended for bulk sample export, downstream analysis, and integrations that need to move many sample rows without fetching one detail response for every drive or charging process.

This is not a complete TeslaMate database backup. It exports a documented subset of samples under eligible completed parents. Parent records, non-drive positions, and other TeslaMate data remain outside this surface.

This is an additive v1 surface. Existing endpoints and their error behavior are unchanged.

## Configuration

Export cursors are signed with `EXPORT_CURSOR_SECRET`. If that variable is not set, the API derives a separate cursor key from `ENCRYPTION_KEY`.

The secret must remain stable while an export is in progress. Changing it invalidates existing cursors. The API returns `503 export_unavailable` if neither variable is configured.

Export database work is separately bounded:

| Variable | Default | Meaning |
| --- | ---: | --- |
| `EXPORT_QUERY_TIMEOUT` | `30000` | Total milliseconds allowed for an export query, including time waiting for a concurrency slot |
| `EXPORT_MAX_CONCURRENT_QUERIES` | `2` | Maximum export queries allowed to use the database at once |

Values less than one use the default. These limits apply only to the export routes.

## Endpoints

```text
GET /api/v1/cars/:CarID/export/manifest
GET /api/v1/cars/:CarID/export/drive-samples?cursor=...&limit=5000
GET /api/v1/cars/:CarID/export/charge-samples?cursor=...&limit=5000
```

All three endpoints use the existing API token authentication. `CarID` must be between 1 and 32767, matching the TeslaMate database type.

### 1. Create a manifest

Request the manifest first:

```http
GET /api/v1/cars/1/export/manifest
Authorization: Bearer <token>
```

Example response:

```json
{
  "data": {
    "schema_version": 1,
    "car_id": 1,
    "car_name": "Example car",
    "completed_before": "2026-07-16T12:30:45.123456Z",
    "default_page_limit": 5000,
    "maximum_page_limit": 10000,
    "resources": {
      "drive_samples": {
        "endpoint": "/api/v1/cars/1/export/drive-samples",
        "row_count": 12000,
        "high_watermark": 13000,
        "cursor": "<opaque cursor>"
      },
      "charge_samples": {
        "endpoint": "/api/v1/cars/1/export/charge-samples",
        "row_count": 1,
        "high_watermark": 31,
        "cursor": "<opaque cursor>"
      }
    }
  }
}
```

`completed_before` is the completion cutoff captured for this export. Each cursor is restricted to one car, one resource, and the manifest boundary. Cursors are opaque. Clients must not create or modify them.

### 2. Read drive sample pages

Pass the drive cursor back to its endpoint:

```http
GET /api/v1/cars/1/export/drive-samples?cursor=<opaque cursor>&limit=1
Authorization: Bearer <token>
```

Example response:

```json
{
  "data": {
    "schema_version": 1,
    "car_id": 1,
    "resource": "drive_samples",
    "high_watermark": 13000,
    "items": [
      {
        "sample_id": 101,
        "drive_id": 12,
        "car_id": 1,
        "recorded_at": "2026-07-10T08:15:01.234Z",
        "latitude": 51.5074,
        "longitude": -0.1278,
        "speed_kph": 48,
        "power_kw": 12,
        "odometer_km": 42123.4,
        "battery_level_percent": 74,
        "usable_battery_level_percent": 72,
        "elevation_m": 18,
        "inside_temp_c": 20.5,
        "outside_temp_c": 16.0,
        "is_climate_on": true,
        "fan_status": 3,
        "driver_temp_setting_c": 20.0,
        "passenger_temp_setting_c": 20.0,
        "is_rear_defroster_on": false,
        "is_front_defroster_on": false,
        "est_battery_range_km": 310.2,
        "ideal_battery_range_km": 320.1,
        "rated_battery_range_km": 298.7,
        "battery_heater": false,
        "battery_heater_on": false,
        "battery_heater_no_power": null
      }
    ],
    "page": {
      "count": 1,
      "has_more": true,
      "next_cursor": "<opaque cursor>"
    }
  }
}
```

### 3. Read charge sample pages

Pass the charge cursor back to its endpoint:

```http
GET /api/v1/cars/1/export/charge-samples?cursor=<opaque cursor>&limit=5000
Authorization: Bearer <token>
```

Example response:

```json
{
  "data": {
    "schema_version": 1,
    "car_id": 1,
    "resource": "charge_samples",
    "high_watermark": 31,
    "items": [
      {
        "sample_id": 31,
        "charge_id": 7,
        "car_id": 1,
        "recorded_at": "2026-07-11T22:04:00Z",
        "battery_level_percent": 45,
        "usable_battery_level_percent": 43,
        "charge_energy_added_kwh": 12.34,
        "not_enough_power_to_heat": false,
        "charger_actual_current_a": 16,
        "charger_phases": 3,
        "charger_pilot_current_a": 16,
        "charger_power_kw": 11,
        "charger_voltage_v": 230,
        "ideal_battery_range_km": 201.4,
        "rated_battery_range_km": 188.8,
        "battery_heater": false,
        "battery_heater_on": false,
        "battery_heater_no_power": false,
        "charge_cable": "IEC",
        "fast_charger_present": false,
        "fast_charger_brand": null,
        "fast_charger_type": null,
        "outside_temp_c": 13.5
      }
    ],
    "page": {
      "count": 1,
      "has_more": false,
      "next_cursor": null
    }
  }
}
```

Continue with `next_cursor` until `has_more` is `false`. The final `next_cursor` is `null`.

`limit` defaults to 5,000. Values above 10,000 are capped at 10,000. Invalid and non-positive values return `400 invalid_limit`.

## Data contract

Sample rows are ordered by `sample_id`. Drive rows include `drive_id`. Charge rows include `charge_id`, which is the TeslaMate `charging_process_id` used by the existing charge endpoints.

The export representation does not apply user display settings. Timestamps use UTC RFC 3339. Distances and ranges use kilometres. Temperatures use Celsius. SQL `NULL` is JSON `null`.

### Drive sample fields

| Field | JSON type | Nullable | Unit or format | TeslaMate source |
| --- | --- | :---: | --- | --- |
| `sample_id` | integer | no | row ID | `positions.id` |
| `drive_id` | integer | no | parent ID | `positions.drive_id` |
| `car_id` | integer | no | car ID | `positions.car_id` |
| `recorded_at` | string | no | UTC RFC 3339 | `positions.date` |
| `latitude` | number | no | decimal degrees | `positions.latitude` |
| `longitude` | number | no | decimal degrees | `positions.longitude` |
| `speed_kph` | integer | yes | kilometres per hour | `positions.speed` |
| `power_kw` | integer | no | signed instantaneous kilowatts | `positions.power` |
| `odometer_km` | number | yes | kilometres | `positions.odometer` |
| `battery_level_percent` | integer | yes | percent | `positions.battery_level` |
| `usable_battery_level_percent` | integer | yes | percent | `positions.usable_battery_level` |
| `elevation_m` | integer | yes | metres | `positions.elevation` |
| `inside_temp_c` | number | yes | Celsius | `positions.inside_temp` |
| `outside_temp_c` | number | yes | Celsius | `positions.outside_temp` |
| `is_climate_on` | boolean | yes | state | `positions.is_climate_on` |
| `fan_status` | integer | yes | Tesla fan setting | `positions.fan_status` |
| `driver_temp_setting_c` | number | yes | Celsius | `positions.driver_temp_setting` |
| `passenger_temp_setting_c` | number | yes | Celsius | `positions.passenger_temp_setting` |
| `is_rear_defroster_on` | boolean | yes | state | `positions.is_rear_defroster_on` |
| `is_front_defroster_on` | boolean | yes | state | `positions.is_front_defroster_on` |
| `est_battery_range_km` | number | yes | kilometres | `positions.est_battery_range_km` |
| `ideal_battery_range_km` | number | yes | kilometres | `positions.ideal_battery_range_km` |
| `rated_battery_range_km` | number | yes | kilometres | `positions.rated_battery_range_km` |
| `battery_heater` | boolean | yes | state | `positions.battery_heater` |
| `battery_heater_on` | boolean | yes | state | `positions.battery_heater_on` |
| `battery_heater_no_power` | boolean | yes | state | `positions.battery_heater_no_power` |

### Charge sample fields

| Field | JSON type | Nullable | Unit or format | TeslaMate source |
| --- | --- | :---: | --- | --- |
| `sample_id` | integer | no | row ID | `charges.id` |
| `charge_id` | integer | no | parent ID | `charges.charging_process_id` |
| `car_id` | integer | no | car ID | `charging_processes.car_id` |
| `recorded_at` | string | no | UTC RFC 3339 | `charges.date` |
| `battery_level_percent` | integer | yes | percent | `charges.battery_level` |
| `usable_battery_level_percent` | integer | yes | percent | `charges.usable_battery_level` |
| `charge_energy_added_kwh` | number | no | cumulative session kilowatt-hours at this sample | `charges.charge_energy_added` |
| `not_enough_power_to_heat` | boolean | yes | state | `charges.not_enough_power_to_heat` |
| `charger_actual_current_a` | integer | yes | amps | `charges.charger_actual_current` |
| `charger_phases` | integer | yes | phase count | `charges.charger_phases` |
| `charger_pilot_current_a` | integer | yes | amps | `charges.charger_pilot_current` |
| `charger_power_kw` | integer | no | kilowatts | `charges.charger_power` |
| `charger_voltage_v` | integer | yes | volts | `charges.charger_voltage` |
| `ideal_battery_range_km` | number | no | kilometres | `charges.ideal_battery_range_km` |
| `rated_battery_range_km` | number | yes | kilometres | `charges.rated_battery_range_km` |
| `battery_heater` | boolean | yes | state | `charges.battery_heater` |
| `battery_heater_on` | boolean | yes | state | `charges.battery_heater_on` |
| `battery_heater_no_power` | boolean | yes | state | `charges.battery_heater_no_power` |
| `charge_cable` | string | yes | Tesla cable value | `charges.conn_charge_cable` |
| `fast_charger_present` | boolean | yes | state | `charges.fast_charger_present` |
| `fast_charger_brand` | string | yes | Tesla brand value | `charges.fast_charger_brand` |
| `fast_charger_type` | string | yes | Tesla charger value | `charges.fast_charger_type` |
| `outside_temp_c` | number | yes | Celsius | `charges.outside_temp` |

Only completed parent records are included. Drive samples also follow the existing drive list eligibility rules: the drive needs start and end odometer values, and the sample needs a non-null power value.

## Boundary guarantees

The manifest captures a completion cutoff plus sample and parent high watermarks in one read-only repeatable-read transaction. The displayed `high_watermark` is the sample-ID bound. The parent-ID bound is carried only inside the opaque cursor. Page queries never return IDs above either bound. A parent completed after `completed_before` cannot join the export later.

The boundary covers only the two sample streams. Parent drive and charge summaries fetched from existing endpoints are outside this boundary and are not transactionally aligned with the sample export. Consumers that also export parents should reconcile them by ID.

This is a bounded export, not a persisted MVCC snapshot. TeslaMate sample tables are normally append-only, which makes the boundary useful for resumable exports. Updates, deletes, or delayed transactions affecting IDs inside the boundary can still change the final rows. `row_count` is advisory, so clients should compare it with the number of rows received.

## Errors

New export routes use HTTP status codes and [RFC 9457 Problem Details](https://www.rfc-editor.org/rfc/rfc9457.html). Existing v1 routes keep their current response behavior.

Error responses use `application/problem+json` and include a stable documentation URL in `type`, plus `code`, `retryable`, and `request_id`. The same request ID is returned in the `X-Request-ID` header. A `401` response also returns `WWW-Authenticate: Bearer`. Database and panic details are not returned to the client.

```json
{
  "type": "https://github.com/MyTeslaMate/teslamateapi/blob/main/docs/export-api.md#problem-invalid-cursor",
  "title": "Invalid cursor",
  "status": 400,
  "detail": "The export cursor is invalid or does not match this resource.",
  "instance": "urn:teslamateapi:request:0123456789abcdef0123456789abcdef",
  "code": "invalid_cursor",
  "retryable": false,
  "request_id": "0123456789abcdef0123456789abcdef"
}
```

`retryable: true` means a later attempt may succeed. The API does not promise a fixed retry delay or return `Retry-After`. Clients should use bounded exponential backoff, retry the same request with the same cursor, and advance only after a successful page.

<a id="problem-unauthorized"></a>
<a id="problem-invalid-car-id"></a>
<a id="problem-invalid-limit"></a>
<a id="problem-invalid-cursor"></a>
<a id="problem-car-not-found"></a>
<a id="problem-request-timeout"></a>
<a id="problem-database-unavailable"></a>
<a id="problem-export-unavailable"></a>
<a id="problem-internal-error"></a>

| Code | Status | Retryable | Meaning |
| --- | ---: | :---: | --- |
| `unauthorized` | 401 | no | The API token is missing or invalid |
| `invalid_car_id` | 400 | no | The car ID is malformed or outside the database range |
| `invalid_limit` | 400 | no | The page limit is malformed or non-positive |
| `invalid_cursor` | 400 | no | The cursor is missing, malformed, modified, or belongs to another car or resource |
| `car_not_found` | 404 | no | No car exists with the requested ID |
| `request_timeout` | 503 | yes | The query or its wait for an export slot exceeded the configured deadline |
| `database_unavailable` | 503 | yes | PostgreSQL returned a classified transient failure |
| `export_unavailable` | 503 | no | No stable cursor signing key is configured |
| `internal_error` | 500 | no | An unexpected export failure occurred |
