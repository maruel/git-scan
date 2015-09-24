// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"log"
	"net/http"
)

// loggingHandler converts an handler to log every HTTP request.
type loggingHandler struct {
	handler http.Handler
	log     *log.Logger
}

type loggingResponseWriter struct {
	http.ResponseWriter
	length int
	status int
}

func (l *loggingResponseWriter) Write(data []byte) (size int, err error) {
	size, err = l.ResponseWriter.Write(data)
	l.length += size
	return
}

func (l *loggingResponseWriter) WriteHeader(status int) {
	l.ResponseWriter.WriteHeader(status)
	l.status = status
}

func (l *loggingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lW := &loggingResponseWriter{ResponseWriter: w}
	l.handler.ServeHTTP(lW, r)
	l.log.Printf("%s - %3d %6db %4s %s",
		r.RemoteAddr,
		lW.status,
		lW.length,
		r.Method,
		r.RequestURI)
}

// restrictedHandler converts an handler to restrict methods to a subset.
type restrictedHandler struct {
	http.Handler
	methods []string
}

func (d restrictedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, method := range d.methods {
		if r.Method == method {
			d.Handler.ServeHTTP(w, r)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.Error(w, "Invalid Method", http.StatusMethodNotAllowed)
	return
}

func restrict(h http.Handler, m ...string) http.Handler {
	return restrictedHandler{h, m}
}

func restrictFunc(h http.HandlerFunc, m ...string) http.Handler {
	return restrictedHandler{h, m}
}

func handle(m *http.ServeMux, prefix string, h http.Handler) {
	m.Handle(prefix, http.StripPrefix(prefix, h))
}
