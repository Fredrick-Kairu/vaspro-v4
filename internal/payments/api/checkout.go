package api

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
)

// checkoutCSP allows the page its own script, stylesheet and API
// calls, and nothing else (task 4.8).
const checkoutCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'"

//go:embed checkout/page.html checkout/checkout.js checkout/checkout.css
var checkoutFiles embed.FS

var checkoutPage = template.Must(template.ParseFS(checkoutFiles, "checkout/page.html"))

type checkoutPageData struct {
	Token            string
	Status           string
	Problem          string
	Payee            string
	AmountText       string
	AccountReference string
}

// mountCheckoutPage serves the hosted checkout page (/pay/{token}) and
// its assets, outside the API: no session, no Idempotency-Key.
func mountCheckoutPage(r chi.Router, a *app.Service) {
	asset := func(name, contentType string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			b, _ := checkoutFiles.ReadFile("checkout/" + name)
			securityHeaders(w)
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(b)
		}
	}
	r.Get("/pay/static/checkout.js", asset("checkout.js", "text/javascript; charset=utf-8"))
	r.Get("/pay/static/checkout.css", asset("checkout.css", "text/css; charset=utf-8"))
	r.Get("/pay/{token}", func(w http.ResponseWriter, req *http.Request) {
		token := chi.URLParam(req, "token")
		data, status := checkoutPageData{Token: token}, http.StatusOK
		v, err := a.GetCheckout(req.Context(), token)
		switch {
		case errors.Is(err, app.ErrNotFound):
			data.Problem, status = "This payment link isn't valid.", http.StatusNotFound
		case errors.Is(err, app.ErrTopupUnavailable):
			data.Problem, status = "Payments are unavailable right now. Try again later.", http.StatusServiceUnavailable
		case err != nil:
			slog.ErrorContext(req.Context(), "payments: rendering a checkout page", "error", err)
			data.Problem, status = "Something went wrong. Try again later.", http.StatusInternalServerError
		default:
			data.Status, data.Payee, data.AmountText, data.AccountReference = v.Status, v.Payee, v.AmountText, v.AccountReference
		}
		securityHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = checkoutPage.Execute(w, data)
	})
}

func securityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", checkoutCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
}
