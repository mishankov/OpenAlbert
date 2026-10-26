package sttserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func upload(t *testing.T, model string, b []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("model", model)
	_ = w.WriteField("response_format", "json")
	f, _ := w.CreateFormFile("file", "voice.ogg")
	_, _ = f.Write(b)
	_ = w.Close()
	r := httptest.NewRequest("POST", "/v1/audio/transcriptions", &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}
func wav(seconds int) []byte {
	size := seconds * 32000
	b := make([]byte, 44+size)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(size))
	return b
}
func TestWAVDurationAndMalformedData(t *testing.T) {
	if d, e := wavDuration(wav(2)); e != nil || d != 2 {
		t.Fatalf("duration %f %v", d, e)
	}
	for _, b := range [][]byte{nil, []byte("ogg"), wav(1)[:40]} {
		if _, e := wavDuration(b); e == nil {
			t.Fatal("malformed WAV accepted")
		}
	}
}

func TestReadinessRequiresLoadedNativeModel(t *testing.T) {
	for _, tc := range []struct {
		body string
		code int
	}{{`{"ready":false}`, 503}, {`{"ready":true}`, 200}, {`{}`, 503}} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) }))
		s := New("m", "key", up.URL)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "/ready", nil))
		up.Close()
		if w.Code != tc.code {
			t.Fatalf("readiness %s: %d", tc.body, w.Code)
		}
	}
}
func TestAdapterAuthenticationModelAndBackpressure(t *testing.T) {
	s := New("m", "key", "http://127.0.0.1:1")
	r := upload(t, "m", []byte("x"))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	r = upload(t, "other", []byte("x"))
	r.Header.Set("Authorization", "Bearer key")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	s.slot <- struct{}{}
	defer func() { <-s.slot }()
	r = upload(t, "m", []byte("x"))
	r.Header.Set("Authorization", "Bearer key")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatal("missing bounded overload response")
	}
}
func ffmpeg(t *testing.T) string {
	t.Helper()
	p, e := exec.LookPath("ffmpeg")
	if e != nil {
		t.Skip("FFmpeg integration runs in the STT image")
	}
	return p
}
func TestOGGDecodingAndDurationLimit(t *testing.T) {
	bin := ffmpeg(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "source.ogg")
	if out, e := exec.Command(bin, "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:a", "libopus", path).CombinedOutput(); e != nil {
		t.Fatalf("fixture: %v %s", e, out)
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if e := r.ParseMultipartForm(1 << 20); e != nil {
			t.Error(e)
			return
		}
		defer r.MultipartForm.RemoveAll()
		f, _, e := r.FormFile("file")
		if e != nil {
			t.Error(e)
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		d, e := wavDuration(b)
		if e != nil || d < 1.9 || d > 2.1 || r.FormValue("language") != "ru-RU" {
			t.Error("invalid normalized audio")
		}
		_, _ = io.WriteString(w, `{"text":"Тестовая расшифровка"}`)
	}))
	defer upstream.Close()
	s := New("m", "", upstream.URL)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, upload(t, "m", data))
	var result speechText
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result.Text != "Тестовая расшифровка" || result.DurationSeconds < 1.9 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	s.MaxDuration = time.Second
	w = httptest.NewRecorder()
	s.ServeHTTP(w, upload(t, "m", data))
	if w.Code != 413 || calls != 1 {
		t.Fatal("overlong audio reached inference")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, upload(t, "m", []byte("broken")))
	if w.Code != 422 {
		t.Fatal("broken audio accepted")
	}
}
func TestCancelledDecodeNeverCallsInference(t *testing.T) {
	bin := ffmpeg(t)
	s := New("m", "", "http://127.0.0.1:1")
	s.FFmpeg = bin
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, upload(t, "m", wav(1)).WithContext(ctx))
	if len(s.slot) != 0 {
		t.Fatal("slot leaked")
	}
}

func TestDisconnectedClientKeepsSlotUntilNativeCallFinishes(t *testing.T) {
	bin := ffmpeg(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, `{"text":"Готово"}`)
	}))
	defer upstream.Close()
	s := New("m", "", upstream.URL)
	s.FFmpeg = bin
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(httptest.NewRecorder(), upload(t, "m", wav(1)).WithContext(ctx))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("native call not reached")
	}
	cancel()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, upload(t, "m", wav(1)))
	if w.Code != 503 {
		t.Error("disconnected client released native capacity")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("slot did not finish")
	}
	if len(s.slot) != 0 {
		t.Fatal("native slot leaked")
	}
}
