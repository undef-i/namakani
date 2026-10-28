package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"namakani/internal/api"
	"namakani/internal/proton"
	"namakani/internal/store"
	syncmgr "namakani/internal/sync"
)

const (
	readHeaderTimeout = 20 * time.Second
	idleTimeout       = 90 * time.Second
)

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
	addr := flag.String("addr", "", "listen address (required)")
	dataDir := flag.String("data", "", "data directory (required)")
	flag.Parse()

	var missing []string
	if *addr == "" {
		missing = append(missing, "-addr")
	}
	if *dataDir == "" {
		missing = append(missing, "-data")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "missing required flag(s): %s\n", strings.Join(missing, ", "))
		os.Exit(2)
	}

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
