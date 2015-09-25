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
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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

func getRefs(root string) (map[string]string, error) {
	cmd := exec.Command("git", "for-each-ref", "--format=%(refname)", "refs/heads/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.New("failed to run git for-each-ref")
	}
	refs := map[string]string{}
	for _, ref := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		refs[ref[len("refs/heads/"):]], err = getRef(root, ref)
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}

func getRef(root, ref string) (string, error) {
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	return "", fmt.Errorf("failed to run git rev-parse: %s", out)
}

func scanParentHood(root string, branches map[string]string) (map[string][]string, error) {
	revList := map[string][]string{}
	for ref := range branches {
		cmd := exec.Command("git", "rev-list", "--parents", ref)
		cmd.Dir = root
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			parts := strings.Split(scanner.Text(), " ")
			if len(parts) <= 1 {
				break
			}
			if _, ok := revList[parts[0]]; !ok {
				children := make([]string, len(parts)-1)
				copy(children, parts[1:])
				revList[parts[0]] = children
			}
		}
	}
	return revList, nil
}

func eqMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

type repositories struct {
	logger *log.Logger
	root   string
	delay  time.Duration

	lock  sync.Mutex
	repos map[string]*git
}

func (r *repositories) init() error {
	// Find all the repositories, sync them all.
	return filepath.Walk(r.root, func(path string, info os.FileInfo, err error) error {
		if strings.HasSuffix(path, "/config") {
			path = path[:len(path)-7]
			name := path[len(r.root)+1:]
			g := &git{
				logger:   r.logger,
				root:     path,
				name:     name,
				delay:    r.delay,
				branches: map[string]string{},
				revList:  map[string][]string{},
			}
			r.repos[name] = g
			if err := g.fetch(); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		return nil
	})
}

func (r *repositories) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/" {
		r.rootHandler(w, req)
	} else {
		r.flowHandler(w, req)
	}
}

func (r *repositories) rootHandler(w http.ResponseWriter, req *http.Request) {
	_, _ = io.WriteString(w, "<html><body>")
	_, _ = io.WriteString(w, "<h1>Repositories</h1><ul>")
	r.lock.Lock()
	defer r.lock.Unlock()
	for name := range r.repos {
		_, _ = fmt.Fprintf(w, "<li>%s</li>\n", name)
	}
	_, _ = io.WriteString(w, "</ul>To add a repo, visit /&lt;repo.git&gt;/init.</body></html>")
}

func (r *repositories) flowHandler(w http.ResponseWriter, req *http.Request) {
	// The first part is the git URL.
	i := strings.Index(req.URL.Path, ".git")
	if i < 5 || i == -1 || (len(req.URL.Path) > i+4 && req.URL.Path[i+4] != '/') {
		http.Error(w, "Use .git to specify the git repository", 400)
		return
	}
	repo := req.URL.Path[1:i]
	req.URL.Path = req.URL.Path[i+4:]

	r.lock.Lock()
	g, ok := r.repos[repo]
	if !ok {
		// init is the special case to create one.
		if req.URL.Path != "/init" {
			r.lock.Unlock()
			u := "http://" + req.Host + "/" + repo + ".git/init"
			http.Error(w, fmt.Sprintf("Visit %s to initialize the repository.", u), 400)
		} else {
			g := &git{
				logger:   r.logger,
				root:     filepath.Join(r.root, repo),
				name:     repo,
				delay:    r.delay,
				branches: map[string]string{},
				revList:  map[string][]string{},
			}
			g.lock.Lock()
			defer g.lock.Unlock()
			r.repos[repo] = g
			r.lock.Unlock()
			g.init(w)
		}
	} else {
		r.lock.Unlock()
		if req.URL.Path == "/init" {
			http.Error(w, "The repository is already initialized.", 400)
		} else {
			if req.URL.Path == "" {
				req.URL.Path = "/"
			}
			g.ServeHTTP(w, req)
		}
	}
}

type git struct {
	logger *log.Logger
	root   string
	name   string
	delay  time.Duration

	lock     sync.RWMutex
	branches map[string]string
	revList  map[string][]string
}

func (g *git) init(w http.ResponseWriter) {
	start := time.Now()
	repo := "https://" + g.name
	g.logger.Printf("Cloning %s into %s", repo, g.root)
	cmd := exec.Command("git", "clone", "--mirror", "--verbose", repo, g.root)
	w.WriteHeader(http.StatusOK)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		g.logger.Printf("Cloned %s: %s in %s", repo, err, time.Since(start))
	}
	g.logger.Printf("Cloned %s in %s", repo, time.Since(start))

	if err := g.updateBranches(true); err != nil {
		g.logger.Printf("Failed to get branches %s: %s", g.root, err)
	}
	go g.scanLoop()
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
	g.logger.Printf("Fetching %s", g.root)
	cmd := exec.Command("git", "fetch", "origin", "--prune", "--quiet")
	cmd.Dir = g.root
	start := time.Now()
	if err := cmd.Run(); err != nil {
		return errors.New("failed to run git fetch")
	}
	g.logger.Printf("Fetched %s in %s", g.root, time.Since(start))
	return g.updateBranches(false)
}

func (g *git) updateBranches(locked bool) error {
	newRefs, err := getRefs(g.root)
	if err != nil {
		return err
	}
	// Rescan only if fetched something.
	if !eqMap(newRefs, g.branches) {
		g.logger.Printf("Branches %s: %d branches found", g.root, len(newRefs))
		start := time.Now()
		revList, err := scanParentHood(g.root, newRefs)
		if err != nil {
			g.logger.Printf("Scanning failed: %s in %s", err, time.Since(start))
			return err
		}
		g.logger.Printf("Scanning found %d commits in %s", len(revList), time.Since(start))
		if !locked {
			g.lock.Lock()
			defer g.lock.Unlock()
		}
		g.branches = newRefs
		g.revList = revList
	}
	return nil
}

func (g *git) isChild(parent, child string) bool {
	for _, c := range g.revList[parent] {
		if c == child {
			return true
		}
		if g.isChild(c, child) {
			return true
		}
	}
	return false
}

func (g *git) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		g.rootHandler(w, r)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/compare/") {
		http.Error(w, "Use /compare/ref1/ref2", 400)
		return
	}
	parts := strings.Split(r.URL.Path[9:], "/")
	if len(parts) != 2 {
		http.Error(w, "Use /compare/ref1/ref2", 400)
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
	if _, ok := g.revList[parent]; !ok {
		_, _ = io.WriteString(w, "{\"ok\":false,\"ancestor\":false}")
		return
	}
	if g.isChild(parent, child) {
		_, _ = io.WriteString(w, "{\"ok\":true,\"ancestor\":true}")
		return
	}
	_, _ = io.WriteString(w, "{\"ok\":true,\"ancestor\":false}")
}

func (g *git) rootHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "<html><body>")
	_, _ = fmt.Fprintf(w, "<h1>%s</h1><ul>", g.name)
	for name, h := range g.branches {
		_, _ = fmt.Fprintf(w, "<li>%s: %s</li>", name, h)
	}
	_, _ = io.WriteString(w, "</ul></body></html>")
}

func mainImpl() error {
	port := flag.Int("port", 8010, "port number")
	notLocal := flag.Bool("all", false, "listen on all IPs, not just localhost")
	cwd, _ := os.Getwd()
	root := flag.String("root", filepath.Join(cwd, "repos"), "checkout root dir")
	flag.Parse()
	logger := log.New(os.Stderr, "", log.Lmicroseconds|log.LUTC)

	if err := os.MkdirAll(*root, 0700); err != nil {
		return err
	}
	repos := &repositories{
		root:   *root,
		delay:  60 * time.Second,
		logger: logger,
		repos:  map[string]*git{},
	}
	if err := repos.init(); err != nil {
		return err
	}

	serveMux := http.NewServeMux()
	serveMux.Handle("/", restrict(repos, "GET"))
	var addr string
	if *notLocal {
		addr = fmt.Sprintf(":%d", *port)
	} else {
		addr = fmt.Sprintf("localhost:%d", *port)
	}
	s := &http.Server{
		Addr:    addr,
		Handler: &loggingHandler{serveMux, logger},
		//Handler: exitOnPanic{&loggingHandler{serveMux, logger}},
	}
	ls, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	_, portStr, _ := net.SplitHostPort(ls.Addr().String())
	logger.Printf("Serving %s on port %s", repos.root, portStr)
	return s.Serve(ls)
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		os.Exit(1)
	}
}
