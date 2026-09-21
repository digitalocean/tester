package runner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/digitalocean/tester"
	"github.com/google/uuid"
)

// fakeTester is a minimal stand-in for the tester API: it hands out one run
// for a single package and records what the runner reports back.
type fakeTester struct {
	t   *testing.T
	pkg tester.Package
	run tester.Run

	mu        sync.Mutex
	tests     []tester.Test
	completed bool
	failed    bool
	failError string
}

func newFakeTester(t *testing.T, pkg tester.Package) *fakeTester {
	return &fakeTester{
		t:   t,
		pkg: pkg,
		run: tester.Run{ID: uuid.New(), Package: pkg.Name},
	}
}

func (f *fakeTester) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/runs/claim":
		json.NewEncoder(w).Encode(f.run)
	case r.Method == http.MethodGet && r.URL.Path == "/api/packages/"+f.pkg.Name:
		json.NewEncoder(w).Encode(f.pkg)
	case r.Method == http.MethodPost && r.URL.Path == "/api/tests":
		var test tester.Test
		if err := json.Unmarshal(body, &test); err != nil {
			f.t.Errorf("bad test submission: %s", err)
		}
		f.tests = append(f.tests, test)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/api/runs/%s/complete", f.run.ID):
		f.completed = true
	case r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/api/runs/%s/fail", f.run.ID):
		f.failed = true
		if err := json.Unmarshal(body, &f.failError); err != nil {
			f.t.Errorf("bad fail body: %s", err)
		}
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// newRunnerWithScript installs script as the "test binary" for pkg and returns
// a Runner pointed at a fake tester that will hand out one run for it.
func newRunnerWithScript(t *testing.T, script string) (*Runner, *fakeTester) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH; runner needs `go tool test2json`")
	}

	const pkgName = "fake"
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, pkgName)
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("#!/bin/sh\n" + script))

	ft := newFakeTester(t, tester.Package{Name: pkgName, Path: binPath, SHA256Sum: fmt.Sprintf("%x", sum)})
	srv := httptest.NewServer(ft)
	t.Cleanup(srv.Close)

	r, err := New(
		WithTesterAddr(srv.URL),
		WithTestBinsPath(binDir),
		WithLocalTestBinsOnly(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return r, ft
}

func TestRunOnce_NoTestResultsFailsRun(t *testing.T) {
	// Mimics a TestMain that bails out during setup: diagnostics on stderr,
	// exit 1, no test events at all.
	r, ft := newRunnerWithScript(t, `
echo "Using doctl-agents version: 1.2.3"
echo "agents e2e suite is misconfigured: ACCESS_TOKEN not set" >&2
exit 1
`)

	err := r.runOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "produced no test results") {
		t.Fatalf("expected no-test-results error, got %v", err)
	}

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.completed {
		t.Error("run was completed; a run with no tests must not be completed")
	}
	if !ft.failed {
		t.Fatal("run was not failed")
	}
	for _, want := range []string{
		"no test results",
		"Exit Code: 1",
		"Using doctl-agents version: 1.2.3",
		"ACCESS_TOKEN not set",
	} {
		if !strings.Contains(ft.failError, want) {
			t.Errorf("fail message missing %q:\n%s", want, ft.failError)
		}
	}
	if len(ft.tests) != 0 {
		t.Errorf("expected no submitted tests, got %d", len(ft.tests))
	}
}

func TestRunOnce_TestsCompleteRun(t *testing.T) {
	// A binary that runs one passing and one failing test and exits 1, which
	// is the normal "some tests failed" exit status and must still complete.
	r, ft := newRunnerWithScript(t, `
echo "=== RUN   TestPass"
echo "--- PASS: TestPass (0.00s)"
echo "=== RUN   TestFail"
echo "--- FAIL: TestFail (0.00s)"
echo "FAIL"
exit 1
`)

	if err := r.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.failed {
		t.Errorf("run was failed: %s", ft.failError)
	}
	if !ft.completed {
		t.Error("run was not completed")
	}
	if len(ft.tests) != 2 {
		t.Fatalf("expected 2 submitted tests, got %d", len(ft.tests))
	}
}

func TestTailBytes(t *testing.T) {
	if got := tailBytes([]byte("short"), 10); string(got) != "short" {
		t.Errorf("short input altered: %q", got)
	}
	got := string(tailBytes([]byte("0123456789"), 4))
	if !strings.HasPrefix(got, "... (6 bytes truncated)\n") || !strings.HasSuffix(got, "6789") {
		t.Errorf("unexpected truncation: %q", got)
	}
}
