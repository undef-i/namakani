package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"namakani/internal/api"
	"namakani/internal/proton"
	"namakani/internal/store"
	syncmgr "namakani/internal/sync"
)

const (
	// readHeaderTimeout bounds how long a client may take to send its
	// request headers. It reuses the codebase's existing header-wait
	// budget (tuta rest.go responseHeaderTimeout, 20s) and stays far
	// below the 100s Cloudflare origin timeout on the public path.
	readHeaderTimeout = 20 * time.Second
	// idleTimeout bounds how long an idle keep-alive connection is held
	// between requests. It matches net/http's DefaultTransport
	// IdleConnTimeout (90s) and likewise stays below the 100s
	// Cloudflare origin timeout. It only applies between requests, so
	// it never touches an in-flight SSE stream or upload.
	idleTimeout = 90 * time.Second
)

// newHTTPServer deliberately leaves ReadTimeout and WriteTimeout unset:
// the app serves long-lived SSE streams (/api/events) and accepts mail
// with up to max_attachment_mb of attachments, both of which a whole-
// request deadline would cut off.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

//go:embed all:web/dist
var uiEmbed embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	dataDir := flag.String("data", "./data", "data directory (sqlite db lives here)")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	proton.Init(*dataDir)
	st, err := store.Open(filepath.Join(*dataDir, "namakani.db"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()

	uiFS, err := fs.Sub(uiEmbed, "web/dist")
	if err != nil {
		log.Fatalf("ui fs: %v", err)
	}

	mgr := syncmgr.NewManager(st)
	srv := api.New(st, uiFS, mgr)
	if mgr.SnippetsCurrent() {
		log.Printf("snippet recompute: skipped, algorithm version current")
	} else {
		go mgr.RecomputeSnippets()
	}
	log.Printf("namakani listening on %s (data: %s)", *addr, *dataDir)
	log.Fatal(newHTTPServer(*addr, srv.Handler()).ListenAndServe())
}
