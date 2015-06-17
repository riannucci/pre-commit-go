// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// covg: yet another coverage tool.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	"github.com/maruel/pre-commit-go/checks"
	"github.com/maruel/pre-commit-go/checks/definitions"
	"github.com/maruel/pre-commit-go/scm"
)

// silentError means that the process exit code must be 1.
var silentError = errors.New("silent error")

// printProfile prints the results to stdout and returns false if the process
// exit code must be 1.
func printProfile(settings *definitions.CoverageSettings, profile checks.CoverageProfile, indent string) bool {
	out, err := checks.ProcessProfile(profile, settings)
	if indent != "" {
		tmp := ""
		for _, line := range strings.SplitAfter(out, "\n") {
			if len(line) > 1 {
				tmp += indent + line
			} else {
				tmp += line
			}
		}
		out = tmp
	}
	fmt.Printf("%s", out)
	if err != nil {
		fmt.Printf("%s%s", indent, err)
		return false
	}
	return true
}

func mainImpl() error {
	// TODO(maruel): Add support to use the same diff as pre-commit-go.
	minFlag := flag.Float64("min", 0, "minimum expected coverage in %")
	maxFlag := flag.Float64("max", 100, "maximum expected coverage in %")
	globalFlag := flag.Bool("g", false, "use global coverage")
	verboseFlag := flag.Bool("v", false, "enable logging")
	flag.Parse()

	log.SetFlags(log.Lmicroseconds)
	if !*verboseFlag {
		log.SetOutput(ioutil.Discard)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repo, err := scm.GetRepo(cwd, "")
	if err != nil {
		return err
	}

	c := checks.Coverage{
		Global: definitions.CoverageSettings{
			MinCoverage: *minFlag,
			MaxCoverage: *maxFlag,
		},
		PerDirDefault: definitions.CoverageSettings{
			MinCoverage: *minFlag,
			MaxCoverage: *maxFlag,
		},
	}

	// TODO(maruel): Run tests ala pre-commit-go; e.g. determine what diff to use.
	// TODO(maruel): Run only tests down the current directory when
	// *globalFlag == false.
	change, err := repo.Between(scm.Current, scm.GitInitialCommit, nil)
	if err != nil {
		return err
	}
	log.Printf("Packages: %s\n", change.All().TestPackages())
	profile, err := c.RunProfile(change)
	if err != nil {
		return err
	}

	if *globalFlag {
		if !printProfile(&c.Global, profile, "") {
			return silentError
		}
	} else {
		for _, pkg := range change.All().TestPackages() {
			d := pkgToDir(pkg)
			subset := profile.Subset(d)
			if len(subset) != 0 {
				fmt.Printf("%s\n", d)
				if !printProfile(&c.Global, subset, "  ") {
					err = silentError
				}
			} else {
				log.Printf("%s is empty", pkg)
			}
		}
	}
	return nil
}

func main() {
	if err := mainImpl(); err != nil {
		if err != silentError {
			fmt.Fprintf(os.Stderr, "covg: %s\n", err)
		}
		os.Exit(1)
	}
}

func pkgToDir(p string) string {
	if p == "." {
		return p
	}
	return p[2:]
}
