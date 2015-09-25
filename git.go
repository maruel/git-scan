// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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
