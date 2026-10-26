package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"openalbert/apps/backend/internal/domain"
	"strings"
	"sync"
	"time"
)

type AudioInput struct {
	Run                    RunInput
	IncomingID, ArtifactID string
	QueuedAt               time.Time
}

func audioResultKey(session, incoming, artifact string) string {
	return "audio/" + ID(session, incoming, artifact)
}
func (a *Activities) saveAudio(ctx context.Context, r domain.AudioResult) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return a.Repo.SaveCheckpoint(ctx, audioResultKey(r.SessionID, r.IncomingID, r.ArtifactID), b)
}
func (a *Activities) loadAudio(ctx context.Context, session, incoming, artifact string) (r domain.AudioResult, ok bool, err error) {
	b, ok, err := a.Repo.LoadCheckpoint(ctx, audioResultKey(session, incoming, artifact))
	if err == nil && ok {
		err = json.Unmarshal(b, &r)
	}
	return r, ok, err
}
func audioAttachment(at domain.Attachment) bool {
	return strings.HasPrefix(at.MIME, "audio/") || at.Kind == "voice" || at.Kind == "audio"
}

type attachmentDownloadError struct{ cause error }

func (e *attachmentDownloadError) Error() string { return "Attachment download failed" }
func (e *attachmentDownloadError) Unwrap() error { return e.cause }

func (a *Activities) attachmentArtifact(ctx context.Context, in RunInput, id string, index int, at domain.Attachment) (domain.Artifact, error) {
	if at.ArtifactID != "" {
		art, found, err := a.Repo.Artifact(ctx, in.ConversationID, at.ArtifactID)
		if err != nil {
			return art, err
		}
		if !found {
			return art, errors.New("attachment artifact missing")
		}
		return art, nil
	}
	artifactID := ID(id, fmt.Sprint(index))
	if prior, ok, err := a.Repo.Artifact(ctx, in.ConversationID, artifactID); err != nil {
		return prior, err
	} else if ok {
		return prior, nil
	}
	data := at.Data
	var err error
	if len(data) == 0 && at.FileID != "" {
		data, err = a.Messenger.Download(ctx, at.FileID)
		if err != nil {
			return domain.Artifact{}, &attachmentDownloadError{cause: err}
		}
	}
	art := domain.Artifact{ID: artifactID, SessionID: in.SessionID, ConversationID: in.ConversationID, Name: at.Name, MIME: at.MIME, Data: data, CreatedAt: time.Now().UTC()}
	err = a.Repo.SaveArtifact(ctx, art)
	return art, err
}

// PrepareAudio saves originals before any inference. No audio bytes enter the
// workflow history, and the inspector can show an attachment while it is queued.
func (a *Activities) PrepareAudio(ctx context.Context, in RunInput) (jobs []AudioInput, err error) {
	ctx = runContext(ctx, in)
	if activity.IsActivity(ctx) {
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				activity.RecordHeartbeat(ctx, "saving audio")
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
		defer func() { close(stop); <-done }()
	}
	if err = a.Repo.StartSession(ctx, domain.Session{ID: in.SessionID, ConversationID: in.ConversationID, Status: "active", CreatedAt: time.Now().UTC()}); err != nil {
		return
	}
	for _, id := range in.IncomingIDs {
		if _, done, e := a.Repo.LoadCheckpoint(ctx, "incoming-done/"+id); e != nil {
			return nil, e
		} else if done {
			continue
		}
		msg, ok, e := a.Repo.Incoming(ctx, id)
		if e != nil {
			return nil, e
		}
		if !ok {
			continue
		}
		if msg.Conversation.ID != in.ConversationID {
			return nil, errors.New("incoming scope mismatch")
		}
		for i, at := range msg.Attachments {
			if !audioAttachment(at) {
				continue
			}
			art, e := a.attachmentArtifact(ctx, in, id, i, at)
			if e != nil {
				return nil, e
			}
			job := AudioInput{Run: in, IncomingID: id, ArtifactID: art.ID, QueuedAt: time.Now().UTC()}
			prior, found, e := a.loadAudio(ctx, in.SessionID, id, art.ID)
			if e != nil {
				return nil, e
			}
			if found && prior.Status != "queued" && prior.Status != "running" {
				continue
			}
			result := domain.AudioResult{ConversationID: in.ConversationID, AuthorID: msg.Conversation.OwnerID, AuthorName: msg.AuthorName, SessionID: in.SessionID, IncomingID: id, ArtifactID: art.ID, Model: a.STTModel, Status: "queued", QueuedAt: job.QueuedAt}
			if found {
				job.QueuedAt = prior.QueuedAt
				result.QueuedAt = prior.QueuedAt
			}
			if a.STT == nil {
				result.Status = "unavailable"
				result.ErrorCode = "stt_disabled"
			} else if at.DurationSeconds > 300 {
				result.Status = "failed"
				result.ErrorCode = "duration_limit"
			}
			if e = a.saveAudio(ctx, result); e != nil {
				return nil, e
			}
			if result.Status == "queued" {
				jobs = append(jobs, job)
			}
		}
	}
	return
}

func (a *Activities) TranscribeAudio(ctx context.Context, in AudioInput) (out domain.AudioResult, err error) {
	ctx = runContext(ctx, in.Run)
	out = domain.AudioResult{SessionID: in.Run.SessionID, IncomingID: in.IncomingID, ArtifactID: in.ArtifactID, Model: a.STTModel, Status: "running", QueuedAt: in.QueuedAt, StartedAt: time.Now().UTC()}
	if prior, ok, e := a.loadAudio(ctx, in.Run.SessionID, in.IncomingID, in.ArtifactID); e != nil {
		return out, e
	} else if ok && prior.Status != "queued" && prior.Status != "running" {
		return prior, nil
	} else if ok {
		out.ConversationID = prior.ConversationID
		out.AuthorID = prior.AuthorID
		out.AuthorName = prior.AuthorName
		if !prior.StartedAt.IsZero() {
			out.StartedAt = prior.StartedAt
		}
	}
	if a.STT == nil {
		return out, temporal.NewNonRetryableApplicationError("STT is disabled", "stt_disabled", nil)
	}
	art, ok, err := a.Repo.Artifact(ctx, in.Run.ConversationID, in.ArtifactID)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, temporal.NewNonRetryableApplicationError("Audio artifact is missing", "audio_missing", nil)
	}
	sum := sha256.Sum256(art.Data)
	key := ID(in.Run.ConversationID, hex.EncodeToString(sum[:]), a.STT.Profile())
	owner := rand.Text()
	result, cached, claimed, err := a.Repo.ClaimTranscription(ctx, key, owner)
	if err != nil {
		return out, err
	}
	if cached {
		out.SpeechText = result
		out.Cached = true
		out.Status = "completed"
		if strings.TrimSpace(result.Text) == "" {
			out.Status = "no_speech"
		}
		out.EndedAt = time.Now().UTC()
		err = a.saveAudio(ctx, out)
		return
	}
	if !claimed {
		return out, temporal.NewApplicationError("Another attempt owns audio inference", "stt_busy")
	}
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_ = a.Repo.ReleaseTranscription(cleanup, key, owner)
	}()
	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan struct{})
	var stopOnce sync.Once
	stopBeating := func() { stopOnce.Do(func() { close(stopHeartbeat); <-heartbeatDone }) }
	go func() {
		defer close(heartbeatDone)
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			if activity.IsActivity(c) {
				activity.RecordHeartbeat(c, "transcribing")
			}
			select {
			case <-stopHeartbeat:
				return
			case <-c.Done():
				return
			case <-tick.C:
				_, hit, ours, e := a.Repo.ClaimTranscription(c, key, owner)
				if e != nil || hit || !ours {
					cancel()
					return
				}
			}
		}
	}()
	defer stopBeating()
	if err = a.saveAudio(c, out); err != nil {
		return
	}
	finish := a.startTrace(c, domain.Trace{SessionID: in.Run.SessionID, TurnID: in.Run.Key, Trigger: "message", Kind: "transcription", Name: a.STTModel, CallID: in.ArtifactID}, map[string]any{"incoming_id": in.IncomingID, "artifact_id": in.ArtifactID, "profile": ID(a.STT.Profile()), "queued_at": in.QueuedAt, "size_bytes": len(art.Data)}, nil)
	defer func() { finish(out, err) }()
	out.SpeechText, err = a.STT.Transcribe(c, art.Name, art.Data)
	if err != nil {
		err = speechError(c, err)
		return
	}
	if err = c.Err(); err != nil {
		return out, err
	}
	// Stop lease renewal before our own commit turns the row into a cache hit;
	// otherwise the renewal goroutine could mistake that hit for lost ownership.
	stopBeating()
	out.Status = "completed"
	if strings.TrimSpace(out.Text) == "" {
		out.Status = "no_speech"
	}
	out.EndedAt = time.Now().UTC()
	committed, e := a.Repo.CompleteTranscription(c, key, owner, out.SpeechText)
	if e != nil {
		return out, e
	}
	if !committed {
		return out, temporal.NewApplicationError("Audio lease was lost", "stt_lease_lost")
	}
	err = a.saveAudio(c, out)
	return
}

func speechError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var status interface{ HTTPStatus() int }
	if errors.As(err, &status) {
		code := status.HTTPStatus()
		if code >= 400 && code < 500 && code != 408 && code != 429 {
			return temporal.NewNonRetryableApplicationError(fmt.Sprintf("STT rejected audio (HTTP %d)", code), fmt.Sprintf("stt_http_%d", code), nil)
		}
		return temporal.NewApplicationError(fmt.Sprintf("STT temporarily unavailable (HTTP %d)", code), "stt_unavailable")
	}
	return temporal.NewApplicationError("STT request failed", "stt_request_failed")
}

type AudioFailure struct {
	Input AudioInput
	Code  string
}

func (a *Activities) RecordAudioFailure(ctx context.Context, in AudioFailure) error {
	out, ok, err := a.loadAudio(ctx, in.Input.Run.SessionID, in.Input.IncomingID, in.Input.ArtifactID)
	if err != nil {
		return err
	}
	if !ok {
		out = domain.AudioResult{SessionID: in.Input.Run.SessionID, IncomingID: in.Input.IncomingID, ArtifactID: in.Input.ArtifactID, Model: a.STTModel, QueuedAt: in.Input.QueuedAt}
	}
	// Cancellation can race with a successful, durable inference. Keep the text
	// for inspection, while the workflow still suppresses the cancelled turn.
	if out.Status == "completed" || out.Status == "no_speech" {
		return nil
	}
	out.Status = "failed"
	if in.Code == "cancelled" {
		out.Status = "cancelled"
	}
	out.ErrorCode = in.Code
	out.EndedAt = time.Now().UTC()
	return a.saveAudio(ctx, out)
}
