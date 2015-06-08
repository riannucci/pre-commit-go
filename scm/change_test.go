// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package scm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/maruel/ut"
)

func TestInternalCheck(t *testing.T) {
	t.Parallel()
	d, err := os.Getwd()
	ut.AssertEqual(t, nil, err)
	ut.AssertEqual(t, "scm", filepath.Base(d))
}

func TestIsMainPackage(t *testing.T) {
	t.Parallel()
	data := []struct {
		expected string
		in       string
	}{
		{"foo", "// Hi\npackage foo\n"},
		{"main", "package main\n"},
		{"", ""},
	}
	for i, line := range data {
		ut.AssertEqualIndex(t, i, line.expected, getPackageName([]byte(line.in)))
	}
}

func TestGoDirs(t *testing.T) {
	scmDir, err := os.Getwd()
	ut.AssertEqual(t, nil, err)
	repo, err := GetRepo(scmDir)
	ut.AssertEqual(t, nil, err)
	change := repo.All().(*change)
	preCommitGoDir := filepath.Dir(scmDir)
	defer func() {
		_ = os.Chdir(scmDir)
	}()
	checksDir := filepath.Join(preCommitGoDir, "checks")
	definitionsDir := filepath.Join(checksDir, "definitions")
	internalDir := filepath.Join(preCommitGoDir, "internal")
	customCheckDir := filepath.Join(preCommitGoDir, "samples", "sample-pre-commit-go-custom-check")
	ut.AssertEqual(t, nil, os.Chdir(preCommitGoDir))
	ut.AssertEqual(t, []string{preCommitGoDir, checksDir, definitionsDir, internalDir, customCheckDir, scmDir}, change.goDirs(sourceDirs))
	ut.AssertEqual(t, []string{preCommitGoDir, checksDir, scmDir}, change.goDirs(testDirs))
	ut.AssertEqual(t, []string{checksDir, definitionsDir, internalDir, scmDir}, change.goDirs(packageDirs))
}