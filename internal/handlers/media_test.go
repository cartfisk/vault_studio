package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"bungleware/vault/internal/auth"
	"bungleware/vault/internal/handlers"
	"bungleware/vault/internal/middleware"
	"bungleware/vault/internal/testutil"
)

// testAuthConfig returns an auth.Config with a signing secret set, so
// BuildSignedURL produces a real URL instead of "".
func testAuthConfig() auth.Config {
	return auth.Config{
		SignedURLSecret:     "test-secret",
		SignedURLExpiration: time.Minute,
	}
}

// decodeResultObject decodes rec's JSON body into the object StreamURL
// builds. httputil.OKResult writes the payload directly (no envelope), so
// this is a plain json.Unmarshal into a map.
func decodeResultObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response body error = %v, body = %s", err, rec.Body.String())
	}
	return payload
}

// withUser attaches userID to req's context the same way AuthMiddleware
// does.
func withUser(req *http.Request, userID int64) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, int(userID)))
}

func TestStreamURLWithoutCodecsIsUnchanged(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)
	trackPublicID, versionID := testutil.SeedTrackForUser(t, database, owner)

	// A completed set exists, so any leakage would show up here.
	path := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(path, []byte("alac-fragment-bytes"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	testutil.SeedCompletedSegmentSet(t, database, versionID, "alac", path)

	h := handlers.NewMediaHandler(testAuthConfig(), database)

	req := httptest.NewRequest(http.MethodGet, "/api/media/stream/"+trackPublicID, nil)
	req.SetPathValue("id", trackPublicID)
	req = withUser(req, owner)

	rec := httptest.NewRecorder()
	if err := h.StreamURL(rec, req); err != nil {
		t.Fatalf("StreamURL() error = %v", err)
	}

	payload := decodeResultObject(t, rec)

	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(keys) != 1 || keys[0] != "url" {
		t.Fatalf("response keys = %v, want [url]", keys)
	}
}

func TestStreamURLOmitsGaplessWhenNoCompletedSet(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)
	trackPublicID, _ := testutil.SeedTrackForUser(t, database, owner)
	testutil.SetUserQuality(t, database, owner, "lossless")

	h := handlers.NewMediaHandler(testAuthConfig(), database)

	req := httptest.NewRequest(http.MethodGet, "/api/media/stream/"+trackPublicID+"?codecs=alac,flac", nil)
	req.SetPathValue("id", trackPublicID)
	req = withUser(req, owner)

	rec := httptest.NewRecorder()
	if err := h.StreamURL(rec, req); err != nil {
		t.Fatalf("StreamURL() error = %v", err)
	}

	payload := decodeResultObject(t, rec)
	if _, ok := payload["gapless"]; ok {
		t.Fatalf("response = %v, want no gapless key when no completed set exists", payload)
	}
}

func TestStreamURLOmitsGaplessForLossyQuality(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)
	trackPublicID, versionID := testutil.SeedTrackForUser(t, database, owner)

	path := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(path, []byte("alac-fragment-bytes"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	testutil.SeedCompletedSegmentSet(t, database, versionID, "alac", path)
	// No SetUserQuality call: default_quality defaults to "lossy".

	h := handlers.NewMediaHandler(testAuthConfig(), database)

	req := httptest.NewRequest(http.MethodGet, "/api/media/stream/"+trackPublicID+"?codecs=alac,flac", nil)
	req.SetPathValue("id", trackPublicID)
	req = withUser(req, owner)

	rec := httptest.NewRecorder()
	if err := h.StreamURL(rec, req); err != nil {
		t.Fatalf("StreamURL() error = %v", err)
	}

	payload := decodeResultObject(t, rec)
	if _, ok := payload["gapless"]; ok {
		t.Fatalf("response = %v, want no gapless key for lossy quality", payload)
	}
}

func TestStreamURLPrefersClientCodecOrder(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)
	trackPublicID, versionID := testutil.SeedTrackForUser(t, database, owner)

	alacPath := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(alacPath, []byte("alac-fragment-bytes"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	flacPath := filepath.Join(t.TempDir(), "f.mp4")
	if err := os.WriteFile(flacPath, []byte("flac-fragment-bytes"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	testutil.SeedCompletedSegmentSet(t, database, versionID, "alac", alacPath)
	testutil.SeedCompletedSegmentSet(t, database, versionID, "flac", flacPath)
	testutil.SetUserQuality(t, database, owner, "lossless")

	h := handlers.NewMediaHandler(testAuthConfig(), database)

	for _, tc := range []struct {
		codecs string
		want   string
	}{
		{"flac,alac", "flac"},
		{"alac,flac", "alac"},
		{"flac", "flac"},
	} {
		t.Run(tc.codecs, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet,
				"/api/media/stream/"+trackPublicID+"?codecs="+tc.codecs, nil)
			req.SetPathValue("id", trackPublicID)
			req = withUser(req, owner)

			rec := httptest.NewRecorder()
			if err := h.StreamURL(rec, req); err != nil {
				t.Fatalf("StreamURL() error = %v", err)
			}

			payload := decodeResultObject(t, rec)
			gapless, ok := payload["gapless"].(map[string]any)
			if !ok {
				t.Fatalf("gapless missing from response %v", payload)
			}
			if got := gapless["codec"]; got != tc.want {
				t.Errorf("codec = %v, want %q", got, tc.want)
			}
		})
	}
}
