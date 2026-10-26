// Package desktopapi exposes only transport-visible owner data, never agent traces.
package desktopapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
)

const MaxAttachmentBytes = 20 << 20

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type MemoryReader interface {
	ListMemory(context.Context, string) ([]domain.MemoryEntry, error)
}
type API struct {
	Repo    domain.Repository
	Memory  MemoryReader
	OwnerID int64
	Token   string
}
type Attachment struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MIME      string `json:"mime"`
	SizeBytes int64  `json:"size_bytes"`
}
type Message struct {
	ID          string       `json:"id"`
	Role        string       `json:"role"`
	Text        string       `json:"text"`
	Format      string       `json:"format"`
	CreatedAt   time.Time    `json:"created_at"`
	Delivery    string       `json:"delivery"`
	Attachments []Attachment `json:"attachments"`
	TaskID      *string      `json:"task_id"`
}
type sendRequest struct {
	ClientMessageID string   `json:"client_message_id"`
	Text            string   `json:"text"`
	AttachmentIDs   []string `json:"attachment_ids"`
	TaskID          *string  `json:"task_id"`
}

func (a *API) scope() string { return fmt.Sprintf("personal:%d", a.OwnerID) }
func (a *API) Register(mux *http.ServeMux) {
	if a.Token == "" {
		return
	}
	mux.Handle("/api/v1/", a.Handler())
}
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", a.status)
	mux.HandleFunc("GET /api/v1/messages", a.messages)
	mux.HandleFunc("POST /api/v1/messages", a.send)
	mux.HandleFunc("POST /api/v1/stop", a.stop)
	mux.HandleFunc("GET /api/v1/tasks", a.tasks)
	mux.HandleFunc("GET /api/v1/memory", a.memory)
	mux.HandleFunc("GET /api/v1/files", a.files)
	mux.HandleFunc("POST /api/v1/attachments", a.upload)
	mux.HandleFunc("GET /api/v1/attachments/{id}/content", a.download)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		expected := sha256.Sum256([]byte(a.Token))
		actual := sha256.Sum256([]byte(token))
		if a.Token == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
			fail(w, 401, "unauthorized", "Authentication required")
			return
		}
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			fail(w, 403, "invalid_request", "Browser origins are not allowed")
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func internal(w http.ResponseWriter) {
	fail(w, 503, "unavailable", "Albert is temporarily unavailable")
}
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, "too_large", "Request body is too large")
		} else {
			fail(w, 400, "invalid_request", "Invalid JSON request")
		}
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_request", "Expected one JSON object")
		return false
	}
	return true
}
func (a *API) status(w http.ResponseWriter, r *http.Request) {
	activity, err := a.Repo.ConversationActivity(r.Context(), a.scope())
	if err != nil {
		internal(w)
		return
	}
	respond(w, 200, map[string]any{"api_version": 1, "name": "Альберт", "conversation_id": a.scope(), "max_attachment_bytes": MaxAttachmentBytes, "activity": activity})
}
func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func attachment(art domain.Artifact) Attachment {
	n := art.SizeBytes
	if n == 0 {
		n = int64(len(art.Data))
	}
	return Attachment{art.ID, art.Name, art.MIME, n}
}
func artifactID(inID string, i int) string {
	sum := sha256.Sum256([]byte(inID + "\x00" + strconv.Itoa(i)))
	return hex.EncodeToString(sum[:16])
}
func (a *API) messageMetadata(ctx context.Context) (map[string]Attachment, error) {
	arts, err := a.Repo.ListArtifacts(ctx, a.scope())
	if err != nil {
		return nil, err
	}
	out := make(map[string]Attachment, len(arts))
	for _, art := range arts {
		out[art.ID] = attachment(art)
	}
	return out, nil
}
func (a *API) incomingMessage(ctx context.Context, in domain.Incoming, metadata map[string]Attachment) (Message, error) {
	m := Message{ID: in.ID, Role: "user", Text: in.Text, Format: "plain", CreatedAt: in.CreatedAt, Delivery: "accepted", Attachments: []Attachment{}, TaskID: ptr(in.TaskContextID)}
	for i, at := range in.Attachments {
		id := at.ArtifactID
		if id == "" {
			id = artifactID(in.ID, i)
		}
		if art, ok := metadata[id]; ok {
			m.Attachments = append(m.Attachments, art)
		}
	}
	return m, nil
}
func (a *API) outgoingMessage(ctx context.Context, out domain.Outgoing, metadata map[string]Attachment) (Message, error) {
	m := Message{ID: "outgoing:" + out.ID, Role: "assistant", Text: out.Text, Format: "plain", CreatedAt: out.EnqueuedAt, Delivery: out.State, Attachments: []Attachment{}, TaskID: ptr(out.TaskID)}
	if !out.Formatted {
		m.Format = "markdown"
	}
	if out.Markdown != "" {
		m.Text = out.Markdown
		m.Format = "markdown"
	}
	if m.Delivery == "sending" {
		m.Delivery = "pending"
	}
	if out.ArtifactID != "" {
		if art, ok := metadata[out.ArtifactID]; ok {
			m.Attachments = append(m.Attachments, art)
		}
	}
	return m, nil
}
func cursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(seq, 10)))
}
func (a *API) messages(w http.ResponseWriter, r *http.Request) {
	n := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, e := strconv.Atoi(raw)
		if e != nil || v < 1 || v > 100 {
			fail(w, 400, "invalid_request", "limit must be 1–100")
			return
		}
		n = v
	}
	var before int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		b, e := base64.RawURLEncoding.DecodeString(raw)
		if e != nil {
			fail(w, 400, "invalid_request", "Invalid cursor")
			return
		}
		before, e = strconv.ParseInt(string(b), 10, 64)
		if e != nil || before < 1 {
			fail(w, 400, "invalid_request", "Invalid cursor")
			return
		}
	}
	records, err := a.Repo.PublicTimeline(r.Context(), a.scope(), before, n+1)
	if err != nil {
		internal(w)
		return
	}
	metadata, err := a.messageMetadata(r.Context())
	if err != nil {
		internal(w)
		return
	}
	more := len(records) > n
	if more {
		records = records[:n]
	}
	messages := []Message{}
	for i := len(records) - 1; i >= 0; i-- {
		var m Message
		var e error
		if records[i].Incoming != nil {
			m, e = a.incomingMessage(r.Context(), *records[i].Incoming, metadata)
		} else {
			m, e = a.outgoingMessage(r.Context(), *records[i].Outgoing, metadata)
		}
		if e != nil {
			internal(w)
			return
		}
		messages = append(messages, m)
	}
	var next *string
	if more && len(records) > 0 {
		v := cursor(records[len(records)-1].Seq)
		next = &v
	}
	respond(w, 200, map[string]any{"messages": messages, "has_more": more, "next_before": next})
}
func (a *API) accept(ctx context.Context, req sendRequest, stop bool) (domain.Incoming, int, string) {
	if !uuidPattern.MatchString(req.ClientMessageID) {
		return domain.Incoming{}, 400, "client_message_id must be a UUID"
	}
	if !utf8.ValidString(req.Text) || len(req.Text) > 32000 || len(req.AttachmentIDs) > 10 || (!stop && strings.TrimSpace(req.Text) == "" && len(req.AttachmentIDs) == 0) {
		return domain.Incoming{}, 400, "Expected text up to 32000 UTF-8 bytes or up to 10 attachments"
	}
	in := domain.Incoming{ID: "desktop:" + strings.ToLower(req.ClientMessageID), Source: "desktop", Text: req.Text, CreatedAt: time.Now().UTC()}
	if stop {
		in.Command = "stop"
		in.Text = "/stop"
	}
	if req.TaskID != nil {
		in.TaskContextID = *req.TaskID
	}
	// A retry is compared against its original immutable payload before dependencies.
	if prior, ok, e := a.Repo.Incoming(ctx, in.ID); e != nil {
		return in, 503, ""
	} else if ok {
		if prior.Conversation.ID == a.scope() && samePayload(prior, in, req.AttachmentIDs) {
			return prior, 202, ""
		}
		return in, 409, "Message ID has already been used with another payload"
	}
	conv, ok, err := a.Repo.Conversation(ctx, a.scope())
	if err != nil {
		return in, 503, ""
	}
	if !ok {
		conv = domain.Conversation{ID: a.scope(), ChatID: a.OwnerID, OwnerID: a.OwnerID, Private: true}
	}
	conv.DeliveryChannel = ""
	in.Conversation = conv
	if in.TaskContextID != "" {
		_, ok, e := a.Repo.Task(ctx, a.scope(), in.TaskContextID)
		if e != nil {
			return in, 503, ""
		}
		if !ok {
			return in, 404, "Task not found"
		}
	}
	var metadata map[string]Attachment
	if len(req.AttachmentIDs) > 0 {
		metadata, err = a.messageMetadata(ctx)
		if err != nil {
			return in, 503, ""
		}
	}
	for _, id := range req.AttachmentIDs {
		art, ok := metadata[id]
		if !ok {
			return in, 404, "Attachment not found"
		}
		kind := "document"
		if strings.HasPrefix(art.MIME, "image/") {
			kind = "photo"
		} else if strings.HasPrefix(art.MIME, "audio/") {
			kind = "audio"
		}
		in.Attachments = append(in.Attachments, domain.Attachment{ArtifactID: art.ID, Name: art.Name, MIME: art.MIME, SizeBytes: art.SizeBytes, Kind: kind})
	}
	accepted, err := a.Repo.AcceptIncoming(ctx, in, 0)
	if err != nil {
		return in, 503, ""
	}
	if !accepted {
		prior, ok, e := a.Repo.Incoming(ctx, in.ID)
		if e != nil || !ok {
			return in, 503, ""
		}
		if prior.Conversation.ID != a.scope() || !samePayload(prior, in, req.AttachmentIDs) {
			return in, 409, "Message ID has already been used with another payload"
		}
		in = prior
	}
	return in, 202, ""
}
func samePayload(a, b domain.Incoming, ids []string) bool {
	if a.Source != "desktop" || a.Text != b.Text || a.Command != b.Command || a.TaskContextID != b.TaskContextID || len(a.Attachments) != len(ids) {
		return false
	}
	for i, id := range ids {
		if a.Attachments[i].ArtifactID != id {
			return false
		}
	}
	return true
}
func acceptanceError(w http.ResponseWriter, status int, msg string) {
	switch status {
	case 400:
		fail(w, status, "invalid_request", msg)
	case 404:
		fail(w, status, "not_found", msg)
	case 409:
		fail(w, status, "conflict", msg)
	default:
		internal(w)
	}
}
func (a *API) send(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if !decodeBody(w, r, &req) {
		return
	}
	in, status, msg := a.accept(r.Context(), req, false)
	if status != 202 {
		acceptanceError(w, status, msg)
		return
	}
	metadata, err := a.messageMetadata(r.Context())
	if err != nil {
		internal(w)
		return
	}
	m, err := a.incomingMessage(r.Context(), in, metadata)
	if err != nil {
		internal(w)
		return
	}
	respond(w, 202, map[string]any{"message": m})
}
func (a *API) stop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientMessageID string `json:"client_message_id"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	_, status, msg := a.accept(r.Context(), sendRequest{ClientMessageID: req.ClientMessageID}, true)
	if status != 202 {
		acceptanceError(w, status, msg)
		return
	}
	respond(w, 202, map[string]bool{"accepted": true})
}
func (a *API) tasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := a.Repo.FindTasks(r.Context(), a.scope(), "")
	if err != nil {
		internal(w)
		return
	}
	if tasks == nil {
		tasks = []domain.Task{}
	}
	respond(w, 200, map[string]any{"tasks": tasks})
}
func (a *API) memory(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		Path      string    `json:"path"`
		Content   string    `json:"content"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	out := []entry{}
	if a.Memory != nil {
		entries, err := a.Memory.ListMemory(r.Context(), a.scope())
		if err != nil {
			internal(w)
			return
		}
		for _, e := range entries {
			out = append(out, entry{e.ID, e.Content, e.UpdatedAt})
		}
	}
	respond(w, 200, map[string]any{"entries": out})
}
func (a *API) files(w http.ResponseWriter, r *http.Request) {
	arts, err := a.Repo.ListArtifacts(r.Context(), a.scope())
	if err != nil {
		internal(w)
		return
	}
	out := []Attachment{}
	for _, art := range arts {
		out = append(out, attachment(art))
	}
	respond(w, 200, map[string]any{"files": out})
}
func (a *API) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxAttachmentBytes+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "invalid_request", "Expected multipart file")
		return
	}
	var art domain.Artifact
	found := false
	for {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			fail(w, 413, "too_large", "Invalid or oversized upload")
			return
		}
		if part.FormName() != "file" || part.FileName() == "" || found {
			fail(w, 400, "invalid_request", "Expected exactly one file part")
			return
		}
		found = true
		data, e := io.ReadAll(io.LimitReader(part, MaxAttachmentBytes+1))
		if e != nil {
			fail(w, 400, "invalid_request", "Cannot read upload")
			return
		}
		if len(data) > MaxAttachmentBytes {
			fail(w, 413, "too_large", "Attachment exceeds 20 MiB")
			return
		}
		name := filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		if len(name) > 255 || strings.ContainsAny(name, "\r\n\x00") {
			fail(w, 400, "invalid_request", "Invalid filename")
			return
		}
		kind, mimeErr := attachmentMIME(data, part.Header.Get("Content-Type"))
		if mimeErr != nil {
			fail(w, 400, "invalid_request", mimeErr.Error())
			return
		}
		random := make([]byte, 16)
		if _, e = rand.Read(random); e != nil {
			internal(w)
			return
		}
		art = domain.Artifact{ID: "upload-" + hex.EncodeToString(random), ConversationID: a.scope(), Name: name, MIME: kind, Data: data, SizeBytes: int64(len(data)), CreatedAt: time.Now().UTC()}
	}
	if !found {
		fail(w, 400, "invalid_request", "Expected one file")
		return
	}
	if err = a.Repo.SaveArtifact(r.Context(), art); err != nil {
		internal(w)
		return
	}
	respond(w, 201, map[string]any{"attachment": attachment(art)})
}
func (a *API) download(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	art, ok, err := a.Repo.Artifact(r.Context(), a.scope(), id)
	if err != nil {
		internal(w)
		return
	}
	if !ok {
		fail(w, 404, "not_found", "Attachment not found")
		return
	}
	kind, _, e := mime.ParseMediaType(art.MIME)
	if e != nil {
		kind = "application/octet-stream"
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": art.Name}))
	w.Header().Set("Content-Length", strconv.Itoa(len(art.Data)))
	w.WriteHeader(200)
	_, _ = w.Write(art.Data)
}
