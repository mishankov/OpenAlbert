package integrations

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndependentSTTMultipart(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer speech-only" {
			t.Error("incorrect STT route or credentials")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("model") != "nemotron-3.5" || r.FormValue("response_format") != "json" {
			t.Error("incorrect STT fields")
		}
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		if string(b) != "opus" || h.Filename != "voice.ogg" {
			t.Error("incorrect source audio")
		}
		_, _ = io.WriteString(w, `{"text":"Привет, Альберт","duration_seconds":1,"decode_ms":3,"inference_ms":12}`)
	}))
	defer s.Close()
	c, err := NewSTT(STTConfig{BaseURL: s.URL + "/v1/", APIKey: "speech-only", Model: "nemotron-3.5", Revision: "weights-v1"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Transcribe(context.Background(), "../../voice.ogg", []byte("opus"))
	if err != nil || r.Text != "Привет, Альберт" || r.InferenceMS != 12 {
		t.Fatalf("%+v %v", r, err)
	}
	if strings.Contains(c.Profile(), "speech-only") {
		t.Fatal("credential leaked into profile")
	}
}

func TestSTTErrorsDoNotExposeUpstreamBodies(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, "PRIVATE_UPSTREAM_SECRET")
	}))
	defer s.Close()
	c, _ := NewSTT(STTConfig{BaseURL: s.URL, Model: "m", Revision: "v1"})
	_, err := c.Transcribe(context.Background(), "v.ogg", []byte("x"))
	if err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("unsafe error %v", err)
	}
}

func TestSTTRejectsMissingTextButAcceptsSilence(t *testing.T) {
	for _, body := range []string{`{}`, `{"text":""}`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		c, _ := NewSTT(STTConfig{BaseURL: s.URL, Model: "m", Revision: "v1"})
		r, e := c.Transcribe(context.Background(), "v.ogg", []byte("x"))
		s.Close()
		if body == `{}` && e == nil {
			t.Fatal("missing text accepted")
		}
		if body != `{}` && (e != nil || r.Text != "") {
			t.Fatal("silence rejected")
		}
	}
}
