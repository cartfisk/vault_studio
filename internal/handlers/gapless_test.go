package handlers_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"bungleware/vault/internal/apperr"
	"bungleware/vault/internal/handlers"
	"bungleware/vault/internal/middleware"
	"bungleware/vault/internal/testutil"
)

func TestStreamGaplessRejectsRevokedAccess(t *testing.T) {
	database := testutil.NewDB(t)
	owner, stranger := int64(1), int64(2)

	// Seed a track owned by `owner`, with a completed ALAC set on disk.
	trackPublicID, versionID := testutil.SeedTrackForUser(t, database, owner)
	path := filepath.Join(t.TempDir(), "gapless-alac.mp4")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	testutil.SeedCompletedSegmentSet(t, database, versionID, "alac", path)

	h := handlers.NewStreamingHandler(database)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+trackPublicID+"/gapless/alac", nil)
	req.SetPathValue("id", trackPublicID)
	req.SetPathValue("codec", "alac")
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, int(stranger)))

	err := h.StreamGapless(httptest.NewRecorder(), req)
	if err == nil {
		t.Fatal("StreamGapless() error = nil for a user without access, want forbidden")
	}
	var appErr *apperr.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v, want *apperr.AppError", err)
	}
	if appErr.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", appErr.Status, http.StatusForbidden)
	}
	if !errors.Is(err, apperr.ErrForbidden) {
		t.Fatalf("error = %v, want errors.Is match against apperr.ErrForbidden", err)
	}
}

func TestStreamGaplessRejectsUnknownCodec(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)

	trackPublicID, _ := testutil.SeedTrackForUser(t, database, owner)

	h := handlers.NewStreamingHandler(database)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+trackPublicID+"/gapless/mp3", nil)
	req.SetPathValue("id", trackPublicID)
	req.SetPathValue("codec", "mp3")
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, int(owner)))

	err := h.StreamGapless(httptest.NewRecorder(), req)
	if err == nil {
		t.Fatal("StreamGapless() error = nil for an unknown codec, want bad request")
	}
	var appErr *apperr.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v, want *apperr.AppError", err)
	}
	if appErr.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", appErr.Status, http.StatusBadRequest)
	}
}

func TestStreamGaplessMissingSetReturns404(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)

	// Track exists, but no segment set has been created for it.
	trackPublicID, _ := testutil.SeedTrackForUser(t, database, owner)

	h := handlers.NewStreamingHandler(database)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+trackPublicID+"/gapless/alac", nil)
	req.SetPathValue("id", trackPublicID)
	req.SetPathValue("codec", "alac")
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, int(owner)))

	err := h.StreamGapless(httptest.NewRecorder(), req)
	if err == nil {
		t.Fatal("StreamGapless() error = nil for a missing segment set, want not found")
	}
	var appErr *apperr.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v, want *apperr.AppError", err)
	}
	if appErr.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", appErr.Status, http.StatusNotFound)
	}
}

func TestStreamGaplessServesRangeRequest(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)

	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 251)
	}
	path := filepath.Join(t.TempDir(), "gapless-alac.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	trackPublicID, versionID := testutil.SeedTrackForUser(t, database, owner)
	testutil.SeedCompletedSegmentSet(t, database, versionID, "alac", path)

	h := handlers.NewStreamingHandler(database)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+trackPublicID+"/gapless/alac", nil)
	req.SetPathValue("id", trackPublicID)
	req.SetPathValue("codec", "alac")
	req.Header.Set("Range", "bytes=1024-2047")
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, int(owner)))

	rec := httptest.NewRecorder()
	if err := h.StreamGapless(rec, req); err != nil {
		t.Fatalf("StreamGapless() error = %v", err)
	}

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusPartialContent)
	}
	body, _ := io.ReadAll(res.Body)
	if len(body) != 1024 {
		t.Fatalf("body length = %d, want 1024", len(body))
	}
	if !bytes.Equal(body, data[1024:2048]) {
		t.Error("body does not match the requested byte range")
	}
	if ct := res.Header.Get("Content-Type"); ct != "audio/mp4" {
		t.Errorf("Content-Type = %q, want \"audio/mp4\"", ct)
	}
}
