package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	privatePath   = "/private.bin"
	readyPath     = "/ready"
	headerName    = "Authorization"
	headerPrefix  = "Bearer "
	redirectPath  = "/redirect"

	escapedHeaderName  = "X-Pveguest-Escaped"
	escapedHeaderValue = `a"b\c`
	processGlob   = "/proc/[0-9]*/cmdline"
	stateFileMode = 0o644
)

type serverState struct {
	Authorized int `json:"authorized"`
	Rejected   int `json:"rejected"`
	Scanned    int `json:"scanned"`
	Leaks      int `json:"leaks"`
}

type server struct {
	mutex     sync.Mutex
	state     serverState
	token     []byte
	content   []byte
	stateFile string
}

func (s *server) save() {
	document, err := json.Marshal(s.state)
	if err != nil {
		slog.Error("encode the state failed", "err", err)
		os.Exit(1)
	}
	temporary := s.stateFile + ".part"
	if err := os.WriteFile(temporary, document, stateFileMode); err != nil {
		slog.Error("write the state failed", "err", err)
		os.Exit(1)
	}
	if err := os.Rename(temporary, s.stateFile); err != nil {
		slog.Error("rename the state failed", "err", err)
		os.Exit(1)
	}
}

func (s *server) scanProcesses() (int, int) {
	commandLines, err := filepath.Glob(processGlob)
	if err != nil {
		slog.Error("list the processes failed", "err", err)
		os.Exit(1)
	}
	scanned := 0
	leaks := 0
	for _, commandLine := range commandLines {
		content, err := os.ReadFile(commandLine)
		if err != nil {
			continue
		}
		scanned++
		if bytes.Contains(content, s.token) {
			leaks++
		}
	}
	return scanned, leaks
}

func (s *server) private(writer http.ResponseWriter, request *http.Request) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if request.Header.Get(headerName) != headerPrefix+string(s.token) ||
		request.Header.Get(escapedHeaderName) != escapedHeaderValue {
		s.state.Rejected++
		s.save()
		http.Error(writer, "missing or wrong Authorization header", http.StatusUnauthorized)
		return
	}
	scanned, leaks := s.scanProcesses()
	s.state.Authorized++
	s.state.Scanned += scanned
	s.state.Leaks += leaks
	s.save()
	_, _ = writer.Write(s.content)
}

func readFile(path string) []byte {
	content, err := os.ReadFile(path)
	if err != nil {
		slog.Error("read a file failed", "path", path, "err", err)
		os.Exit(1)
	}
	return content
}

func main() {
	address := flag.String("addr", "", "")
	certFile := flag.String("cert", "", "")
	keyFile := flag.String("key", "", "")
	tokenFile := flag.String("token-file", "", "")
	contentFile := flag.String("content-file", "", "")
	stateFile := flag.String("state-file", "", "")
	flag.Parse()

	s := &server{
		token:     []byte(strings.TrimSpace(string(readFile(*tokenFile)))),
		content:   readFile(*contentFile),
		stateFile: *stateFile,
	}
	s.save()
	mux := http.NewServeMux()
	mux.HandleFunc(privatePath, s.private)
	mux.HandleFunc(redirectPath, func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, privatePath, http.StatusFound)
	})
	mux.HandleFunc(readyPath,func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	httpServer := &http.Server{Addr: *address, Handler: mux}
	if err := httpServer.ListenAndServeTLS(*certFile, *keyFile); err != nil {
		slog.Error("serve failed", "err", err)
		os.Exit(1)
	}
}
