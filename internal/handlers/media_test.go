package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// A user whose quality preference is "source" must still get gapless.
//
// The client's quality control is a two-way toggle between "source" and
// "lossy" -- "lossless" is accepted by the API but unreachable from the UI --
// so gating the manifest on "lossless" alone made the feature unreachable for
// every user. "source" implies lossless here by construction: segment sets are
// only ever built when the source codec is lossless, so a completed set
// existing is itself proof the source was lossless.
func TestStreamURLOffersGaplessForSourceQuality(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)
	trackPublicID, versionID := testutil.SeedTrackForUser(t, database, owner)

	path := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(path, []byte("alac-fragment-bytes"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	testutil.SeedCompletedSegmentSet(t, database, versionID, "alac", path)
	testutil.SetUserQuality(t, database, owner, "source")

	h := handlers.NewMediaHandler(testAuthConfig(), database)

	req := httptest.NewRequest(http.MethodGet, "/api/media/stream/"+trackPublicID+"?codecs=alac,flac", nil)
	req.SetPathValue("id", trackPublicID)
	req = withUser(req, owner)

	rec := httptest.NewRecorder()
	if err := h.StreamURL(rec, req); err != nil {
		t.Fatalf("StreamURL() error = %v", err)
	}

	payload := decodeResultObject(t, rec)
	gapless, ok := payload["gapless"].(map[string]any)
	if !ok {
		t.Fatalf("response = %v, want a gapless manifest for source quality", payload)
	}
	if gapless["codec"] != "alac" {
		t.Errorf("codec = %v, want alac", gapless["codec"])
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

// TestStreamURLOmitsGaplessForCrossUserTrack proves that gaplessManifest
// gates on the calling user's access to the track, not merely on quality.
// User A calls StreamURL naming user B's track (public id from the URL
// path). User B's track has a completed lossless set, and BOTH users have
// their own quality preference set to "lossless" -- if user A's own
// preference were left at the "lossy" default, this test would pass
// because of the quality gate, not the access gate, and would keep passing
// even with the access check deleted.
func TestStreamURLOmitsGaplessForCrossUserTrack(t *testing.T) {
	database := testutil.NewDB(t)
	userA, userB := int64(1), int64(2)

	testutil.SeedTrackForUser(t, database, userA)
	trackBPublicID, versionB := testutil.SeedTrackForUser(t, database, userB)

	path := filepath.Join(t.TempDir(), "b.mp4")
	if err := os.WriteFile(path, []byte("user-b-alac-fragment-bytes"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	testutil.SeedCompletedSegmentSet(t, database, versionB, "alac", path)
	testutil.SetUserQuality(t, database, userB, "lossless")
	testutil.SetUserQuality(t, database, userA, "lossless")

	h := handlers.NewMediaHandler(testAuthConfig(), database)

	req := httptest.NewRequest(http.MethodGet,
		"/api/media/stream/"+trackBPublicID+"?codecs=alac,flac", nil)
	req.SetPathValue("id", trackBPublicID)
	req = withUser(req, userA)

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
		t.Fatalf("response keys = %v, want [url] (gapless metadata leaked to a user without access)", keys)
	}
}

// TestStreamURLManifestURLNamesTheVersionItDescribes proves that the
// manifest's "url" field carries the same version_id whose fragment layout
// the manifest describes. gaplessManifest resolves the requested (possibly
// non-active) version and returns THAT version's fragments, but the URL it
// hands back must let StreamGapless serve bytes from the same version --
// otherwise a client applies one version's byte offsets to a different
// version's file.
//
// The two versions are seeded with segment sets of different sizes, so a
// mismatch between the returned fragments and the requested version would
// be visible even if the URL's version_id happened to be right, and vice
// versa.
func TestStreamURLManifestURLNamesTheVersionItDescribes(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)
	trackPublicID, activeVersionID := testutil.SeedTrackForUser(t, database, owner)
	nonActiveVersionID := testutil.AddNonActiveVersion(t, database, trackPublicID)

	activePath := filepath.Join(t.TempDir(), "active.mp4")
	if err := os.WriteFile(activePath, make([]byte, 100), 0o644); err != nil {
		t.Fatalf("write active fixture: %v", err)
	}
	testutil.SeedCompletedSegmentSet(t, database, activeVersionID, "alac", activePath)

	nonActivePath := filepath.Join(t.TempDir(), "non-active.mp4")
	if err := os.WriteFile(nonActivePath, make([]byte, 250), 0o644); err != nil {
		t.Fatalf("write non-active fixture: %v", err)
	}
	testutil.SeedCompletedSegmentSet(t, database, nonActiveVersionID, "alac", nonActivePath)

	testutil.SetUserQuality(t, database, owner, "lossless")

	h := handlers.NewMediaHandler(testAuthConfig(), database)

	req := httptest.NewRequest(http.MethodGet,
		"/api/media/stream/"+trackPublicID+"?codecs=alac&version_id="+strconv.FormatInt(nonActiveVersionID, 10), nil)
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

	// The fragments must be the non-active version's (byte_end 249, from
	// its 250-byte file), not the active version's (byte_end 99).
	fragments, ok := gapless["fragments"].([]any)
	if !ok || len(fragments) != 1 {
		t.Fatalf("fragments = %v, want a single fragment", gapless["fragments"])
	}
	frag, ok := fragments[0].(map[string]any)
	if !ok {
		t.Fatalf("fragment[0] = %v, want an object", fragments[0])
	}
	if end, _ := frag["end"].(float64); int64(end) != 249 {
		t.Fatalf("fragment end = %v, want 249 (the non-active version's fragments)", frag["end"])
	}

	wantURL := "/api/stream/" + trackPublicID + "/gapless/alac?version_id=" + strconv.FormatInt(nonActiveVersionID, 10)
	if gotURL, _ := gapless["url"].(string); gotURL != wantURL {
		t.Fatalf("url = %q, want %q (must name the version the fragments describe)", gotURL, wantURL)
	}
}
