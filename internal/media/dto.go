package media

import (
	"strconv"
	"time"
)

// createUploadBody is the POST /api/v1/media/uploads declaration (R1).
type createUploadBody struct {
	FileName string `json:"file_name"`
	Size     int64  `json:"size"`
	MIME     string `json:"mime"`
	SHA256   string `json:"sha256"`
	Purpose  string `json:"purpose"`
}

// uploadView is the created session with its derived upload geometry (R1/R3).
type uploadView struct {
	UploadID    string    `json:"upload_id"`
	ChunkSize   int64     `json:"chunk_size,string"`
	TotalChunks int       `json:"total_chunks"`
	Status      string    `json:"status"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func viewFromSession(s SessionRow) uploadView {
	return uploadView{
		UploadID: s.ID, ChunkSize: s.ChunkSize, TotalChunks: s.TotalChunks,
		Status: s.Status, ExpiresAt: s.ExpiresAt,
	}
}

// uploadStatusView is the GET response: the session plus the received bitmap (R5).
type uploadStatusView struct {
	uploadView
	ReceivedChunks []int `json:"received_chunks"`
}

// completeView is the POST /complete result (R6).
type completeView struct {
	MediaObjectID string `json:"media_object_id"`
}

// downloadURLView is the short-lived URL answer (R14).
type downloadURLView struct {
	URL       string `json:"url"`
	ExpiresIn int64  `json:"expires_in_seconds"`
}

func viewFromObjectID(id int64) completeView {
	return completeView{MediaObjectID: strconv.FormatInt(id, 10)}
}
