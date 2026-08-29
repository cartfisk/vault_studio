package handlers_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"bungleware/vault/internal/handlers"
	"bungleware/vault/internal/middleware"
	"bungleware/vault/internal/storage"
	"bungleware/vault/internal/testutil"
	"bungleware/vault/internal/transcoding"
)

// makeLosslessWAV renders a tiny silent PCM WAV file with ffmpeg, so
// ExtractMetadata sees a real pcm_s16le stream rather than a stub.
func makeLosslessWAV(t *testing.T, path string) {
	t.Helper()

	cmd := exec.Command("ffmpeg",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono",
		"-t", "0.05",
		"-c:a", "pcm_s16le",
		"-y", path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg fixture generation failed: %v: %s", err, out)
	}
}

// newMultipartUpload builds a multipart/form-data request body containing a
// "file" part with the contents of path.
func newMultipartUpload(t *testing.T, url, path string) *http.Request {
	t.Helper()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write fixture into form: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

// TestUploadVersionCreatesPendingSegmentSetsForLosslessSource proves that a
// second version uploaded onto an existing track (the versions handler's
// create path, distinct from the initial-upload path in
// internal/handlers/tracks/upload.go) still wires SourceCodec through to
// the transcoder, so a lossless version gets its gapless segment sets
// queued. Before the fix, SourceCodec was left as the zero value here,
// IsLosslessCodec("") was false, and no segment sets were ever created for
// any version after a track's first.
func TestUploadVersionCreatesPendingSegmentSetsForLosslessSource(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)
	trackPublicID, _ := testutil.SeedTrackForUser(t, database, owner)

	wavPath := filepath.Join(t.TempDir(), "new-version.wav")
	makeLosslessWAV(t, wavPath)

	store := storage.NewFilesystemStorage(t.TempDir())
	tr := transcoding.NewTranscoder(database, 0) // no workers; inspect the rows only

	h := handlers.NewVersionsHandler(database, store, tr)

	req := newMultipartUpload(t, "/api/tracks/"+trackPublicID+"/versions", wavPath)
	req.SetPathValue("track_id", trackPublicID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserIDKey, int(owner)))

	rec := httptest.NewRecorder()
	if err := h.UploadVersion(rec, req); err != nil {
		t.Fatalf("UploadVersion() error = %v", err)
	}

	rows, err := database.Query(
		`SELECT s.codec, s.status FROM track_segment_sets s
		 JOIN track_versions tv ON tv.id = s.version_id
		 JOIN tracks t ON t.id = tv.track_id
		 WHERE t.public_id = ?
		 ORDER BY s.codec`,
		trackPublicID,
	)
	if err != nil {
		t.Fatalf("query segment sets error = %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var codec, status string
		if err := rows.Scan(&codec, &status); err != nil {
			t.Fatalf("scan error = %v", err)
		}
		got = append(got, codec+":"+status)
	}

	want := []string{"alac:pending", "flac:pending"}
	if len(got) != len(want) {
		t.Fatalf("segment sets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("segment sets[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
