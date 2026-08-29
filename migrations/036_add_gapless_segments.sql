CREATE TABLE track_segment_sets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id INTEGER NOT NULL REFERENCES track_versions(id) ON DELETE CASCADE,
    codec TEXT NOT NULL CHECK (codec IN ('alac', 'flac')),
    file_path TEXT NOT NULL,
    file_size INTEGER NOT NULL DEFAULT 0,
    sample_rate INTEGER NOT NULL DEFAULT 0,
    sample_count INTEGER NOT NULL DEFAULT 0,
    channels INTEGER NOT NULL DEFAULT 0,
    init_byte_end INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (version_id, codec)
);

CREATE TABLE track_segment_fragments (
    set_id INTEGER NOT NULL REFERENCES track_segment_sets(id) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    byte_start INTEGER NOT NULL,
    byte_end INTEGER NOT NULL,
    PRIMARY KEY (set_id, idx),
    CHECK (byte_end >= byte_start)
);

CREATE INDEX idx_track_segment_sets_version
ON track_segment_sets(version_id, status);
