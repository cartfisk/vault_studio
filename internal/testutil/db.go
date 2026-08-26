// Package testutil provides shared test fixtures. Not imported by production code.
package testutil

import (
	"path/filepath"
	"runtime"
	"testing"

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
