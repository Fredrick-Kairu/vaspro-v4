package api

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
)

// preferencesCSP allows the page its own script, stylesheet and API
// calls, and nothing else, as payments' checkout page (task 4.8).
const preferencesCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'"

//go:embed preferences/page.html preferences/preferences.js preferences/preferences.css
var preferenceFiles embed.FS

var preferencePage = template.Must(template.ParseFS(preferenceFiles, "preferences/page.html"))

// channelLabels name the page's channels.
var channelLabels = map[string]string{"sms": "Promotional SMS", "whatsapp": "Promotional WhatsApp messages"}

type preferenceChannel struct {
	Channel, Label string
	Receiving      bool
}

type preferencePageData struct {
	Token, Problem, MaskedPhone string
	Channels                    []preferenceChannel
}

// mountPreferencePage serves the preference page (/p/{token}, data model
// §10) and its assets, outside the API: no session, no Idempotency-Key.
func mountPreferencePage(r chi.Router, a *app.Service) {
	asset := func(name, contentType string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			b, _ := preferenceFiles.ReadFile("preferences/" + name)
			pageHeaders(w)
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(b)
		}
	}
	r.Get("/p/static/preferences.js", asset("preferences.js", "text/javascript; charset=utf-8"))
	r.Get("/p/static/preferences.css", asset("preferences.css", "text/css; charset=utf-8"))
	r.Get("/p/{token}", func(w http.ResponseWriter, req *http.Request) {
		token := chi.URLParam(req, "token")
		data, status := preferencePageData{Token: token}, http.StatusOK
		p, err := a.GetPreferences(req.Context(), token)
		switch {
		case errors.Is(err, app.ErrNotFound):
			data.Problem, status = "This link isn't valid or has expired.", http.StatusNotFound
		case err != nil:
			slog.ErrorContext(req.Context(), "contacts: rendering a preference page", "error", err)
			data.Problem, status = "Something went wrong. Try again later.", http.StatusInternalServerError
		default:
			data.MaskedPhone = p.MaskedPhone
			for _, c := range p.Channels {
				data.Channels = append(data.Channels, preferenceChannel{Channel: c.Channel, Label: channelLabels[c.Channel], Receiving: c.Receiving})
			}
		}
		pageHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = preferencePage.Execute(w, data)
	})
}

func pageHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", preferencesCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
}
