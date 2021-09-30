// Package svc abstracts starting and stopping processes locally so they can be
// integrated into Mage targets.
package svc

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/magefile/mage/mg"
	"golang.org/x/sys/unix"
)

// New defines a new service with the provided name that can be started and stopped using the returned values as Mage
// targets.
func New(name string, options ...Option) Interface {
	cfg := config{
		name: name,
	} // TODO: let users configure this directory.
	for _, option := range options {
		option(&cfg)
	}
	if cfg.pidFile != `` {
		// leave it alone.
	} else if cfg.run.dir != `` {
		cfg.pidFile = filepath.Join(cfg.run.dir, name+`.pid`)
	} else {
		cfg.pidFile = name + `.pid`
	}
	return &cfg
}

// Interface describes the interface provided by a configured service.
type Interface interface {
	// Start returns a Mage target that will start the service if it is not already
	// running and wait until it is ready based on the configured checks.
	Start() mg.Fn

	// Stop returns a Mage target that will stop the service if it is already running
	// and clean up its pidfile.
	Stop() mg.Fn

	// Status returns the status of the service.
	Status(context.Context) *Status
}

// PIDFile specifies the PID file path, which defaults to name.pid in the service
// directory.  (The service directory itself defaults to the current directory.)
func PIDFile(path string) Option { return func(cfg *config) { cfg.pidFile = path } }

// Run specifies the command that should be run when starting the service.
func Run(name string, args ...string) Option {
	return func(cfg *config) {
		if cfg.run.args != nil {
			panic(fmt.Errorf(`services expect exactly one run option`))
		}
		if args == nil {
			args = []string{}
		}
		cfg.run.name, cfg.run.args = name, args
	}
}

// HTTPCheck specifies that getting a HTTP URL that should return a given status as a
// check for service readiness.
func HTTPCheck(url string, status int) Option {
	return Check(func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, `GET`, url, nil)
		if err != nil {
			return err
		}
		rsp, err := http.DefaultClient.Do(req)
		if err != nil {
			return ErrNotReady{}
		}
		defer rsp.Body.Close()
		defer io.Copy(ioutil.Discard, rsp.Body)
		if rsp.StatusCode != status {
			return ErrNotReady{}
		}
		return nil
	})
}

// DialCheck specifies a check for the ability to contact the service.
func DialCheck(network, address string) Option {
	var dialer net.Dialer
	dial := func(ctx context.Context) error {
		conn, err := dialer.DialContext(ctx, network, address)
		switch err {
		case nil:
			conn.Close()
			return nil
		case context.Canceled:
			return err
		default:
			if _, ok := err.(*net.AddrError); ok {
				return err // address errors should not be retried
			}
			return ErrNotReady{}
		}
	}
	return Check(dial)
}

// Check specifies functions that confirms the service is actually started and ready.  The check should return
// ErrNotReady if the service is not yet ready.
func Check(checks ...func(ctx context.Context) error) Option {
	return func(cfg *config) {
		cfg.checks = append(cfg.checks, checks...)
	}
}

type ErrNotReady struct{}

func (ErrNotReady) Error() string { return `service is not ready` }

// Dir specifies the starting directory for the service.  If the directory does not exist, it will be created.
func Dir(path string) Option {
	return func(cfg *config) {
		cfg.run.dir = path
	}
}

// Env extends the OS environment when starting a service.
func Env(environment ...string) Option {
	return func(cfg *config) {
		cfg.run.env = append(cfg.run.env, environment...)
	}
}

type Option func(*config)

type config struct {
	name    string
	pidFile string
	run     struct {
		name string
		args []string
		env  []string
		dir  string
	}
	checks []func(context.Context) error
}

func (cfg *config) ID() string { return cfg.name }
func (cfg *config) running() bool {
	return cfg.getProcess() != nil
}

func (cfg *config) getPIDFile() (pid int, mtime time.Time) {
	file, err := os.Open(cfg.pidFile)
	if err != nil {
		return // no pidfile
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return // can't stat the pidfile, we're going down in flames..
	}
	mtime = info.ModTime()
	data, err := ioutil.ReadAll(file)
	if err != nil {
		return // lost pidfile
	}
	pid, err = strconv.Atoi(string(data))
	if err != nil {
		pid = 0
		return // garbled pid file
	}
	return
}

func (cfg *config) getProcessByPID(pid int, start time.Time) *os.Process {
	sys, err := getSysinfo()
	if err == nil && time.Since(start).Seconds() > float64(sys.Uptime) {
		return nil // system rebooted since the pidfile was created, wraparound likely.
	}
	ps, err := os.FindProcess(pid)
	if err != nil {
		// This should never happen on UNIX, see godoc.
		return nil // pid terminated
	}
	err = ps.Signal(syscall.Signal(0))
	if err != nil {
		return nil // process did not exist or there is a permission problem.
	} // Signal 0 is not sent in UNIX, it just tests to see if it exists.
	return ps
}

func (cfg *config) getProcess() *os.Process {
	pid, mtime := cfg.getPIDFile()
	if pid < 2 {
		// just.. No.  Leave init alone, stupid.
		return nil
	}
	return cfg.getProcessByPID(pid, mtime)
}

func (cfg *config) check(ctx context.Context) error {
	ps := cfg.getProcess()
	if ps == nil {
		return fmt.Errorf(`process exited before starting checks`)
	}
	unready := len(cfg.checks)
	ready := make([]bool, unready)
	for {
		err := ctx.Err()
		if err != nil {
			return err
		}
		for i, check := range cfg.checks {
			if ready[i] {
				continue
			}
			err := check(ctx)
			switch err {
			case nil:
				unready--
				ready[i] = true
			case ErrNotReady{}:
				// fine.
			default:
				return err
			}
		}
		if unready < 1 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
		err = ps.Signal(syscall.Signal(0))
		if err != nil {
			return fmt.Errorf(`process %v exited before checks were satisfied`, ps.Pid)
		}
	}
}

func (cfg *config) kill(ps *os.Process) error {
	//TODO: progressive interrupt and wait.
	err := ps.Kill()
	if err != nil {
		return err
	}
	_, err = ps.Wait()
	return err
}

func (cfg *config) start(ctx context.Context) error {
	if cfg.running() {
		return cfg.check(ctx)
	}

	cmd := exec.CommandContext(ctx, cfg.run.name, cfg.run.args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, nil
	cmd.Env = append(os.Environ(), cfg.run.env...)
	if cfg.run.dir != `` {
		cmd.Dir = cfg.run.dir
		os.MkdirAll(cfg.run.dir, 0700)
	}
	err := cmd.Start()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(cfg.pidFile); dir != `` {
		os.MkdirAll(dir, 0700)
	}
	err = ioutil.WriteFile(cfg.pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0600)
	if err != nil {
		_ = cfg.kill(cmd.Process)
		return err
	}
	err = cmd.Process.Release()
	if err != nil {
		_ = cfg.kill(cmd.Process)
		return err
	}
	err = cfg.check(ctx)
	if err != nil {
		_ = cfg.kill(cmd.Process)
		return err
	}
	return nil
}

func (cfg *config) stop(ctx context.Context) error {
	ps := cfg.getProcess()
	if ps == nil {
		os.RemoveAll(cfg.pidFile)
		return nil
	}
	err := ps.Kill()
	if err != nil {
		return err
	}
	defer os.RemoveAll(cfg.pidFile)
	ps.Wait() // will fail unless this mage instance also did the start.
	return nil
}

func (cfg *config) Start() mg.Fn { return &start{*cfg} }
func (cfg *config) Stop() mg.Fn  { return &stop{*cfg} }
func (cfg *config) Status(ctx context.Context) *Status {
	nfo := new(Status)
	nfo.Name = cfg.name
	nfo.PID, nfo.Started = cfg.getPIDFile()
	if nfo.PID == 0 {
		return nfo
	}
	nfo.Running = cfg.getProcessByPID(nfo.PID, nfo.Started) != nil
	if !nfo.Running {
		return nfo
	}
	err := cfg.check(ctx)
	if err != nil {
		return nfo
	}
	nfo.Ready = true
	return nfo
}

// Status explains the status of a service, as understood by this package.
type Status struct {
	// Name is the configured name of the service.
	Name string `json:"name"`

	// PID is nonzero if the PID file could be read.  If this is zero, then all the
	// other fields will also be zero.
	PID int `json:"pid,omitempty"`

	// Started is the time the PID file was written.
	Started time.Time `json:"started,omitempty"`

	// Running is true if the PID matches a running process and the PID file was
	// modified after the last time the system started.
	Running bool `json:"running,omitempty"`

	// Ready is true if the service passed all of its checks.
	Ready bool `json:"ready,omitempty"`
}

// Print writes the status to stderr.
func (nfo *Status) Print() {
	fmt.Fprintln(os.Stderr, nfo.String())
}

// String explains the status in English
func (nfo *Status) String() string {
	var buf strings.Builder
	buf.WriteString(nfo.Name)
	if nfo.PID == 0 {
		buf.WriteString(` is not running`)
		return buf.String()
	} else if !nfo.Running {
		buf.WriteString(` had pid `)
		buf.WriteString(strconv.Itoa(nfo.PID))
		buf.WriteString(` and is not running`)
		return buf.String()
	} else {
		buf.WriteString(` has pid `)
		buf.WriteString(strconv.Itoa(nfo.PID))
	}
	if nfo.Ready {
		buf.WriteString(` and is ready`)
	} else {
		buf.WriteString(` and is running but not ready`)
	}
	return buf.String()
}

type start struct{ config }

func (cfg *start) Name() string                  { return `start` }
func (cfg *start) Run(ctx context.Context) error { return cfg.start(ctx) }

type stop struct{ config }

func (cfg *stop) Name() string                  { return `stop` }
func (cfg *stop) Run(ctx context.Context) error { return cfg.stop(ctx) }

func getSysinfo() (*unix.Sysinfo_t, error) {
	getSysinfoOnce.Do(func() {
		sysinfoErr = unix.Sysinfo(&sysinfo)
	})
	return &sysinfo, sysinfoErr
}

var (
	getSysinfoOnce sync.Once
	sysinfo        unix.Sysinfo_t
	sysinfoErr     error
)
