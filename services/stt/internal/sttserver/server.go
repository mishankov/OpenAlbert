// Package sttserver adapts completed Telegram audio to NeMo-Speech.cpp's WAV API.
package sttserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// speechText is the service's JSON response, consumed by the backend's STT adapter.
type speechText struct {
	Text            string  `json:"text"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	DecodeMS        int64   `json:"decode_ms,omitempty"`
	InferenceMS     int64   `json:"inference_ms,omitempty"`
}

const MaxBytes = 20 << 20

type Server struct {
	Model, APIKey, Upstream string
	MaxDuration             time.Duration
	Client                  *http.Client
	FFmpeg                  string
	slot                    chan struct{}
}

func New(model, key, upstream string) *Server {
	return &Server{Model: model, APIKey: key, Upstream: upstream, MaxDuration: 5 * time.Minute, Client: &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, FFmpeg: "ffmpeg", slot: make(chan struct{}, 1)}
}
func fail(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message}})
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && (r.URL.Path == "/healthz" || r.URL.Path == "/ready") {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", s.Upstream+"/ready", nil)
		resp, err := s.Client.Do(req)
		if err != nil {
			fail(w, 503, "ASR is not ready")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			fail(w, 503, "ASR is not ready")
			return
		}
		var ready struct {
			Ready bool `json:"ready"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ready) != nil || !ready.Ready {
			fail(w, 503, "ASR is not ready")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	if s.APIKey != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.APIKey)) != 1 {
		fail(w, 401, "Unauthorized")
		return
	}
	if r.Method != "POST" || r.URL.Path != "/v1/audio/transcriptions" {
		fail(w, 404, "Not found")
		return
	}
	select {
	case s.slot <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "15")
		fail(w, 503, "ASR is busy")
		return
	}
	defer func() { <-s.slot }()
	r.Body = http.MaxBytesReader(w, r.Body, MaxBytes+(64<<10))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
		fail(w, 413, "Invalid or oversized multipart upload")
		return
	}
	defer r.MultipartForm.RemoveAll()
	if r.FormValue("model") != s.Model {
		fail(w, 400, "The requested model is not loaded")
		return
	}
	if f := r.FormValue("response_format"); f != "" && f != "json" {
		fail(w, 400, "Only response_format=json is supported")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, "An audio file is required")
		return
	}
	defer file.Close()
	dir, err := os.MkdirTemp("", "openalbert-audio-")
	if err != nil {
		fail(w, 500, "Temporary storage unavailable")
		return
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "input")
	output := filepath.Join(dir, "audio.wav")
	f, err := os.Create(input)
	if err != nil {
		fail(w, 500, "Temporary storage unavailable")
		return
	}
	n, copyErr := io.Copy(f, io.LimitReader(file, MaxBytes+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		fail(w, 500, "Audio could not be saved")
		return
	}
	if n == 0 || n > MaxBytes {
		fail(w, 413, "Audio exceeds size limit")
		return
	}
	decodeStart := time.Now()
	decodeCtx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	cmd := exec.CommandContext(decodeCtx, s.FFmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,pipe", "-format_whitelist", "ogg,wav,mp3,flac,mov,matroska,webm,aac", "-threads", "1", "-i", input, "-t", strconv.FormatFloat(s.MaxDuration.Seconds()+1, 'f', 3, 64), "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-threads", "1", output)
	err = cmd.Run()
	cancel()
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		fail(w, 422, "Audio could not be decoded within the time limit")
		return
	}
	wav, err := os.ReadFile(output)
	if err != nil || len(wav) > MaxBytes {
		fail(w, 413, "Decoded audio exceeds size limit")
		return
	}
	duration, err := wavDuration(wav)
	if err != nil {
		fail(w, 422, "Invalid decoded WAV")
		return
	}
	if duration > s.MaxDuration.Seconds() {
		fail(w, 413, "Audio exceeds duration limit")
		return
	}
	decodeMS := time.Since(decodeStart).Milliseconds()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", s.Model)
	_ = mw.WriteField("response_format", "json")
	_ = mw.WriteField("language", "ru-RU")
	p, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = p.Write(wav)
	_ = mw.Close()
	// A disconnected client must not release the slot while the native engine is
	// still computing: its file API does not promise server-side cancellation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", s.Upstream+"/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	start := time.Now()
	resp, err := s.Client.Do(req)
	if err != nil {
		fail(w, 504, "ASR request failed or timed out")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fail(w, 502, "ASR inference failed")
		return
	}
	var payload struct {
		Text *string `json:"text"`
	}
	response, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if readErr != nil || len(response) > 1<<20 || json.Unmarshal(response, &payload) != nil || payload.Text == nil {
		fail(w, 502, "Invalid ASR response")
		return
	}
	result := speechText{Text: *payload.Text}
	result.DurationSeconds = duration
	result.DecodeMS = decodeMS
	result.InferenceMS = time.Since(start).Milliseconds()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
func wavDuration(b []byte) (float64, error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, fmt.Errorf("invalid WAV")
	}
	var rate uint32
	for i := 12; i+8 <= len(b); {
		size := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		start := i + 8
		if size > len(b)-start {
			return 0, fmt.Errorf("invalid chunk")
		}
		switch string(b[i : i+4]) {
		case "fmt ":
			if size < 16 {
				return 0, fmt.Errorf("invalid format")
			}
			rate = binary.LittleEndian.Uint32(b[start+8 : start+12])
		case "data":
			if rate == 0 {
				return 0, fmt.Errorf("missing format")
			}
			return float64(size) / float64(rate), nil
		}
		i = start + size + (size % 2)
	}
	return 0, fmt.Errorf("missing audio data")
}
