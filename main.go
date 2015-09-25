// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/facebookgo/grace/gracehttp"
	"github.com/maruel/circular"
	"github.com/maruel/interrupt"
)

type repositories struct {
	root  string
	delay time.Duration

	wg       sync.WaitGroup
	readLock sync.Mutex
	addLock  sync.Mutex
	repos    map[string]*git
}

func (r *repositories) init() error {
	// Find all the repositories, sync them all.
	var wg sync.WaitGroup
	err := filepath.Walk(r.root, func(path string, info os.FileInfo, err error) error {
		if strings.HasSuffix(path, "/config") {
			wg.Add(1)
			go func() {
				defer wg.Done()
				path = path[:len(path)-7]
				name := path[len(r.root)+1:]
				g := &git{
					root:    path,
					name:    name,
					delay:   r.delay,
					revList: map[string][]string{},
				}
				r.repos[name] = g
				// TODO(maruel): Do this asynchronously.
				if err := g.updateBranches(false); err != nil {
					log.Fatal(err)
				}
				r.wg.Add(1)
				go func() {
					defer r.wg.Done()
					g.scanLoop()
				}()
			}()
			return filepath.SkipDir
		}
		return nil
	})
	wg.Wait()
	return err
}

func (r *repositories) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// TODO(maruel): Split the model-view.
	if req.URL.Path == "/" {
		r.rootHandler(w, req)
	} else {
		r.flowHandler(w, req)
	}
}

var repoRootTmpl = template.Must(template.New("name").Parse(`<html><body>
<h1>Repositories</h1><ul>
{{range .}}<li><a href="{{.}}.git/">{{.}}</a></li>
{{end}}</ul>
</body></html>`))

func (r *repositories) rootHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "Invalid Method", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	r.readLock.Lock()
	defer r.readLock.Unlock()
	list := make([]string, 0, len(r.repos))
	for k := range r.repos {
		list = append(list, k)
	}
	sort.Strings(list)
	if err := repoRootTmpl.Execute(w, list); err != nil {
		log.Fatal(err)
	}
}

var (
	repoReStr = "^[a-z0-9./\\-]+$"
	repoRe    = regexp.MustCompile(repoReStr)
)

func (r *repositories) flowHandler(w http.ResponseWriter, req *http.Request) {
	// TODO(maruel): Rewrite this code.
	// The first part is the git URL.
	i := strings.Index(req.URL.Path, ".git")
	if i < 5 || i == -1 || (len(req.URL.Path) > i+4 && req.URL.Path[i+4] != '/') {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "Use .git to specify the git repository", 400)
		return
	}
	repo := req.URL.Path[1:i]
	req.URL.Path = req.URL.Path[i+4:]

	// Whitelist the characters allowed in repo URL section.
	if !repoRe.MatchString(repo) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, fmt.Sprintf("Repository URL %s must match \"%s\".", repo, repoReStr), 400)
		return
	}

	r.readLock.Lock()
	g, ok := r.repos[repo]
	r.readLock.Unlock()
	if !ok {
		// init is the special case to create one.
		if req.URL.Path != "/init" {
			u := "http://" + req.Host + "/" + repo + ".git/init"
			http.Error(w, fmt.Sprintf("Visit %s to initialize the repository.", u), 400)
		} else {
			// TODO(maruel): Enforce POST.
			r.addLock.Lock()
			defer r.addLock.Unlock()
			g := &git{
				root:    filepath.Join(r.root, repo),
				name:    repo,
				delay:   r.delay,
				revList: map[string][]string{},
			}
			if err := g.initialClone(w); err != nil {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				http.Error(w, "Use .git to specify the git repository", 400)
				return
			}
			r.repos[repo] = g
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				g.scanLoop()
			}()
		}
	} else {
		if req.URL.Path == "/init" {
			// Discard redondant init.
			req.URL.Path = "/"
		}
		if req.URL.Path == "" {
			req.URL.Path = "/"
		}
		g.ServeHTTP(w, req)
	}
}

type git struct {
	root  string
	name  string
	delay time.Duration

	lock     sync.RWMutex
	branches map[string]string
	revList  map[string][]string
}

func (g *git) initialClone(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	start := time.Now()
	repo := "https://" + g.name
	log.Printf("%s: git cloning %s", g.root, repo)
	cmd := exec.Command("git", "clone", "--mirror", "--progress", "--verbose", repo, g.root)
	w.WriteHeader(http.StatusOK)
	f := circular.AutoFlush(w, 500*time.Millisecond)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Run(); err != nil {
		log.Printf("%s: git clone failed: %s in %s", g.root, err, time.Since(start))
		return fmt.Errorf("git clone %s failed", repo)
	}
	log.Printf("%s: git clone in %s", g.root, time.Since(start))
	return g.updateBranches(true)
}

func (g *git) scanLoop() {
	for {
		select {
		case <-interrupt.Channel:
			break
		case <-time.After(g.delay):
			if err := g.fetch(); err != nil {
				log.Fatal(err)
			}
		}
	}
}

func (g *git) fetch() error {
	log.Printf("%s: Fetching", g.root)
	cmd := exec.Command("git", "fetch", "origin", "--prune", "--quiet")
	cmd.Dir = g.root
	start := time.Now()
	if err := cmd.Run(); err != nil {
		log.Printf("%s: fetch() failed: %s", g.root, err)
		return fmt.Errorf("git fetch %s failed", g.name)
	}
	log.Printf("%s: fetch() in %s", g.root, time.Since(start))
	return g.updateBranches(false)
}

func (g *git) updateBranches(locked bool) error {
	newRefs, err := getRefs(g.root)
	if err != nil {
		log.Printf("%s: getRefs() failed: %s", g.root, err)
		return fmt.Errorf("getting branches for %s failed", g.name)
	}
	// Rescan only if fetched something.
	if !eqMap(newRefs, g.branches) {
		log.Printf("%s: %d branches found", g.root, len(newRefs))
		start := time.Now()
		revList, err := scanParentHood(g.root, newRefs)
		if err != nil {
			log.Printf("%s: scanParentHood() failed: %s", g.root, err)
			return fmt.Errorf("scanning rev-list for %s failed", g.name)
		}
		log.Printf("%s: scanParentHood() found %d commits in %s", g.root, len(revList), time.Since(start))
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
	if r.Method != "GET" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "Invalid Method", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/" {
		g.rootHandler(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/compare/") {
		g.compareHandler(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.Error(w, "Unknown URL", 404)
	return
}

func (g *git) compareHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path[9:], "/")
	if len(parts) != 2 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "Use /compare/ref1/ref2", 400)
		return
	}
	p, _ := g.compare(parts[0], parts[1])
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(&p); err != nil {
		log.Fatal(err)
	}
}

type parentHood struct {
	ParentValid bool
	ChildValid  bool
	Ancestor    bool
}

func (g *git) compare(parent, child string) (*parentHood, error) {
	p := &parentHood{}
	parent = strings.ToLower(parent)
	child = strings.ToLower(child)
	if !isHashValid(parent) || !isHashValid(child) {
		return p, errors.New("use valid commit digests")
	}
	g.lock.RLock()
	defer g.lock.RUnlock()
	_, p.ParentValid = g.revList[parent]
	_, p.ParentValid = g.revList[child]
	p.Ancestor = g.isChild(parent, child)
	return p, nil
}

var gitRootTmpl = template.Must(template.New("name").Parse(`<html><body>
<h1>{{.Name}}</h1><ul>
{{range $index, $key := .Keys}}<li>{{$key}}: {{index $.Map $key}}</li>
{{end}}</ul>
<form method="GET" action="">
	<table>
		<tr>
			<td>Parent:</td>
			<td><input type="text" name="parent" maxlen=40 size=42 value="{{.Parent}}"></input></td>
		</tr>
		<tr>
			<td>Child:</td>
			<td><input type="text" name="child" maxlen=40 size=42 value="{{.Child}}"></input></td>
		</tr>
		<tr>
			<td></td>
			<td><input type="submit" value="Check"></input></td>
		</tr>
	</table>
</form>
{{.Extra}}<p>
<em>ps: there's a JSON API</em>
</body></html>`))

func (g *git) rootHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	values := r.URL.Query()
	parent := values.Get("parent")
	child := values.Get("child")
	p, err := g.compare(parent, child)
	extra := ""
	if err == nil {
		extra = fmt.Sprintf("Ancestor: %t", p.Ancestor)
	}
	g.lock.RLock()
	defer g.lock.RUnlock()
	list := make([]string, 0, len(g.branches))
	for k := range g.branches {
		list = append(list, k)
	}
	sort.Strings(list)
	t := &struct {
		Name   string
		Keys   []string
		Map    map[string]string
		Parent string
		Child  string
		Extra  string
	}{
		g.name,
		list,
		g.branches,
		parent,
		child,
		extra,
	}
	if err := gitRootTmpl.Execute(w, t); err != nil {
		log.Fatal(err)
	}
}

func mainImpl() error {
	port := flag.Int("port", 8010, "port number")
	notLocal := flag.Bool("all", false, "listen on all IPs, not just localhost")
	cwd, _ := os.Getwd()
	root := flag.String("root", filepath.Join(cwd, "repos"), "checkout root dir")
	flag.Parse()
	log.SetFlags(log.Lmicroseconds | log.LUTC)

	if err := os.MkdirAll(*root, 0700); err != nil {
		return err
	}
	repos := &repositories{
		root:  *root,
		delay: 60 * time.Second,
		repos: map[string]*git{},
	}

	// TODO(maruel): This is a race condition with the parent process.
	if err := repos.init(); err != nil {
		return err
	}

	var addr string
	if *notLocal {
		addr = fmt.Sprintf(":%d", *port)
	} else {
		addr = fmt.Sprintf("localhost:%d", *port)
	}

	mux := http.NewServeMux()
	mux.Handle("/favicon.ico", restrictFunc(func(w http.ResponseWriter, r *http.Request) {
		// TODO(maruel): Eh.
		http.Redirect(w, r, "https://git-scm.com/images/logos/downloads/Git-Icon-1788C.png", http.StatusFound)
	}, "GET"))
	mux.Handle("/", repos)

	s := &http.Server{
		Addr:    addr,
		Handler: exitOnPanic{&loggingHandler{mux, nil}},
	}

	// TODO(maruel): Handle Ctrl-C to quick shutdown but wait for git operations.
	err := gracehttp.Serve(s)
	interrupt.Set()
	repos.wg.Wait()
	return err
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		os.Exit(1)
	}
}
