// Package testutil provides shared test fixtures. Not imported by production code.
package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"bungleware/vault/internal/db"
)

// NewDB opens a migrated SQLite database in a per-test temp directory.
func NewDB(t *testing.T) *db.DB {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve testutil source path")
	}
	migrations := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	database, err := db.New(db.Config{
		DataDir:        t.TempDir(),
		DBFile:         "test.db",
		MigrationsPath: migrations,
	})
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	t.Cleanup(func() { database.Close() })

	return database
}

// SeedVersion inserts the minimum project/track/version chain a
// track_versions foreign key needs, and returns the new version id.
func SeedVersion(t *testing.T, database *db.DB) int64 {
	t.Helper()

	userRes, err := database.Exec(
		`INSERT INTO users (username, email, password_hash) VALUES (?, ?, ?)`,
		"testuser", "testuser@example.com", "hash",
	)
	if err != nil {
		t.Fatalf("insert user error = %v", err)
	}
	userID, err := userRes.LastInsertId()
	if err != nil {
		t.Fatalf("user LastInsertId error = %v", err)
	}

	projectRes, err := database.Exec(
		`INSERT INTO projects (user_id, name, public_id) VALUES (?, ?, ?)`,
		userID, "Test Project", "test-project-public-id",
	)
	if err != nil {
		t.Fatalf("insert project error = %v", err)
	}
	projectID, err := projectRes.LastInsertId()
	if err != nil {
		t.Fatalf("project LastInsertId error = %v", err)
	}

	trackRes, err := database.Exec(
		`INSERT INTO tracks (user_id, project_id, title, public_id) VALUES (?, ?, ?, ?)`,
		userID, projectID, "Test Track", "test-track-public-id",
	)
	if err != nil {
		t.Fatalf("insert track error = %v", err)
	}
	trackID, err := trackRes.LastInsertId()
	if err != nil {
		t.Fatalf("track LastInsertId error = %v", err)
	}

	versionRes, err := database.Exec(
		`INSERT INTO track_versions (track_id, version_name) VALUES (?, ?)`,
		trackID, "v1",
	)
	if err != nil {
		t.Fatalf("insert version error = %v", err)
	}
	versionID, err := versionRes.LastInsertId()
	if err != nil {
		t.Fatalf("version LastInsertId error = %v", err)
	}

	return versionID
}

// SeedTrackForUser inserts a project, a track, and a version owned by
// userID, along with a "source" track_files row for the version. It
// returns the track's public ID and the new version's id.
func SeedTrackForUser(t *testing.T, database *db.DB, userID int64) (publicID string, versionID int64) {
	t.Helper()

	suffix := fmt.Sprintf("%d-%d", userID, time.Now().UnixNano())

	// Insert the user row with the exact id the caller wants ownership
	// attributed to, so foreign keys and CheckTrackAccess's userID
	// comparisons line up. OR IGNORE makes this safe if a test seeds the
	// same userID more than once against one database.
	if _, err := database.Exec(
		`INSERT OR IGNORE INTO users (id, username, email, password_hash) VALUES (?, ?, ?, ?)`,
		userID, "testuser-"+suffix, "testuser-"+suffix+"@example.com", "hash",
	); err != nil {
		t.Fatalf("insert user error = %v", err)
	}

	projectRes, err := database.Exec(
		`INSERT INTO projects (user_id, name, public_id) VALUES (?, ?, ?)`,
		userID, "Test Project "+suffix, "test-project-"+suffix,
	)
	if err != nil {
		t.Fatalf("insert project error = %v", err)
	}
	projectID, err := projectRes.LastInsertId()
	if err != nil {
		t.Fatalf("project LastInsertId error = %v", err)
	}

	trackPublicID := "test-track-" + suffix

	trackRes, err := database.Exec(
		`INSERT INTO tracks (user_id, project_id, title, public_id) VALUES (?, ?, ?, ?)`,
		userID, projectID, "Test Track "+suffix, trackPublicID,
	)
	if err != nil {
		t.Fatalf("insert track error = %v", err)
	}
	trackID, err := trackRes.LastInsertId()
	if err != nil {
		t.Fatalf("track LastInsertId error = %v", err)
	}

	versionRes, err := database.Exec(
		`INSERT INTO track_versions (track_id, version_name) VALUES (?, ?)`,
		trackID, "v1",
	)
	if err != nil {
		t.Fatalf("insert version error = %v", err)
	}
	newVersionID, err := versionRes.LastInsertId()
	if err != nil {
		t.Fatalf("version LastInsertId error = %v", err)
	}

	if _, err := database.Exec(
		`UPDATE tracks SET active_version_id = ? WHERE id = ?`,
		newVersionID, trackID,
	); err != nil {
		t.Fatalf("update active_version_id error = %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO track_files (version_id, quality, file_path, file_size, format) VALUES (?, 'source', ?, 0, 'wav')`,
		newVersionID, "/tmp/testutil-source-"+suffix+".wav",
	); err != nil {
		t.Fatalf("insert source track file error = %v", err)
	}

	return trackPublicID, newVersionID
}

// SeedCompletedSegmentSet inserts a completed track_segment_sets row for
// versionID/codec, backed by the file at path, with a single fragment
// covering the entire file.
func SeedCompletedSegmentSet(t *testing.T, database *db.DB, versionID int64, codec string, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat segment file error = %v", err)
	}
	size := info.Size()

	setRes, err := database.Exec(
		`INSERT INTO track_segment_sets
			(version_id, codec, file_path, file_size, sample_rate, sample_count, channels, init_byte_end, status)
		VALUES (?, ?, ?, ?, 44100, 0, 2, 0, 'completed')`,
		versionID, codec, path, size,
	)
	if err != nil {
		t.Fatalf("insert segment set error = %v", err)
	}
	setID, err := setRes.LastInsertId()
	if err != nil {
		t.Fatalf("segment set LastInsertId error = %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO track_segment_fragments (set_id, idx, byte_start, byte_end) VALUES (?, 0, 0, ?)`,
		setID, size,
	); err != nil {
		t.Fatalf("insert segment fragment error = %v", err)
	}

	return setID
}
