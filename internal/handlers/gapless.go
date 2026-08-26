package handlers

import (
	"net/http"
	"os"
	"strconv"

	"bungleware/vault/internal/apperr"
	sqlc "bungleware/vault/internal/db/sqlc"
	"bungleware/vault/internal/handlers/tracks"
	"bungleware/vault/internal/httputil"
	"bungleware/vault/internal/middleware"
)

// StreamGapless serves a lossless fragmented MP4 by byte range.
//
// Unlike StreamTrack this route is not signed: MSE fetches through the app's
// own fetch(), which carries the session cookie on web and the bearer token in
// the Capacitor build. It therefore runs behind normal AuthMiddleware.
func (h *StreamingHandler) StreamGapless(w http.ResponseWriter, r *http.Request) error {
	userID, ok := middleware.GetUserID(r.Context())
	if !ok {
		return apperr.NewUnauthorized("unauthorized")
	}

	codec := r.PathValue("codec")
	if codec != "alac" && codec != "flac" {
		return apperr.NewBadRequest("unsupported codec")
	}

	publicID := r.PathValue("id")
	ctx := r.Context()

	track, err := h.db.Queries.GetTrackByPublicIDNoFilter(ctx, publicID)
	if err := httputil.HandleDBError(err, "track not found", "failed to query track"); err != nil {
		return err
	}

	// Same check StreamTrack performs. Omitting it would let a revoked share
	// keep playing through the gapless path.
	access, err := tracks.CheckTrackAccess(ctx, h.db, track.ID, track.ProjectID, int64(userID))
	if err != nil {
		return apperr.NewInternal("failed to check track access", err)
	}
	if !access.HasAccess {
		return apperr.NewForbidden("access revoked")
	}

	versionID := track.ActiveVersionID.Int64
	if raw := r.URL.Query().Get("version_id"); raw != "" {
		parsed, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			return apperr.NewBadRequest("invalid version_id")
		}
		versionID = parsed
	}
	if versionID == 0 {
		return apperr.NewBadRequest("track has no active version")
	}

	set, err := h.db.GetCompletedSegmentSet(ctx, sqlc.GetCompletedSegmentSetParams{
		VersionID: versionID,
		Codec:     codec,
	})
	if err := httputil.HandleDBError(err, "segment set not found", "failed to query segment set"); err != nil {
		return err
	}

	f, err := os.Open(set.FilePath)
	if err != nil {
		return apperr.NewInternal("failed to open segment file", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return apperr.NewInternal("failed to stat segment file", err)
	}

	w.Header().Set("Content-Type", "audio/mp4")
	// http.ServeContent handles Range, 206, multipart ranges, and malformed
	// range headers. Nothing custom is needed.
	http.ServeContent(w, r, set.FilePath, stat.ModTime(), f)
	return nil
}
