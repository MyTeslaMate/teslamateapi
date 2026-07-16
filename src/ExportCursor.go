package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const exportCursorVersion = 1

type exportResource string

const (
	exportResourceDriveSamples  exportResource = "drive_samples"
	exportResourceChargeSamples exportResource = "charge_samples"
)

var (
	errExportCursorInvalid     = errors.New("invalid export cursor")
	errExportCursorUnavailable = errors.New("export cursor secret is not configured")
)

type exportCursor struct {
	Version             int            `json:"v"`
	CarID               int            `json:"car_id"`
	Resource            exportResource `json:"resource"`
	AfterID             int64          `json:"after_id"`
	HighWatermark       int64          `json:"high_watermark"`
	ParentHighWatermark int64          `json:"parent_high_watermark"`
	CompletedBeforeUS   int64          `json:"completed_before_us"`
}

type exportCursorCodec struct {
	key []byte
}

func newExportCursorCodec(key []byte) exportCursorCodec {
	return exportCursorCodec{key: append([]byte(nil), key...)}
}

func exportCursorKeyFromEnv() []byte {
	secret := getEnv("EXPORT_CURSOR_SECRET", "")
	if secret == "" {
		secret = getEnv("ENCRYPTION_KEY", "")
	}
	if secret == "" {
		return nil
	}

	return exportCursorKey([]byte(secret))
}

func exportCursorKey(secret []byte) []byte {
	seed := append([]byte("teslamateapi/export-cursor/v1\x00"), secret...)
	sum := sha256.Sum256(seed)
	return sum[:]
}

func (codec exportCursorCodec) Encode(cursor exportCursor) (string, error) {
	if len(codec.key) == 0 {
		return "", errExportCursorUnavailable
	}
	if !validExportCursor(cursor) {
		return "", errExportCursorInvalid
	}

	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	payloadPart := base64.RawURLEncoding.EncodeToString(payload)
	signaturePart := base64.RawURLEncoding.EncodeToString(codec.sign([]byte(payloadPart)))
	return payloadPart + "." + signaturePart, nil
}

func (codec exportCursorCodec) Decode(
	token string,
	expectedResource exportResource,
	expectedCarID int,
) (exportCursor, error) {
	if len(codec.key) == 0 {
		return exportCursor{}, errExportCursorUnavailable
	}
	if token == "" || len(token) > 1024 || strings.Count(token, ".") != 1 {
		return exportCursor{}, errExportCursorInvalid
	}

	parts := strings.SplitN(token, ".", 2)
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(signature, codec.sign([]byte(parts[0]))) {
		return exportCursor{}, errExportCursorInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return exportCursor{}, errExportCursorInvalid
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var cursor exportCursor
	if err := decoder.Decode(&cursor); err != nil {
		return exportCursor{}, errExportCursorInvalid
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return exportCursor{}, errExportCursorInvalid
	}
	if !validExportCursor(cursor) || cursor.Resource != expectedResource || cursor.CarID != expectedCarID {
		return exportCursor{}, errExportCursorInvalid
	}

	return cursor, nil
}

func (codec exportCursorCodec) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, codec.key)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func validExportCursor(cursor exportCursor) bool {
	if cursor.Version != exportCursorVersion || cursor.CarID <= 0 {
		return false
	}
	if cursor.Resource != exportResourceDriveSamples && cursor.Resource != exportResourceChargeSamples {
		return false
	}
	if cursor.AfterID < 0 || cursor.HighWatermark < 0 || cursor.ParentHighWatermark < 0 {
		return false
	}
	if cursor.AfterID > cursor.HighWatermark || cursor.CompletedBeforeUS <= 0 {
		return false
	}
	return true
}
