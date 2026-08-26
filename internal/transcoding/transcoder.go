package transcoding

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"bungleware/vault/internal/db"
	sqlc "bungleware/vault/internal/db/sqlc"
)

const (
	JobKindLossy    = "lossy"
	JobKindSegments = "segments"
)

type Job struct {
	Kind          string
	TrackFileID   int64
	VersionID     int64
	TrackPublicID string
	UserID        int64
	SourcePath    string
	OutputPath    string
	SourceCodec   string
}

type TranscodingNotifier interface {
	NotifyTranscodingUpdate(userID int64, trackPublicID string, versionID int64, status string)
}

type Transcoder struct {
	db       *db.DB
	queue    chan Job
	workers  int
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
	notifier TranscodingNotifier
}

func NewTranscoder(database *db.DB, workers int) *Transcoder {
	ctx, cancel := context.WithCancel(context.Background())
	return &Transcoder{
		db:      database,
		queue:   make(chan Job, 100),
		workers: workers,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (t *Transcoder) SetNotifier(n TranscodingNotifier) {
	t.notifier = n
}

func (t *Transcoder) Start() {
	log.Printf("Starting %d transcoding workers", t.workers)
	for i := 0; i < t.workers; i++ {
		t.wg.Add(1)
		go t.worker(i)
	}
}

func (t *Transcoder) Stop() {
	log.Println("Stopping transcoding workers...")
	t.cancel()
	close(t.queue)
	t.wg.Wait()
	log.Println("All transcoding workers stopped")
}

func (t *Transcoder) QueueJob(job Job) {
	select {
	case t.queue <- job:
		log.Printf("Queued transcoding job for version %d", job.VersionID)
	case <-t.ctx.Done():
		log.Println("Cannot queue job: transcoder is shutting down")
	}
}

func (t *Transcoder) worker(id int) {
	defer t.wg.Done()
	log.Printf("Worker %d started", id)

	for {
		select {
		case job, ok := <-t.queue:
			if !ok {
				log.Printf("Worker %d: queue closed, exiting", id)
				return
			}
			log.Printf("Worker %d: processing job for version %d", id, job.VersionID)
			t.processJob(job)
		case <-t.ctx.Done():
			log.Printf("Worker %d: context cancelled, exiting", id)
			return
		}
	}
}

func (t *Transcoder) processJob(job Job) {
	switch job.Kind {
	case JobKindSegments:
		t.processSegmentsJob(job)
	default:
		t.processLossyJob(job)
	}
}

func (t *Transcoder) processLossyJob(job Job) {
	ctx := context.Background()

	err := t.db.UpdateTranscodingStatus(ctx, sqlc.UpdateTranscodingStatusParams{
		TranscodingStatus: sql.NullString{String: "processing", Valid: true},
		ID:                job.TrackFileID,
	})
	if err != nil {
		log.Printf("Failed to update transcoding status to processing: %v", err)
		return
	}

	t.notify(job, "processing")

	err = t.transcodeToMP3(job.SourcePath, job.OutputPath)
	if err != nil {
		log.Printf("Transcoding failed for version %d: %v", job.VersionID, err)
		t.db.UpdateTranscodingStatus(ctx, sqlc.UpdateTranscodingStatusParams{
			TranscodingStatus: sql.NullString{String: "failed", Valid: true},
			ID:                job.TrackFileID,
		})
		t.notify(job, "failed")
		return
	}

	if stat, err := os.Stat(job.OutputPath); err == nil {
		if err := t.db.UpdateTrackFileSize(ctx, sqlc.UpdateTrackFileSizeParams{
			FileSize: stat.Size(),
			ID:       job.TrackFileID,
		}); err != nil {
			log.Printf("Failed to update file size for version %d: %v", job.VersionID, err)
		}
	}

	log.Printf("Generating waveform for version %d", job.VersionID)
	waveformJSON, err := GenerateWaveformJSON(job.SourcePath, 200)
	if err != nil {
		log.Printf("Failed to generate waveform for version %d: %v", job.VersionID, err)
		waveformJSON = ""
	} else {
		err = t.db.UpdateWaveform(ctx, sqlc.UpdateWaveformParams{
			Waveform: sql.NullString{String: waveformJSON, Valid: true},
			ID:       job.TrackFileID,
		})
		if err != nil {
			log.Printf("Failed to save waveform to database: %v", err)
		} else {
			log.Printf("Successfully saved waveform for version %d", job.VersionID)
		}
	}

	err = t.db.UpdateTranscodingStatus(ctx, sqlc.UpdateTranscodingStatusParams{
		TranscodingStatus: sql.NullString{String: "completed", Valid: true},
		ID:                job.TrackFileID,
	})
	if err != nil {
		log.Printf("Failed to update transcoding status to completed: %v", err)
		return
	}

	t.notify(job, "completed")

	log.Printf("Successfully transcoded version %d to MP3", job.VersionID)
}

func (t *Transcoder) notify(job Job, status string) {
	if t.notifier != nil {
		t.notifier.NotifyTranscodingUpdate(job.UserID, job.TrackPublicID, job.VersionID, status)
	}
}

// processSegmentsJob builds both lossless sets and records their layout.
// It never notifies over the websocket: the client discovers gapless
// availability when it requests a stream URL, which keeps the existing
// notification contract untouched.
func (t *Transcoder) processSegmentsJob(job Job) {
	ctx := context.Background()

	setIDs, err := t.segmentSetIDs(ctx, job.VersionID)
	if err != nil {
		log.Printf("Segment sets missing for version %d: %v", job.VersionID, err)
		return
	}

	for _, id := range setIDs {
		if err := t.db.MarkSegmentSetProcessing(ctx, id); err != nil {
			log.Printf("Failed to mark segment set %d processing: %v", id, err)
		}
	}

	versionDir := filepath.Dir(job.SourcePath)
	sets, err := BuildAllSegmentSets(job.SourcePath, versionDir, job.SourceCodec)
	if err != nil {
		log.Printf("Segment generation failed for version %d: %v", job.VersionID, err)
		for _, id := range setIDs {
			if ferr := t.db.FailSegmentSet(ctx, id); ferr != nil {
				log.Printf("Failed to mark segment set %d failed: %v", id, ferr)
			}
		}
		return
	}

	for _, set := range sets {
		id, ok := setIDs[set.Codec]
		if !ok {
			log.Printf("No row for codec %s on version %d", set.Codec, job.VersionID)
			continue
		}
		if err := t.persistSegmentSet(ctx, id, set); err != nil {
			log.Printf("Failed to persist %s set for version %d: %v", set.Codec, job.VersionID, err)
			if ferr := t.db.FailSegmentSet(ctx, id); ferr != nil {
				log.Printf("Failed to mark segment set %d failed: %v", id, ferr)
			}
		}
	}

	log.Printf("Generated lossless segment sets for version %d", job.VersionID)
}

func (t *Transcoder) persistSegmentSet(ctx context.Context, setID int64, set *SegmentSet) error {
	if err := t.db.DeleteSegmentFragments(ctx, setID); err != nil {
		return fmt.Errorf("clear fragments: %w", err)
	}

	for i, frag := range set.Layout.Fragments {
		err := t.db.CreateSegmentFragment(ctx, sqlc.CreateSegmentFragmentParams{
			SetID:     setID,
			Idx:       int64(i),
			ByteStart: frag.Start,
			ByteEnd:   frag.End,
		})
		if err != nil {
			return fmt.Errorf("insert fragment %d: %w", i, err)
		}
	}

	return t.db.CompleteSegmentSet(ctx, sqlc.CompleteSegmentSetParams{
		FileSize:    set.FileSize,
		SampleRate:  int64(set.SampleRate),
		SampleCount: set.SampleCount,
		Channels:    int64(set.Channels),
		InitByteEnd: set.Layout.InitByteEnd,
		ID:          setID,
	})
}

// segmentSetIDs maps codec to the row id created by TranscodeVersion.
func (t *Transcoder) segmentSetIDs(ctx context.Context, versionID int64) (map[string]int64, error) {
	rows, err := t.db.ListSegmentSetsForVersion(ctx, versionID)
	if err != nil {
		return nil, fmt.Errorf("list segment sets: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no segment set rows for version %d", versionID)
	}

	ids := make(map[string]int64, len(rows))
	for _, row := range rows {
		ids[row.Codec] = row.ID
	}
	return ids, nil
}

func (t *Transcoder) transcodeToMP3(inputPath, outputPath string) error {
	outputDir := filepath.Dir(outputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	cmd := exec.Command(
		"ffmpeg",
		"-i", inputPath,
		"-vn",
		"-ar", "44100",
		"-ac", "2",
		"-b:a", "320k",
		"-y",
		outputPath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg failed: %w, output: %s", err, string(output))
	}

	log.Printf("Transcoded %s to %s", filepath.Base(inputPath), filepath.Base(outputPath))
	return nil
}

type TranscodeVersionInput struct {
	VersionID      int64
	SourceFilePath string
	TrackPublicID  string
	UserID         int64
	SourceCodec    string
}

func (t *Transcoder) TranscodeVersion(ctx context.Context, input TranscodeVersionInput) error {
	sourceDir := filepath.Dir(input.SourceFilePath)
	lossyPath := filepath.Join(sourceDir, "lossy.mp3")

	trackFile, err := t.db.CreateTrackFile(ctx, sqlc.CreateTrackFileParams{
		VersionID:         input.VersionID,
		Quality:           "lossy",
		FilePath:          lossyPath,
		FileSize:          0,
		Format:            "mp3",
		Bitrate:           sql.NullInt64{Int64: 320000, Valid: true},
		ContentHash:       sql.NullString{},
		TranscodingStatus: sql.NullString{String: "pending", Valid: true},
		OriginalFilename:  sql.NullString{},
	})
	if err != nil {
		return fmt.Errorf("failed to create track file record: %w", err)
	}

	t.QueueJob(Job{
		Kind:          JobKindLossy,
		TrackFileID:   trackFile.ID,
		VersionID:     input.VersionID,
		TrackPublicID: input.TrackPublicID,
		UserID:        input.UserID,
		SourcePath:    input.SourceFilePath,
		OutputPath:    lossyPath,
	})

	if !IsLosslessCodec(input.SourceCodec) {
		return nil
	}

	for _, codec := range SegmentCodecs {
		outPath := filepath.Join(sourceDir, "gapless-"+codec+".mp4")
		if _, err := t.db.CreateSegmentSet(ctx, sqlc.CreateSegmentSetParams{
			VersionID: input.VersionID,
			Codec:     codec,
			FilePath:  outPath,
		}); err != nil {
			return fmt.Errorf("failed to create %s segment set: %w", codec, err)
		}
	}

	t.QueueJob(Job{
		Kind:        JobKindSegments,
		VersionID:   input.VersionID,
		SourcePath:  input.SourceFilePath,
		SourceCodec: input.SourceCodec,
	})

	return nil
}
