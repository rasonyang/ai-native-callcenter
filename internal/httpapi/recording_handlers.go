// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// RecordingStreamer opens stored call audio for playback.
type RecordingStreamer interface {
	Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error)
}

// ListCallRecordings lists a call's audio artifacts.
func (s *Server) ListCallRecordings(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	recordings, err := s.ledger.RecordingsByCall(r.Context(), callID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list recordings", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": recordings})
}

// GetRecordingAudio streams one recording.
//
// ServeContent does the heavy lifting: range requests for scrubbing, and
// conditional responses. The reader seeks, which is what makes that possible
// on both backends.
func (s *Server) GetRecordingAudio(w http.ResponseWriter, r *http.Request, recordingID uuid.UUID) {
	rec, err := s.ledger.GetRecording(r.Context(), recordingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such recording", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the recording", nil)
		return
	}
	if s.recordings == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "recording storage is not configured", nil)
		return
	}

	audio, _, err := s.recordings.Open(r.Context(), rec.ObjectKey)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "the audio is no longer stored", nil)
		return
	}
	defer audio.Close()

	w.Header().Set("Content-Type", "audio/wav")
	http.ServeContent(w, r, rec.CallID.String()+".wav", rec.CreatedAt, audio)
}

// reviewRequest is a reviewer scoring a recording.
type reviewRequest struct {
	Scores     map[string]int `json:"scores"`
	TotalScore int            `json:"totalScore"`
	Notes      string         `json:"notes"`
}

// CreateRecordingReview records a quality review against a recording.
func (s *Server) CreateRecordingReview(w http.ResponseWriter, r *http.Request, recordingID uuid.UUID) {
	var req reviewRequest
	if !decode(w, r, &req) {
		return
	}
	if req.TotalScore < 0 || req.TotalScore > 100 {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "totalScore out of range",
			map[string]any{"min": 0, "max": 100})
		return
	}

	rec, err := s.ledger.GetRecording(r.Context(), recordingID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such recording", nil)
		return
	}

	identity, _ := identityFrom(r.Context())
	review, err := s.ledger.InsertQualityReview(r.Context(), store.QualityReview{
		RecordingID: rec.ID,
		CallID:      rec.CallID,
		ReviewerID:  identity.UserID,
		Scores:      req.Scores,
		TotalScore:  req.TotalScore,
		Notes:       req.Notes,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot save the review", nil)
		return
	}
	writeJSON(w, http.StatusCreated, review)
}

// ListCallReviews lists a call's reviews.
func (s *Server) ListCallReviews(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	reviews, err := s.ledger.ReviewsByCall(r.Context(), callID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list reviews", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": reviews})
}
