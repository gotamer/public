// Copyright 2017 github.com/ucirello
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package runner holds the building blocks for cmd runner.
package runner // import "cirello.io/runner/runner"

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	supervisor "cirello.io/supervisor/easy"
)

// RestartMode defines if a process should restart itself.
type RestartMode string

// ParseRestartMode takes a string and converts to RestartMode. If the parsing
// fails, it silently defaults to Never.
func ParseRestartMode(m string) RestartMode {
	switch strings.ToLower(m) {
	case "yes", "always", "true", "1":
		return Always
	case "fail", "failure", "onfail", "onfailure", "on-failure", "on_failure":
		return OnFailure
	default:
		return Never
	}
}

// Restart modes
const (
	Always    RestartMode = "yes"
	OnFailure RestartMode = "fail"
	Never     RestartMode = ""
)

// ProcessType is the piece of software you want to start. Cmd accepts multiple
// commands. All commands are executed in order of declaration. The last command
// is considered the call which activates the process type. If WaitBefore is
// defined, it will wait for network readiness on the defined target before
// executing the first command. If WaitFor is defined, it will wait for network
// readiness on the defined target before executing the last command. Process
// types named as "build" are special, they are executed first in preparation
// for all other process types, upon their completion the application
// initialized.
type ProcessType struct {
	// Name of the process type
	Name string `json:"name"`

	// Cmd are the commands necessary to start the process type. Each
	// command is executed on its own separated shell. No state is shared
	// across commands.
	Cmd []string `json:"cmd"`

	// WaitBefore is the network address that the process type waits to be
	// available before initiating the process type start.
	WaitBefore string `json:"waitbefore,omitempty"`

	// WaitFor is the network address that the process type waits to be
	// available before finalizing the start.
	WaitFor string `json:"waitfor,omitempty"`

	// Restart is the flag that forces the process type to restart. It means
	// that all steps are executed upon restart. This option does not apply
	// to build steps.
	//
	// - yes|always: alway restart the process type.
	// - no|<empty>: never restart the process type.
	// - on-failure|fail: restart the process type if any of the steps fail.
	Restart RestartMode `json:"restart,omitempty"`

	// Group defines to which supervisor group this process type belongs.
	// Group is useful to contain restart to a subset of the process types.
	Group string
}

// Runner defines how this application should be started.
type Runner struct {
	// WorkDir is the working directory from which all commands are going
	// to be executed.
	WorkDir string `json:"workdir,omitempty"`

	// Observables are the filepath.Match() patterns used to scan for files
	// with changes.
	Observables []string `json:"observables,omitempty"`

	// SkipDirs are the directory names that are ignored during changed file
	// scanning.
	SkipDirs []string `json:"skipdir,omitempty"`

	// Processes is the list of processes necessary to start this
	// application.
	Processes []*ProcessType `json:"procs"`

	// BasePort is the IP port number used to calculate an IP port for each
	// process type and set to its $PORT environment variable. Build
	// processes do not earn an IP port.
	BasePort int

	// Formation allows to start more than one process type each time. Each
	// start will yield its own exclusive $PORT. Formation does not apply
	// to build process types.
	Formation map[string]int // map of process type name and count

	// BaseEnvironment is the set of environment variables loaded into
	// the service.
	BaseEnvironment []string

	longestProcessTypeName int

	// ServiceDiscoveryAddr is the net.Listen address used to bind the
	// service discovery service. Set to empty to disable it. If activated
	// this address is passed to the processes through the environment
	// variable named "DISCOVERY".
	ServiceDiscoveryAddr string

	sdMu             sync.Mutex
	serviceDiscovery map[string]int
}

// New creates a new runner ready to use.
func New() Runner {
	return Runner{
		Formation:        make(map[string]int),
		serviceDiscovery: make(map[string]int),
	}
}

// Start initiates the application.
func (r *Runner) Start(ctx context.Context) error {
	for _, proc := range r.Processes {
		name := proc.Name
		if formation, ok := r.Formation[proc.Name]; ok {
			name = fmt.Sprintf("%v.%v", proc.Name, formation)
		} else {
			name = fmt.Sprintf("%v.%v", proc.Name, 0)
		}

		if l := len(name); l > r.longestProcessTypeName {
			r.longestProcessTypeName = l
		}
	}
	r.longestProcessTypeName++

	go r.serveServiceDiscovery(ctx)

	updates, err := r.monitorWorkDir(ctx)
	if err != nil {
		return err
	}

	for {
		c, cancel := context.WithCancel(ctx)
		go r.startProcesses(c)
		select {
		case <-ctx.Done():
			cancel()
			return nil
		case <-updates:
			cancel()
		}
	}
}

func (r *Runner) startProcesses(ctx context.Context) {
	if ok := r.runBuilds(ctx); !ok {
		log.Println("error during build, halted")
		return
	}

	r.runNonBuilds(ctx)
}

func (r *Runner) runBuilds(ctx context.Context) bool {
	var (
		wgBuild sync.WaitGroup
		mu      sync.Mutex
		ok      = true
	)
	for _, sv := range r.Processes {
		if !strings.HasPrefix(sv.Name, "build") {
			continue
		}
		wgBuild.Add(1)
		go func(sv *ProcessType) {
			defer wgBuild.Done()
			if !r.startProcess(ctx, sv, -1, -1) {
				mu.Lock()
				ok = false
				mu.Unlock()
			}
		}(sv)
	}
	wgBuild.Wait()
	return ok
}

func (r *Runner) runNonBuilds(ctx context.Context) {
	var portCount int
	ctx = supervisor.WithContext(ctx)
	groups := make(map[string]context.Context)

	for _, sv := range r.Processes {
		if strings.HasPrefix(sv.Name, "build") {
			continue
		}

		maxProc := 1
		if formation, ok := r.Formation[sv.Name]; ok {
			maxProc = formation
		}

		procCtx := ctx
		if sv.Group != "" {
			groupCtx, ok := groups[sv.Group]
			if !ok {
				groupCtx = supervisor.WithContext(ctx)
				groups[sv.Group] = groupCtx
			}
			procCtx = groupCtx
		}

		for i := 0; i < maxProc; i++ {
			sv, i, pc := sv, i, portCount

			opt := supervisor.Temporary
			switch sv.Restart {
			case Always:
				opt = supervisor.Permanent
			case OnFailure:
				opt = supervisor.Transient
			}
			supervisor.Add(procCtx, func(ctx context.Context) {
				ok := r.startProcess(ctx, sv, i, pc)
				if !ok && sv.Restart == OnFailure {
					panic("restarting on failure")
				}
			}, opt)
			portCount++
		}
	}

	<-ctx.Done()
}

func (r *Runner) startProcess(ctx context.Context, sv *ProcessType, procCount, portCount int) bool {
	pr, pw := io.Pipe()
	procName := sv.Name
	port := r.BasePort + portCount
	if procCount > -1 {
		procName = fmt.Sprintf("%v.%v", procName, procCount)
	}
	if portCount > -1 {
		r.sdMu.Lock()
		r.serviceDiscovery[procName] = port
		r.sdMu.Unlock()
		defer func() {
			r.sdMu.Lock()
			delete(r.serviceDiscovery, procName)
			r.sdMu.Unlock()
		}()
	}
	r.prefixedPrinter(ctx, pr, procName)

	defer pw.Close()
	defer pr.Close()

	for idx, cmd := range sv.Cmd {
		fmt.Fprintln(pw, "running", `"`+cmd+`"`)
		if portCount > -1 {
			fmt.Fprintln(pw, "listening on", port)
		}
		fmt.Fprintln(pw)
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		c.Dir = r.WorkDir

		c.Env = os.Environ()
		if len(r.BaseEnvironment) > 0 {
			c.Env = r.BaseEnvironment
		}
		c.Env = append(c.Env, fmt.Sprintf("PS=%v", procName))
		if portCount > -1 {
			c.Env = append(c.Env, fmt.Sprintf("PORT=%d", port))
		}

		if r.ServiceDiscoveryAddr != "" {
			c.Env = append(c.Env, fmt.Sprintf("DISCOVERY=%v", r.ServiceDiscoveryAddr))
		}

		stderrPipe, err := c.StderrPipe()
		if err != nil {
			fmt.Fprintln(pw, "cannot open stderr pipe", procName, cmd)
			continue
		}
		stdoutPipe, err := c.StdoutPipe()
		if err != nil {
			fmt.Fprintln(pw, "cannot open stdout pipe", procName, cmd)
			continue
		}

		r.prefixedPrinter(ctx, stderrPipe, procName)
		r.prefixedPrinter(ctx, stdoutPipe, procName)

		isFirstCommand := idx == 0
		isLastCommand := idx+1 == len(sv.Cmd)
		if isFirstCommand && sv.WaitBefore != "" {
			r.waitFor(ctx, pw, sv.WaitBefore)
		} else if isLastCommand && sv.WaitFor != "" {
			r.waitFor(ctx, pw, sv.WaitFor)
		}

		if err := c.Run(); err != nil {
			fmt.Fprintf(pw, "exec error %s: (%s) %v\n", procName, cmd, err)
			return false
		}
	}
	return true
}

func (r *Runner) waitFor(ctx context.Context, w io.Writer, target string) {
	fmt.Fprintln(w, "waiting for", target)
	defer fmt.Fprintln(w, "starting")
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
			target = r.resolveProcessTypeAddress(target)
			c, err := net.Dial("tcp", target)
			if err == nil {
				c.Close()
				return
			}
		}
	}
}

func (r *Runner) resolveProcessTypeAddress(target string) string {
	r.sdMu.Lock()
	defer r.sdMu.Unlock()

	for name, port := range r.serviceDiscovery {
		if strings.HasPrefix(name, target) {
			return fmt.Sprint("localhost:", port)
		}
	}
	return target
}

func (r *Runner) prefixedPrinter(ctx context.Context, rdr io.Reader, name string) *bufio.Scanner {
	paddedName := (name + strings.Repeat(" ", r.longestProcessTypeName))[:r.longestProcessTypeName]
	scanner := bufio.NewScanner(rdr)
	scanner.Buffer(make([]byte, 65536), 2*1048576)
	go func() {
		for scanner.Scan() {
			fmt.Println(paddedName+":", scanner.Text())
		}

		select {
		// If the context is cancelled, we really don't care about
		// errors anymore.
		case <-ctx.Done():
			return
		default:
			if err := scanner.Err(); err != nil && err != os.ErrClosed && err != io.ErrClosedPipe {
				fmt.Println(paddedName+":", "error:", err)
			}
		}
	}()
	return scanner
}
