package api

import "net/http"

// withCORS allows cross-origin calls from exactly one origin, exposes the
// list total header to it and answers preflight requests without reaching
// the router.
func withCORS(allowedOrigin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		originAllowed := allowedOrigin != "" && r.Header.Get("Origin") == allowedOrigin
		if originAllowed {
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			w.Header().Set("Access-Control-Expose-Headers", totalCountHeader)
		}

		if r.Method == http.MethodOptions {
			if originAllowed {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
