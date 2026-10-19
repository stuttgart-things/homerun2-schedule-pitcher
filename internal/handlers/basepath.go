package handlers

import "net/http"

// Mount serves app under basePath (e.g. "/schedule"), the way homerun2-portal
// forwards it: the request path arrives unchanged, with the prefix. Everything
// else stays at the root as well -- the API and POST /findings for agents and
// scripts, /health, /ready and /metrics -- and the UI's old root paths
// (/ui/...) redirect under the prefix. An empty basePath returns app as is.
func Mount(basePath string, app http.Handler) http.Handler {
	if basePath == "" {
		return app
	}
	mux := http.NewServeMux()
	mux.Handle(basePath+"/", http.StripPrefix(basePath, app))
	mux.Handle(basePath, http.RedirectHandler(basePath+"/", http.StatusTemporaryRedirect))
	if basePath != "/ui" {
		mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, basePath+r.URL.RequestURI(), http.StatusTemporaryRedirect)
		})
	}
	mux.Handle("/", app)
	return mux
}
