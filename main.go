// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

func rootHandler(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "<html><body>Use /api/git/compare/&lt;from&gt;/&lt;to&gt;</body></html>")
}

type git struct {
	logger    *log.Logger
	root      string
	masterRef string
	delay     time.Duration

	lock       sync.RWMutex
	masterHash string
	parentHood map[string][]string
}

func (g *git) getRef(ref string) (string, error) {
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = g.root
	out, err := cmd.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	return "", errors.New("failed to run git rev-parse")
}

func (g *git) scanLoop() {
	for {
		time.Sleep(g.delay)
		if err := g.fetch(); err != nil {
			log.Fatal(err)
		}
	}
}

func (g *git) fetch() error {
	g.logger.Printf("Fetching")
	cmd := exec.Command("git", "fetch", "origin", "--prune", "--quiet")
	cmd.Dir = g.root
	start := time.Now()
	_, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New("failed to run git fetch")
	}
	newMaster, err := g.getRef(g.masterRef)
	if err != nil {
		return err
	}
	// Rescan only if fetched something.
	if newMaster != g.masterHash {
		g.logger.Printf("Fetched %s : %s in %s", g.masterRef, newMaster, time.Since(start))
		return g.scanParentHood(newMaster)
	}
	return nil
}

func (g *git) scanParentHood(newMaster string) error {
	g.logger.Printf("Rescanning")
	g.lock.Lock()
	g.masterHash = newMaster
	cmd := exec.Command("git", "rev-list", "--parents", g.masterHash)
	cmd.Dir = g.root
	start := time.Now()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	g.parentHood = map[string][]string{}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), " ")
		children := make([]string, len(parts)-1)
		copy(children, parts[1:])
		g.parentHood[parts[0]] = children
	}
	g.lock.Unlock()

	if err := cmd.Wait(); err != nil {
		return err
	}
	g.logger.Printf("Scanning found %d commits in %s", len(g.parentHood), time.Since(start))
	return nil
}

func isHashValid(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch c {
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', 'a', 'b', 'c', 'd', 'e', 'f':
		default:
			return false
		}
	}
	return true
}

func (g *git) compare(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 2 {
		http.Error(w, "Use ref1/ref2", 400)
		return
	}
	parent := strings.ToLower(parts[0])
	child := strings.ToLower(parts[1])
	if !isHashValid(parent) || !isHashValid(child) {
		http.Error(w, "Use valid commit hash", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	g.lock.RLock()
	defer g.lock.RUnlock()
	if _, ok := g.parentHood[parent]; !ok {
		_, _ = io.WriteString(w, "{\"ok\":false,\"ancestor\":false}")
		return
	}
	if g.isChild(parent, child) {
		_, _ = io.WriteString(w, "{\"ok\":true,\"ancestor\":true}")
		return
	}
	_, _ = io.WriteString(w, "{\"ok\":true,\"ancestor\":false}")
}

func (g *git) isChild(parent, child string) bool {
	for _, c := range g.parentHood[parent] {
		if c == child {
			return true
		}
		if g.isChild(c, child) {
			return true
		}
	}
	return false
}

func mainImpl() error {
	port := flag.Int("port", 8010, "port number")
	notLocal := flag.Bool("all", false, "listen on all IPs, not just localhost")
	cwd, _ := os.Getwd()
	root := flag.String("root", cwd, "checkout root dir")
	flag.Parse()
	logger := log.New(os.Stderr, "", log.Lmicroseconds|log.LUTC)

	g := git{
		root:       *root,
		logger:     logger,
		delay:      60 * time.Second,
		masterRef:  "origin/master",
		parentHood: map[string][]string{},
	}
	// Do a fetch on start up to ensure the repository is valid and the data is
	// initialized.
	if err := g.fetch(); err != nil {
		return err
	}
	go g.scanLoop()
	serveMux := http.NewServeMux()
	handle(serveMux, "/api/git/compare/", restrictFunc(g.compare, "GET"))
	serveMux.Handle("/", restrictFunc(rootHandler, "GET"))
	var addr string
	if *notLocal {
		addr = fmt.Sprintf(":%d", *port)
	} else {
		addr = fmt.Sprintf("localhost:%d", *port)
	}
	s := &http.Server{
		Addr:    addr,
		Handler: &loggingHandler{serveMux, logger},
	}
	ls, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	_, portStr, _ := net.SplitHostPort(ls.Addr().String())
	logger.Printf("Serving %s on port %s", g.root, portStr)
	return s.Serve(ls)
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		os.Exit(1)
	}
}
