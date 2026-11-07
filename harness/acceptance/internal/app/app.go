// Package app builds and runs the real torrentd binary for black-box
// acceptance tests. It knows only the contract in spec/contracts/go-api.md.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// RepoRoot is the repository root (GATE_ROOT, or derived from this file).
func RepoRoot() string {
	if r := os.Getenv("GATE_ROOT"); r != "" {
		return r
	}
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
}

var (
	buildOnce sync.Once
	buildDir  string
	binPath   string
	buildErr  error
)

// Binary builds ./cmd/torrentd once per test process and returns its path.
func Binary(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		root := RepoRoot()
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
			buildErr = errors.New("no go.mod at the repo root: the product module does not exist yet")
			return
		}
		buildDir, buildErr = os.MkdirTemp("", "torrentd-acceptance-")
		if buildErr != nil {
			return
		}
		binPath = filepath.Join(buildDir, "torrentd")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/torrentd")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/torrentd failed: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

var (
	sharedMu   sync.Mutex
	sharedInst *Instance
)

// Main wraps testing.M: it runs the tests, then stops the shared instance and
// removes the build directory.
func Main(m *testing.M) int {
	code := m.Run()
	sharedMu.Lock()
	if sharedInst != nil {
		sharedInst.stop()
	}
	sharedMu.Unlock()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	return code
}

// Shared returns one default instance per test package, started on first use.
func Shared(t testing.TB) *Instance {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedInst != nil && !sharedInst.exited() {
		return sharedInst
	}
	inst, err := start(Binary(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	sharedInst = inst
	return inst
}

// FreePort returns a TCP port that was free a moment ago.
func FreePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Options configures an instance.
type Options struct {
	// Args are extra CLI arguments.
	Args []string
	// Env are extra environment variables (KEY=VALUE).
	Env []string
	// Raw disables the default --addr, --data-dir and --peer-port flags; the
	// test must then set ReadyAddr.
	Raw bool
	// ReadyAddr is the host:port polled for /healthz (default: the chosen --addr).
	ReadyAddr string
	// Home is the HOME directory (default: a fresh temp dir).
	Home string
}

// Instance is a running torrentd process.
type Instance struct {
	Addr    string // host:port used for requests
	DataDir string // the --data-dir passed (empty in Raw mode)
	Home    string

	cmd  *exec.Cmd
	logs *syncBuffer
	done chan struct{}
	code int
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Start launches an instance and stops it when the test ends.
func Start(t testing.TB, opts Options) *Instance {
	t.Helper()
	inst, err := start(Binary(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inst.stop() })
	return inst
}

// StartErr launches an instance and returns an error instead of failing.
func StartErr(t testing.TB, opts Options) (*Instance, error) {
	t.Helper()
	inst, err := start(Binary(t), opts)
	if inst != nil {
		t.Cleanup(func() { inst.stop() })
	}
	return inst, err
}

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k := kv[:strings.IndexByte(kv+"=", '=')]
		if strings.HasPrefix(k, "TORRENTD_") || k == "HOME" || k == "XDG_DATA_HOME" || k == "APPDATA" {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func start(bin string, opts Options) (*Instance, error) {
	home := opts.Home
	if home == "" {
		var err error
		if home, err = os.MkdirTemp("", "torrentd-home-"); err != nil {
			return nil, err
		}
	}
	inst := &Instance{Home: home, logs: &syncBuffer{}, done: make(chan struct{})}
	args := append([]string{}, opts.Args...)
	if !opts.Raw {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		inst.Addr = l.Addr().String()
		l.Close()
		inst.DataDir = filepath.Join(home, "data")
		args = append([]string{"--addr", inst.Addr, "--data-dir", inst.DataDir, "--peer-port", "0"}, args...)
	}
	if opts.ReadyAddr != "" {
		inst.Addr = opts.ReadyAddr
	}
	if inst.Addr == "" {
		return nil, errors.New("app: Raw mode requires ReadyAddr")
	}

	cmd := exec.Command(bin, args...)
	cmd.Env = append(append(cleanEnv(), "HOME="+home, "APPDATA="+filepath.Join(home, "AppData")), opts.Env...)
	cmd.Stdout = inst.logs
	cmd.Stderr = inst.logs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	inst.cmd = cmd
	go func() {
		err := cmd.Wait()
		inst.code = 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			inst.code = ee.ExitCode()
		} else if err != nil {
			inst.code = -1
		}
		close(inst.done)
	}()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if inst.exited() {
			return inst, fmt.Errorf("torrentd exited during startup (code %d); args %v\nlogs:\n%s", inst.code, args, inst.Logs())
		}
		resp, err := inst.Request(context.Background(), "GET", "/healthz", nil, nil)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return inst, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return inst, fmt.Errorf("torrentd did not answer GET /healthz with 200 on %s within 15s; args %v\nlogs:\n%s", inst.Addr, args, inst.Logs())
}

func (i *Instance) exited() bool {
	select {
	case <-i.done:
		return true
	default:
		return false
	}
}

// Logs returns everything the process wrote to stdout and stderr so far.
func (i *Instance) Logs() string { return i.logs.String() }

// Signal sends sig to the process.
func (i *Instance) Signal(sig os.Signal) error { return i.cmd.Process.Signal(sig) }

// Wait waits up to d for the process to exit and returns its exit code.
func (i *Instance) Wait(d time.Duration) (int, bool) {
	select {
	case <-i.done:
		return i.code, true
	case <-time.After(d):
		return 0, false
	}
}

func (i *Instance) stop() {
	if i.cmd == nil || i.exited() {
		return
	}
	_ = i.cmd.Process.Signal(syscall.SIGTERM)
	if _, ok := i.Wait(10 * time.Second); !ok {
		_ = syscall.Kill(-i.cmd.Process.Pid, syscall.SIGKILL)
		i.Wait(5 * time.Second)
	}
}

var client = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Request sends a request to the instance. headers may set "Host" to override
// the Host header.
func (i *Instance) Request(ctx context.Context, method, path string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+i.Addr+path, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

// Response is a fully-read HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Do sends a request and reads the whole response, failing the test on
// transport errors.
func (i *Instance) Do(t testing.TB, method, path string, body []byte, headers map[string]string) Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	resp, err := i.Request(context.Background(), method, path, r, headers)
	if err != nil {
		t.Fatalf("%s %s: %v\nlogs:\n%s", method, path, err, i.Logs())
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", method, path, err)
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: data}
}

// PID returns the process id.
func (i *Instance) PID() int { return i.cmd.Process.Pid }
