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
	"strconv"
	"testing"

	"bungleware/vault/internal/apperr"
	"bungleware/vault/internal/handlers"
	"bungleware/vault/internal/middleware"
	"bungleware/vault/internal/testutil"
)

// These tests cover a cross-track version_id confusion: CheckTrackAccess is
// run against the track named in the URL path, but the audio actually
// served is selected by a version_id that can be supplied via the query
// string, with nothing verifying that version belongs to that track. A
// user with legitimate access to their OWN track can supply another
// user's version_id and receive that other user's audio.

func TestStreamGaplessRejectsCrossTrackVersionID(t *testing.T) {
	database := testutil.NewDB(t)
	userA, userB := int64(1), int64(2)

	// User A's own track/version, which A has legitimate access to.
	trackAPublicID, _ := testutil.SeedTrackForUser(t, database, userA)

	// User B's track/version, with a completed ALAC segment set holding
	// content that identifies it as B's data.
	_, versionB := testutil.SeedTrackForUser(t, database, userB)
	bMarker := bytes.Repeat([]byte("USER-B-SECRET-AUDIO-BYTES"), 200)
	pathB := filepath.Join(t.TempDir(), "gapless-alac-b.mp4")
	if err := os.WriteFile(pathB, bMarker, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	testutil.SeedCompletedSegmentSet(t, database, versionB, "alac", pathB)

	h := handlers.NewStreamingHandler(database)

	// User A requests through their OWN track's URL, but asks for B's
	// version_id via the query string.
	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+trackAPublicID+"/gapless/alac?version_id="+strconv.FormatInt(versionB, 10), nil)
	req.SetPathValue("id", trackAPublicID)
	req.SetPathValue("codec", "alac")
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, int(userA)))

	rec := httptest.NewRecorder()
	err := h.StreamGapless(rec, req)

	if err == nil {
		res := rec.Result()
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		leaked := bytes.Contains(body, []byte("USER-B-SECRET-AUDIO-BYTES"))
		t.Fatalf("StreamGapless() error = nil for a cross-track version_id, want forbidden; "+
			"response status=%d bodyLen=%d containsUserBBytes=%v", res.StatusCode, len(body), leaked)
	}

	var appErr *apperr.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v, want *apperr.AppError", err)
	}
	if appErr.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", appErr.Status, http.StatusForbidden)
	}
}

func TestStreamTrackRejectsCrossTrackVersionID(t *testing.T) {
	database := testutil.NewDB(t)
	userA, userB := int64(1), int64(2)

	trackAPublicID, _ := testutil.SeedTrackForUser(t, database, userA)

	_, versionB := testutil.SeedTrackForUser(t, database, userB)
	bMarker := bytes.Repeat([]byte("USER-B-SECRET-TRACK-BYTES"), 200)
	pathB := filepath.Join(t.TempDir(), "track-b-lossy.mp3")
	if err := os.WriteFile(pathB, bMarker, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO track_files (version_id, quality, file_path, file_size, format, transcoding_status) VALUES (?, 'lossy', ?, ?, 'mp3', 'completed')`,
		versionB, pathB, len(bMarker),
	); err != nil {
		t.Fatalf("insert user B track file: %v", err)
	}

	h := handlers.NewStreamingHandler(database)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+trackAPublicID+"?version_id="+strconv.FormatInt(versionB, 10), nil)
	req.SetPathValue("id", trackAPublicID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, int(userA)))

	rec := httptest.NewRecorder()
	err := h.StreamTrack(rec, req)

	if err == nil {
		res := rec.Result()
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		leaked := bytes.Contains(body, []byte("USER-B-SECRET-TRACK-BYTES"))
		t.Fatalf("StreamTrack() error = nil for a cross-track version_id, want forbidden; "+
			"response status=%d bodyLen=%d containsUserBBytes=%v", res.StatusCode, len(body), leaked)
	}

	var appErr *apperr.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v, want *apperr.AppError", err)
	}
	if appErr.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", appErr.Status, http.StatusForbidden)
	}
}

