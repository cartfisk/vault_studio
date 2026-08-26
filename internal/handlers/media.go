package handlers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"bungleware/vault/internal/apperr"
	"bungleware/vault/internal/auth"
	"bungleware/vault/internal/db"
	sqlc "bungleware/vault/internal/db/sqlc"
	"bungleware/vault/internal/httputil"
	"bungleware/vault/internal/middleware"
)

type MediaHandler struct {
	config auth.Config
	db     *db.DB
}

func NewMediaHandler(config auth.Config, database *db.DB) *MediaHandler {
	return &MediaHandler{config: config, db: database}
}

type gaplessFragment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type gaplessManifest struct {
	Codec       string            `json:"codec"`
	URL         string            `json:"url"`
	SampleRate  int64             `json:"sampleRate"`
	SampleCount int64             `json:"sampleCount"`
	Channels    int64             `json:"channels"`
	InitByteEnd int64             `json:"initByteEnd"`
	Fragments   []gaplessFragment `json:"fragments"`
}

func (h *MediaHandler) StreamURL(w http.ResponseWriter, r *http.Request) error {
	userID, err := httputil.RequireUserID(r)
	if err != nil {
		return apperr.NewUnauthorized("unauthorized")
	}

	trackID := r.PathValue("id")
	if trackID == "" {
		return apperr.NewBadRequest("track id is required")
	}

	query := url.Values{}
	query.Set("user_id", strconv.Itoa(userID))

	quality := r.URL.Query().Get("quality")
	if quality != "" {
		query.Set("quality", quality)
	}
	versionID := r.URL.Query().Get("version_id")
	if versionID != "" {
		query.Set("version_id", versionID)
	}

	path := "/api/stream/" + trackID
	signedURL, err := middleware.BuildSignedURL("", path, query, h.config.SignedURLSecret, h.config.SignedURLExpiration)
	if err != nil {
		return apperr.NewInternal("failed to build signed url", err)
	}

	response := map[string]any{"url": signedURL}

	if manifest := h.gaplessManifest(r, trackID, quality, versionID, userID); manifest != nil {
		response["gapless"] = manifest
	}

	return httputil.OKResult(w, response)
}

// gaplessManifest returns the gapless playback manifest for trackID, or nil
// if gapless playback isn't applicable to this request. It only returns a
// manifest when the client sent a codecs list, the resolved quality is
// "lossless", and a completed segment set exists for one of the requested
// codecs. The client's codec order decides which codec is chosen among the
// ones the server has available.
func (h *MediaHandler) gaplessManifest(r *http.Request, trackID, requestedQuality, versionIDStr string, userID int) *gaplessManifest {
	codecsParam := r.URL.Query().Get("codecs")
	if codecsParam == "" {
		return nil
	}

	var codecs []string
	for _, c := range strings.Split(codecsParam, ",") {
		c = strings.TrimSpace(c)
		if c != "" {
			codecs = append(codecs, c)
		}
	}
	if len(codecs) == 0 {
		return nil
	}

	ctx := r.Context()

	track, err := h.db.Queries.GetTrackByPublicIDNoFilter(ctx, trackID)
	if err != nil {
		return nil
	}

	var requestedVersionID *int64
	if versionIDStr != "" {
		vid, err := strconv.ParseInt(versionIDStr, 10, 64)
		if err != nil {
			return nil
		}
		requestedVersionID = &vid
	}

	finalVersionID, err := resolveVersionForTrack(ctx, h.db, track, requestedVersionID)
	if err != nil {
		return nil
	}

	quality := resolveQuality(ctx, h.db, int64(userID), track.ID, requestedQuality)
	if quality != "lossless" {
		return nil
	}

	for _, codec := range codecs {
		if codec != "alac" && codec != "flac" {
			continue
		}

		set, err := h.db.GetCompletedSegmentSet(ctx, sqlc.GetCompletedSegmentSetParams{
			VersionID: finalVersionID,
			Codec:     codec,
		})
		if err != nil {
			continue
		}

		fragments, err := h.buildFragments(ctx, set.ID)
		if err != nil {
			continue
		}

		return &gaplessManifest{
			Codec:       codec,
			URL:         "/api/stream/" + trackID + "/gapless/" + codec,
			SampleRate:  set.SampleRate,
			SampleCount: set.SampleCount,
			Channels:    set.Channels,
			InitByteEnd: set.InitByteEnd,
			Fragments:   fragments,
		}
	}

	return nil
}

func (h *MediaHandler) buildFragments(ctx context.Context, setID int64) ([]gaplessFragment, error) {
	rows, err := h.db.ListSegmentFragments(ctx, setID)
	if err != nil {
		return nil, err
	}

	fragments := make([]gaplessFragment, 0, len(rows))
	for _, row := range rows {
		fragments = append(fragments, gaplessFragment{Start: row.ByteStart, End: row.ByteEnd})
	}

	return fragments, nil
}

func (h *MediaHandler) ProjectCoverURL(w http.ResponseWriter, r *http.Request) error {
	userID, err := httputil.RequireUserID(r)
	if err != nil {
		return apperr.NewUnauthorized("unauthorized")
	}

	projectID := r.PathValue("id")
	if projectID == "" {
		return apperr.NewBadRequest("project id is required")
	}

	query := url.Values{}
	query.Set("user_id", strconv.Itoa(userID))

	size := r.URL.Query().Get("size")
	if size != "" {
		query.Set("size", size)
	}

	path := "/api/projects/" + projectID + "/cover"
	coverURL, err := middleware.BuildSignedURL("", path, query, h.config.SignedURLSecret, h.config.SignedURLExpiration)
	if err != nil {
		return apperr.NewInternal("failed to build signed url", err)
	}

	return httputil.OKResult(w, map[string]string{"url": coverURL})
}
