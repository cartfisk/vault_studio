-- name: CreateSegmentSet :one
INSERT INTO track_segment_sets (version_id, codec, file_path, status)
VALUES (?, ?, ?, 'pending')
ON CONFLICT(version_id, codec) DO UPDATE SET
    file_path = excluded.file_path,
    status = 'pending',
    file_size = 0,
    sample_rate = 0,
    sample_count = 0,
    channels = 0,
    init_byte_end = 0
RETURNING *;

-- name: CompleteSegmentSet :exec
UPDATE track_segment_sets
SET file_size = ?, sample_rate = ?, sample_count = ?, channels = ?,
    init_byte_end = ?, status = 'completed'
WHERE id = ?;

-- name: FailSegmentSet :exec
UPDATE track_segment_sets SET status = 'failed' WHERE id = ?;

-- name: MarkSegmentSetProcessing :exec
UPDATE track_segment_sets SET status = 'processing' WHERE id = ?;

-- name: ListSegmentSetsForVersion :many
SELECT id, codec FROM track_segment_sets WHERE version_id = ?;

-- name: DeleteSegmentFragments :exec
DELETE FROM track_segment_fragments WHERE set_id = ?;

-- name: CreateSegmentFragment :exec
INSERT INTO track_segment_fragments (set_id, idx, byte_start, byte_end)
VALUES (?, ?, ?, ?);

-- name: GetCompletedSegmentSet :one
SELECT * FROM track_segment_sets
WHERE version_id = ? AND codec = ? AND status = 'completed';

-- name: ListSegmentFragments :many
SELECT idx, byte_start, byte_end FROM track_segment_fragments
WHERE set_id = ? ORDER BY idx;

-- name: ListLosslessVersionsMissingSegments :many
SELECT tv.id AS version_id, tf.file_path AS source_path
FROM track_versions tv
JOIN track_files tf
    ON tf.version_id = tv.id AND tf.quality = 'source'
WHERE NOT EXISTS (
    SELECT 1 FROM track_segment_sets s
    WHERE s.version_id = tv.id
      AND s.codec = 'alac'
      AND s.status = 'completed'
)
ORDER BY tv.id;
