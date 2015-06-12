// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// pre-commit-go: runs pre-commit checks on Go projects.
//
// See https://github.com/maruel/pre-commit-go for more details.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/maruel/pre-commit-go/checks"
	"github.com/maruel/pre-commit-go/checks/definitions"
	"github.com/maruel/pre-commit-go/internal"
	"github.com/maruel/pre-commit-go/scm"
	"gopkg.in/yaml.v2"
)

// Globals

// Bump when the CLI, configuration file format or behavior change in any
// significant way. This will make files written by this version backward
// incompatible, forcing downstream users to update their pre-commit-go
// version.
const version = "0.4.4"

const hookContent = `#!/bin/sh
# AUTOGENERATED BY pre-commit-go.
#
# For more information, run:
#   pre-commit-go help
#
# or visit https://github.com/maruel/pre-commit-go

set -e
pre-commit-go run-hook %s
`

const gitNilCommit = "0000000000000000000000000000000000000000"

const helpModes = "Supported modes (with shortcut names):\n- pre-commit / fast / pc\n- pre-push / slow / pp  (default)\n- continous-integration / full / ci\n- lint\n- all: includes both continuous-integration and lint"

// http://git-scm.com/docs/githooks#_pre_push
var rePrePush = regexp.MustCompile("^(.+?) ([0-9a-f]{40}) (.+?) ([0-9a-f]{40})$")

var helpText = template.Must(template.New("help").Parse(`pre-commit-go: runs pre-commit checks on Go projects, fast.

Supported commands are:
  help        - this page
  prereq      - installs prerequisites, e.g.: errcheck, golint, goimports,
                govet, etc as applicable for the enabled checks
  info        - prints the current configuration used
  install     - runs 'prereq' then installs the git commit hook as
                .git/hooks/pre-commit
  installrun  - runs 'prereq', 'install' then 'run'
  run         - runs all enabled checks
  run-hook    - used by hooks (pre-commit, pre-push) exclusively
  version     - print the tool version number
  writeconfig - writes (or rewrite) a pre-commit-go.yml

When executed without command, it does the equivalent of 'installrun'.

Supported flags are:
{{.Usage}}
Supported checks:
  Native checks that only depends on the stdlib:{{range .NativeChecks}}
    - {{printf "%-*s" $.Max .GetName}} : {{.GetDescription}}{{end}}

  Checks that have prerequisites (which will be automatically installed):{{range .OtherChecks}}
    - {{printf "%-*s" $.Max .GetName}} : {{.GetDescription}}{{end}}

No check ever modify any file.
`))

const yamlHeader = `# https://github.com/maruel/pre-commit-go configuration file to run checks
# automatically on commit, on push and on continuous integration service after
# a push or on merge of a pull request.
#
# See https://godoc.org/github.com/maruel/pre-commit-go/checks for more
# information.

`

var parsedVersion []int

// Utils.

func init() {
	var err error
	parsedVersion, err = parseVersion(version)
	if err != nil {
		panic(err)
	}
}

// parseVersion converts a "1.2.3" string into []int{1,2,3}.
func parseVersion(v string) ([]int, error) {
	out := []int{}
	for _, i := range strings.Split(v, ".") {
		v, err := strconv.ParseInt(i, 10, 32)
		if err != nil {
			return nil, err
		}
		out = append(out, int(v))
	}
	return out, nil
}

// loadConfigFile returns a Config with defaults set then loads the config from
// file "pathname".
func loadConfigFile(pathname string) *checks.Config {
	content, err := ioutil.ReadFile(pathname)
	if err != nil {
		return nil
	}
	config := &checks.Config{}
	if err := yaml.Unmarshal(content, config); err != nil {
		// Log but ignore the error, recreate a new config instance.
		log.Printf("failed to parse %s: %s", pathname, err)
		return nil
	}
	configVersion, err := parseVersion(config.MinVersion)
	if err != nil {
		log.Printf("invalid version %s", config.MinVersion)
	}
	for i, v := range configVersion {
		if len(parsedVersion) <= i {
			if v == 0 {
				// 3.0 == 3.0.0
				continue
			}
			log.Printf("requires newer version %s", config.MinVersion)
			return nil
		}
		if parsedVersion[i] > v {
			break
		}
		if parsedVersion[i] < v {
			log.Printf("requires newer version %s", config.MinVersion)
			return nil
		}
	}
	return config
}

// loadConfig loads the on disk configuration or use the default configuration
// if none is found. See CONFIGURATION.md for the logic.
func loadConfig(repo scm.ReadOnlyRepo, path string) (string, *checks.Config) {
	if filepath.IsAbs(path) {
		if config := loadConfigFile(path); config != nil {
			return path, config
		}
	} else {
		// <repo root>/.git/<path>
		if scmDir, err := repo.ScmDir(); err == nil {
			file := filepath.Join(scmDir, path)
			if config := loadConfigFile(file); config != nil {
				return file, config
			}
		}

		// <repo root>/<path>
		file := filepath.Join(repo.Root(), path)
		if config := loadConfigFile(file); config != nil {
			return file, config
		}

		if user, err := user.Current(); err == nil && user.HomeDir != "" {
			if runtime.GOOS == "windows" {
				// ~/<path>
				file = filepath.Join(user.HomeDir, path)
			} else {
				// ~/.config/<path>
				file = filepath.Join(user.HomeDir, ".config", path)
			}
			if config := loadConfigFile(file); config != nil {
				return file, config
			}
		}
	}
	return "<N/A>", checks.New(version)
}

func callRun(check checks.Check, change scm.Change) (error, time.Duration) {
	if l, ok := check.(sync.Locker); ok {
		l.Lock()
		defer l.Unlock()
	}
	start := time.Now()
	err := check.Run(change)
	return err, time.Now().Sub(start)
}

func runChecks(config *checks.Config, change scm.Change, modes []checks.Mode) error {
	enabledChecks, maxDuration := config.EnabledChecks(modes)
	log.Printf("mode: %s; %d checks; %d max seconds allowed", modes, len(enabledChecks), maxDuration)
	var wg sync.WaitGroup
	errs := make(chan error, len(enabledChecks))
	start := time.Now()
	for _, c := range enabledChecks {
		wg.Add(1)
		go func(check checks.Check) {
			defer wg.Done()
			log.Printf("%s...", check.GetName())
			err, duration := callRun(check, change)
			suffix := ""
			if err != nil {
				suffix = " FAILED"
			}
			log.Printf("... %s in %1.2fs%s", check.GetName(), duration.Seconds(), suffix)
			if err != nil {
				errs <- err
			}
			// A check that took too long is a check that failed.
			if duration > time.Duration(maxDuration)*time.Second {
				errs <- fmt.Errorf("check %s took %1.2fs", check.GetName(), duration.Seconds())
			}
		}(c)
	}
	wg.Wait()

	var err error
	for {
		select {
		case err = <-errs:
			fmt.Printf("%s\n", err)
		default:
			if err != nil {
				duration := time.Now().Sub(start)
				return fmt.Errorf("checks failed in %1.2fs", duration.Seconds())
			}
			return err
		}
	}
}

func runPreCommit(repo scm.Repo, config *checks.Config) error {
	// First, stash index and work dir, keeping only the to-be-committed changes
	// in the working directory.
	stashed, err := repo.Stash()
	if err != nil {
		return err
	}
	// Run the checks.
	var change scm.Change
	change, err = repo.Between(scm.Current, repo.HEAD(), config.IgnorePatterns)
	if change != nil {
		err = runChecks(config, change, []checks.Mode{checks.PreCommit})
	}
	// If stashed is false, everything was in the index so no stashing was needed.
	if stashed {
		if err2 := repo.Restore(); err == nil {
			err = err2
		}
	}
	return err
}

func runPrePush(repo scm.Repo, config *checks.Config) (err error) {
	previous := repo.HEAD()
	// Will be "" if the current checkout was detached.
	previousRef := repo.Ref()
	curr := previous
	stashed := false
	defer func() {
		if curr != previous {
			p := previousRef
			if p == "" {
				p = string(previous)
			}
			if err2 := repo.Checkout(p); err == nil {
				err = err2
			}
		}
		if stashed {
			if err2 := repo.Restore(); err == nil {
				err = err2
			}
		}
	}()

	bio := bufio.NewReader(os.Stdin)
	line := ""
	triedToStash := false
	for {
		if line, err = bio.ReadString('\n'); err != nil {
			break
		}
		matches := rePrePush.FindStringSubmatch(line[:len(line)-1])
		if len(matches) != 5 {
			return fmt.Errorf("unexpected stdin for pre-push: %q", line)
		}
		from := scm.Commit(matches[4])
		to := scm.Commit(matches[2])
		if to == gitNilCommit {
			// It's being deleted.
			continue
		}
		if to != curr {
			// Stash, checkout, run tests.
			if !triedToStash {
				// Only try to stash once.
				triedToStash = true
				if stashed, err = repo.Stash(); err != nil {
					return
				}
			}
			curr = to
			if err = repo.Checkout(string(to)); err != nil {
				return
			}
		}
		if from == gitNilCommit {
			from = scm.GitInitialCommit
		}
		change, err := repo.Between(from, to, config.IgnorePatterns)
		if err != nil {
			return err
		}
		if err = runChecks(config, change, []checks.Mode{checks.PrePush}); err != nil {
			return err
		}
	}
	if err == io.EOF {
		err = nil
	}
	return
}

func processModes(modeFlag string) ([]checks.Mode, error) {
	if len(modeFlag) == 0 {
		return nil, nil
	}
	var modes []checks.Mode
	for _, p := range strings.Split(modeFlag, ",") {
		if len(p) != 0 {
			switch p {
			case "all":
				modes = append(modes, checks.ContinuousIntegration, checks.Lint)
			case string(checks.PreCommit), "fast", "pc":
				modes = append(modes, checks.PreCommit)
			case string(checks.PrePush), "slow", "pp":
				modes = append(modes, checks.PrePush)
			case string(checks.ContinuousIntegration), "full", "ci":
				modes = append(modes, checks.ContinuousIntegration)
			case string(checks.Lint):
				modes = append(modes, checks.Lint)
			default:
				return nil, fmt.Errorf("invalid mode \"%s\"\n\n%s", p, helpModes)
			}
		}
	}
	return modes, nil
}

// Commands.

func cmdHelp(repo scm.ReadOnlyRepo, config *checks.Config, usage string) error {
	s := &struct {
		Usage        string
		Max          int
		NativeChecks []checks.Check
		OtherChecks  []checks.Check
	}{
		usage,
		0,
		[]checks.Check{},
		[]checks.Check{},
	}
	for name, factory := range checks.KnownChecks {
		if v := len(name); v > s.Max {
			s.Max = v
		}
		c := factory()
		if len(c.GetPrerequisites()) == 0 {
			s.NativeChecks = append(s.NativeChecks, c)
		} else {
			s.OtherChecks = append(s.OtherChecks, c)
		}
	}
	return helpText.Execute(os.Stdout, s)
}

// cmdInfo displays the current configuration used.
func cmdInfo(repo scm.ReadOnlyRepo, config *checks.Config, modes []checks.Mode, file string) error {
	fmt.Printf("File: %s\n", file)
	fmt.Printf("Repo: %s\n", repo.Root())

	if len(modes) == 0 {
		modes = checks.AllModes
	}
	for _, mode := range modes {
		settings := config.Modes[mode]
		maxLen := 0
		for _, checks := range settings.Checks {
			for _, check := range checks {
				if l := len(check.GetName()); l > maxLen {
					maxLen = l
				}
			}
		}
		fmt.Printf("\n%s:\n  %-*s %d seconds\n", mode, maxLen+1, "Limit:", settings.MaxDuration)
		for _, checks := range settings.Checks {
			for _, check := range checks {
				name := check.GetName()
				fmt.Printf("  %s:%s %s\n", name, strings.Repeat(" ", maxLen-len(name)), check.GetDescription())
				content, err := yaml.Marshal(check)
				if err != nil {
					return err
				}
				options := strings.TrimSpace(string(content))
				if options == "{}" {
					// It means there's no options.
					options = "<no option>"
				}
				lines := strings.Join(strings.Split(options, "\n"), "\n    ")
				fmt.Printf("    %s\n", lines)
			}
		}
	}
	return nil
}

// cmdInstallPrereq installs all the packages needed to run the enabled checks.
func cmdInstallPrereq(repo scm.ReadOnlyRepo, config *checks.Config, modes []checks.Mode, noUpdate bool) error {
	var wg sync.WaitGroup
	enabledChecks, _ := config.EnabledChecks(modes)
	number := 0
	c := make(chan string, len(enabledChecks))
	for _, check := range enabledChecks {
		for _, p := range check.GetPrerequisites() {
			number++
			wg.Add(1)
			go func(prereq definitions.CheckPrerequisite) {
				defer wg.Done()
				if !prereq.IsPresent() {
					c <- prereq.URL
				}
			}(p)
		}
	}
	wg.Wait()
	log.Printf("Checked for %d prerequisites", number)
	loop := true
	// Use a map to remove duplicates.
	m := map[string]bool{}
	for loop {
		select {
		case url := <-c:
			m[url] = true
		default:
			loop = false
		}
	}
	urls := make([]string, 0, len(m))
	for url := range m {
		urls = append(urls, url)
	}
	sort.Strings(urls)
	if len(urls) != 0 {
		if noUpdate {
			out := "-n is specified but prerequites are missing:\n"
			for _, url := range urls {
				out += "  " + url + "\n"
			}
			return errors.New(out)
		}
		fmt.Printf("Installing:\n")
		for _, url := range urls {
			fmt.Printf("  %s\n", url)
		}

		out, _, err := internal.Capture("", nil, append([]string{"go", "get"}, urls...)...)
		if len(out) != 0 {
			return fmt.Errorf("prerequisites installation failed: %s", out)
		}
		if err != nil {
			return fmt.Errorf("prerequisites installation failed: %s", err)
		}
	}
	log.Printf("Prerequisites installation succeeded")
	return nil
}

// cmdInstall first calls cmdInstallPrereq() then install the .git/hooks/pre-commit hook.
//
// Silently ignore installing the hooks when running under a CI. In
// particular, circleci.com doesn't seem to create the directory .git/hooks.
func cmdInstall(repo scm.ReadOnlyRepo, config *checks.Config, modes []checks.Mode, noUpdate bool) (err error) {
	errCh := make(chan error)
	go func() {
		errCh <- cmdInstallPrereq(repo, config, modes, noUpdate)
	}()

	defer func() {
		if err2 := <-errCh; err == nil {
			err = err2
		}
	}()

	if checks.IsContinuousIntegration() {
		log.Printf("Running under CI; not installing hooks")
		return nil
	}
	log.Printf("Installing hooks")
	hookDir, err2 := repo.HookPath()
	if err2 != nil {
		return err2
	}
	for _, t := range []string{"pre-commit", "pre-push"} {
		// Always remove hook first if it exists, in case it's a symlink.
		p := filepath.Join(hookDir, t)
		_ = os.Remove(p)
		if err = ioutil.WriteFile(p, []byte(fmt.Sprintf(hookContent, t)), 0777); err != nil {
			return err
		}
	}
	log.Printf("Installation done")
	return nil
}

// cmdRun runs all the enabled checks.
func cmdRun(repo scm.ReadOnlyRepo, config *checks.Config, modes []checks.Mode, allFiles bool) error {
	old := scm.GitInitialCommit
	if !allFiles {
		var err error
		if old, err = repo.Upstream(); err != nil {
			return err
		}
	}
	change, err := repo.Between(scm.Current, old, config.IgnorePatterns)
	if err != nil {
		return err
	}
	return runChecks(config, change, modes)
}

// cmdRunHook runs the checks in a git repository.
//
// Use a precise "stash, run checks, unstash" to ensure that the check is
// properly run on the data in the index.
func cmdRunHook(repo scm.Repo, config *checks.Config, mode string, noUpdate bool) error {
	switch checks.Mode(mode) {
	case checks.PreCommit:
		return runPreCommit(repo, config)

	case checks.PrePush:
		return runPrePush(repo, config)

	case checks.ContinuousIntegration:
		// Always runs all tests on CI.
		change, err := repo.Between(scm.Current, scm.GitInitialCommit, config.IgnorePatterns)
		if err != nil {
			return err
		}
		mode := []checks.Mode{checks.ContinuousIntegration}

		// This is a special case, some users want reproducible builds and in this
		// case they do not want any external reference and want to enforce
		// noUpdate, but many people may not care (yet). So default to fetching but
		// it can be overriden.
		if err = cmdInstallPrereq(repo, config, mode, noUpdate); err != nil {
			return err
		}
		return runChecks(config, change, mode)

	default:
		return errors.New("unsupported hook type for run-hook")
	}
}

func cmdWriteConfig(repo scm.ReadOnlyRepo, config *checks.Config, configPath string) error {
	content, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("internal error when marshaling config: %s", err)
	}
	_ = os.Remove(configPath)
	return ioutil.WriteFile(configPath, append([]byte(yamlHeader), content...), 0666)
}

func mainImpl() error {

	if len(os.Args) == 1 {
		if checks.IsContinuousIntegration() {
			os.Args = append(os.Args, "run-hook", "continuous-integration")
		} else {
			os.Args = append(os.Args, "installrun")
		}
	}

	cmd := os.Args[1]
	copy(os.Args[1:], os.Args[2:])
	os.Args = os.Args[:len(os.Args)-1]

	verboseFlag := flag.Bool("v", checks.IsContinuousIntegration(), "enables verbose logging output")
	allFlag := flag.Bool("a", false, "runs checks as if all files had been modified")
	noUpdateFlag := flag.Bool("n", false, "disallow using go get even if a prerequisite is missing; bail out instead")
	configPathFlag := flag.String("c", "pre-commit-go.yml", "file name of the config to load")
	modeFlag := flag.String("m", "", "coma separated list of modes to process; default depends on the command")
	flag.Parse()

	log.SetFlags(log.Lmicroseconds)
	if !*verboseFlag {
		log.SetOutput(ioutil.Discard)
	}

	modes, err := processModes(*modeFlag)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repo, err := scm.GetRepo(cwd)
	if err != nil {
		return err
	}
	if err := os.Chdir(repo.Root()); err != nil {
		return err
	}

	file, config := loadConfig(repo, *configPathFlag)

	switch cmd {
	case "help", "-help", "-h":
		cmd = "help"
		if *allFlag != false {
			return fmt.Errorf("-a can't be used with %s", cmd)
		}
		if *noUpdateFlag != false {
			return fmt.Errorf("-n can't be used with %s", cmd)
		}
		if *configPathFlag != "pre-commit-go.yml" {
			return fmt.Errorf("-m can't be used with %s", cmd)
		}
		if *modeFlag != "" {
			return fmt.Errorf("-m can't be used with %s", cmd)
		}
		b := &bytes.Buffer{}
		flag.CommandLine.SetOutput(b)
		flag.CommandLine.PrintDefaults()
		return cmdHelp(repo, config, b.String())

	case "info":
		if *allFlag != false {
			return fmt.Errorf("-a can't be used with %s", cmd)
		}
		if *noUpdateFlag != false {
			return fmt.Errorf("-n can't be used with %s", cmd)
		}
		return cmdInfo(repo, config, modes, file)

	case "install", "i":
		cmd = "install"
		if *allFlag != false {
			return fmt.Errorf("-a can't be used with %s", cmd)
		}
		if len(modes) == 0 {
			modes = checks.AllModes
		}
		return cmdInstall(repo, config, modes, *noUpdateFlag)

	case "installrun":
		if len(modes) == 0 {
			modes = []checks.Mode{checks.PrePush}
		}
		if err := cmdInstall(repo, config, modes, *noUpdateFlag); err != nil {
			return err
		}
		// TODO(maruel): Start running all checks that do not have a prerequisite
		// before installation is completed.
		return cmdRun(repo, config, modes, *allFlag)

	case "prereq", "p":
		cmd = "prereq"
		if *allFlag != false {
			return fmt.Errorf("-a can't be used with %s", cmd)
		}
		if len(modes) == 0 {
			modes = checks.AllModes
		}
		return cmdInstallPrereq(repo, config, modes, *noUpdateFlag)

	case "run", "r":
		cmd = "run"
		if *noUpdateFlag != false {
			return fmt.Errorf("-n can't be used with %s", cmd)
		}
		if len(modes) == 0 {
			modes = []checks.Mode{checks.PrePush}
		}
		return cmdRun(repo, config, modes, *allFlag)

	case "run-hook":
		if modes != nil {
			return fmt.Errorf("-m can't be used with %s", cmd)
		}
		if *allFlag != false {
			return fmt.Errorf("-a can't be used with %s", cmd)
		}
		if flag.NArg() != 1 {
			return errors.New("run-hook is only meant to be used by hooks")
		}
		return cmdRunHook(repo, config, flag.Arg(0), *noUpdateFlag)

	case "version":
		if modes != nil {
			return fmt.Errorf("-m can't be used with %s", cmd)
		}
		if *allFlag != false {
			return fmt.Errorf("-a can't be used with %s", cmd)
		}
		if *noUpdateFlag != false {
			return fmt.Errorf("-n can't be used with %s", cmd)
		}
		fmt.Println(version)
		return nil

	case "writeconfig", "w":
		if modes != nil {
			return fmt.Errorf("-m can't be used with %s", cmd)
		}
		if *allFlag != false {
			return fmt.Errorf("-a can't be used with %s", cmd)
		}
		// Note that in that case, file is ignored and not overritten.
		return cmdWriteConfig(repo, config, *configPathFlag)

	default:
		return errors.New("unknown command, try 'help'")
	}
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "pre-commit-go: %s\n", err)
		os.Exit(1)
	}
}
