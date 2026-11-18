package desktopapi

import (
	"net/http"
)

func (a *API) chatGPTAvailable(w http.ResponseWriter) bool {
	if a.Billing == nil || a.Billing.Plan == nil {
		fail(w, 409, "disabled", "Enable OPENAI_AUTH_MODE=siwc or auto on the server to connect ChatGPT.")
		return false
	}
	return true
}
func (a *API) chatGPTStatus(w http.ResponseWriter, r *http.Request) {
	if a.Billing == nil {
		fail(w, 409, "disabled", "ChatGPT settings are unavailable on this server.")
		return
	}
	respond(w, 200, a.Billing.Status())
}
func (a *API) chatGPTBegin(w http.ResponseWriter, r *http.Request) {
	if !a.chatGPTAvailable(w) {
		return
	}
	link, err := a.Billing.Plan.Begin()
	if err != nil {
		fail(w, 409, "sign_in_pending", err.Error())
		return
	}
	respond(w, 200, map[string]string{"url": link})
}
func (a *API) chatGPTFinish(w http.ResponseWriter, r *http.Request) {
	if !a.chatGPTAvailable(w) {
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := a.Billing.Plan.Finish(r.Context(), body.URL); err != nil {
		fail(w, 400, "sign_in_failed", err.Error())
		return
	}
	a.Billing.Reconnected()
	respond(w, 200, a.Billing.Status())
}
func (a *API) chatGPTModels(w http.ResponseWriter, r *http.Request) {
	if !a.chatGPTAvailable(w) {
		return
	}
	models, err := a.Billing.Plan.Models(r.Context())
	if err != nil {
		fail(w, 503, "models_unavailable", err.Error())
		return
	}
	respond(w, 200, map[string]any{"models": models})
}
func (a *API) chatGPTModel(w http.ResponseWriter, r *http.Request) {
	if !a.chatGPTAvailable(w) {
		return
	}
	var body struct {
		Slug string `json:"slug"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := a.Billing.Plan.SetModel(r.Context(), body.Slug); err != nil {
		fail(w, 400, "invalid_model", err.Error())
		return
	}
	respond(w, 200, a.Billing.Status())
}
