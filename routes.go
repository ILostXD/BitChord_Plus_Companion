package main

import "net/http"

// Route names are reserved; handlers do not read bodies, call upstreams, or change state.
var stubRoutes = []string{
	"POST /api/v1/library/scan",
}

func registerREST(mux *http.ServeMux) {
	for _, pattern := range stubRoutes {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotImplemented, map[string]string{
				"error": "not_implemented", "message": "Reserved for a later phase",
				"route": pattern,
			})
		})
	}
}
