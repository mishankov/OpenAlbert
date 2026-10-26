# Desktop API v1

This is the shared implementation contract for the first native macOS client.
It exposes the owner's single `personal:<owner id>` conversation, shared with
Telegram. Session IDs, model messages, reasoning, tool calls, and group history
are never part of this API. All dates are RFC 3339 UTC. JSON uses snake_case.

## Access

Every `/api/v1/` request, including downloads, requires
`Authorization: Bearer <token>`. `DESKTOP_API_TOKEN` enables the API; unset means
disabled. Use a randomly generated token of at least 32 bytes. The first version
has one owner token; device enrollment and separate revocation are future work.
The API accepts direct HTTP connections, including a local-network address such
as `http://192.168.1.31:8080`. Authentication is required for both HTTP and HTTPS.
Never put tokens in URLs or logs. HTTP transmits tokens and messages unencrypted.

Errors: `{"error":{"code":"unauthorized","message":"…"}}`.
Codes include `unauthorized`, `invalid_request`, `not_found`, `conflict`,
`too_large`, `unavailable`, and `internal`. Reject cross-origin browser requests;
there is no CORS API. JSON request bodies have a finite size limit.

## Routes

### GET /api/v1/status

`{"api_version":1,"name":"Альберт","conversation_id":"personal:123","max_attachment_bytes":20971520}`

### GET /api/v1/messages?limit=100&before=<opaque cursor>

Newest page by default, returned in chronological order. `limit` is 1–100.
`before` loads an older page. The client refreshes the newest page every two
seconds while active and merges by message ID, preserving loaded older pages.
After reconnect, it pages backwards until reaching known history, so receiving
more than 100 messages while offline does not leave a gap.
This version uses polling; there is no SSE endpoint or token-bearing image URL.

`{"messages":[Message],"has_more":false,"next_before":null}`

Message:
```
{
  "id":"desktop:550e8400-e29b-41d4-a716-446655440000",
  "role":"user",
  "text":"Посмотри этот снимок",
  "format":"plain",
  "created_at":"2026-10-26T10:00:00Z",
  "delivery":"accepted",
  "attachments":[Attachment],
  "task_id":null
}
```

Roles: `user`, `assistant`. Formats: `plain`, `markdown`.
Delivery: `accepted`, `pending`, `sent`, `failed`, `unknown`.
Incoming messages come from the durable inbox; public assistant messages come
from the outbox, never from raw model history. Telegram replies are shown only
after successful delivery (or explicit unknown delivery), desktop replies are
visible once durably queued. Stable IDs deduplicate reloads. No arbitrary
conversation parameter is accepted. Legacy Telegram formatting may fall back to
plain text; newly generated desktop output retains Markdown.

### POST /api/v1/messages

`{"client_message_id":"550e8400-e29b-41d4-a716-446655440000","text":"…","attachment_ids":[],"task_id":null}`

Returns 202 `{"message":Message}` after durable acceptance. Repeating the same
UUID and same payload returns the same message without a second turn. Reusing
an ID with different content returns 409. UUIDs are generated and retained by
the client before sending; ambiguous network failure must retry the same ID.
Text is at most 32,000 UTF-8 bytes; text or an attachment is required. At most
10 attachments. Attachments and optional task must belong to the owner scope.
Task context is supplied to the agent without adding boilerplate to visible text.
The existing durable dispatcher wakes the agent. Replies to desktop input go
to the desktop outbox, without an extra Telegram notification. Telegram input
and background tasks retain their existing delivery behavior.

### POST /api/v1/stop

`{"client_message_id":"<UUID>"}` → 202 `{"accepted":true}`.
An idempotent stop command for the single conversation, dispatched through the
existing workflow. It does not erase history or drafts.

### GET /api/v1/tasks

`{"tasks":[Task]}`. Task fields: `id`, `title`, `goal`, `status`, `reason`,
`result`, `schedule_seconds`, `next_run_at`, `created_at`, `updated_at`, `version`.
Statuses match the core: queued, running, waiting, paused, completed, cancelled,
failed, blocked. Optional values may be omitted or null. Discussing a task uses
POST messages with `task_id`; editing tasks happens through conversation in v1.

### GET /api/v1/memory

`{"entries":[{"path":"…","content":"…","updated_at":"…"}]}`.
Only personal memory is returned. Read-only in v1.

### GET /api/v1/files

`{"files":[Attachment]}`. Only artifacts belonging to this conversation.

### POST /api/v1/attachments

`multipart/form-data`, one `file` part, max file size 20 MiB. Returns 201
`{"attachment":Attachment}`. Uploads are immutable; do not attach an artifact
from another scope. Uploaded but unsent files may remain available in Files.

Voice recordings use the same upload and message routes: `voice-<UUID>.m4a`,
`Content-Type: audio/mp4`, followed by POST messages with the returned attachment
ID. The API identifies the M4A audio container even when the generic HTTP sniffer
calls it `video/mp4`; malformed or video-containing data claimed as `audio/mp4`
returns `invalid_request`. Uploaded audio reaches the existing STT workflow
without a Telegram download. The STT decoder enforces the real five-minute
limit. Voice upload/POST retries preserve the same message UUID and uploaded ID.
Public history contains attachment metadata, with audio bytes available only
through authenticated content downloads.

Attachment:
`{"id":"…","name":"lake.jpg","mime":"image/jpeg","size_bytes":12345}`.

### GET /api/v1/attachments/{id}/content

Authenticated binary download with accurate Content-Type, attachment filename,
`X-Content-Type-Options: nosniff`, and private/no-store caching. IDs are URL path
escaped. Images are downloaded by the client and decoded locally.

## Deployment

Set `DESKTOP_API_TOKEN` in the server's `.env` to a random token of at least
32 bytes and recreate the `openalbert` service. Leaving it empty disables all
API routes. Existing Telegram settings determine the owner; the client cannot
choose a different user or conversation.

The API uses the existing HTTP listener on port 8080. For LAN access, bind the
Compose port to the Pi's LAN address, for example `192.168.1.31:8080:8080`, and
connect using `http://192.168.1.31:8080` with the bearer token. This publishes the
existing listener, including the inspector; only expose it to the intended LAN.
HTTPS can be provided by a reverse proxy when needed.

Database startup adds and backfills a public timeline index from the durable
inbox/outbox. No separate agent, queue or conversation is created. Deployments
use the repository's existing GitHub Actions / GHCR release process.
