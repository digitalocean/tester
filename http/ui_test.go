package http

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/tester"
	"github.com/digitalocean/tester/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// uiTestSummaries builds one bucket per summaryState for package "pkg".
func uiTestSummaries(begin time.Time, window time.Duration) []*tester.RunSummary {
	newPkg := func() *tester.PackageSummary {
		return &tester.PackageSummary{
			Package:      "pkg",
			PassedTests:  map[string][]uuid.UUID{},
			FailedTests:  map[string][]uuid.UUID{},
			SkippedTests: map[string][]uuid.UUID{},
		}
	}

	results := newPkg()
	results.RunIDs = []uuid.UUID{uuid.New()}
	results.PassedTests["TestA"] = []uuid.UUID{uuid.New()}
	results.FailedTests["TestB"] = []uuid.UUID{uuid.New()}

	running := newPkg()
	running.RunIDs = []uuid.UUID{uuid.New()}
	running.RunningRunIDs = running.RunIDs

	errored := newPkg()
	errored.ErrorRunIDs = []uuid.UUID{uuid.New()}

	active := newPkg()
	active.RunIDs = []uuid.UUID{uuid.New()}

	var summaries []*tester.RunSummary
	for i, ps := range []*tester.PackageSummary{results, running, errored, active, nil} {
		s := &tester.RunSummary{
			Time:           begin.Add(time.Duration(i) * window),
			Duration:       window,
			PackageSummary: map[string]*tester.PackageSummary{},
		}
		if ps != nil {
			s.PackageSummary["pkg"] = ps
		}
		summaries = append(summaries, s)
	}
	return summaries
}

func TestSummaryState(t *testing.T) {
	summaries := uiTestSummaries(time.Now(), time.Minute)
	assert.Equal(t, "results", summaryState(summaries[0].PackageSummary["pkg"]))
	assert.Equal(t, "running", summaryState(summaries[1].PackageSummary["pkg"]))
	assert.Equal(t, "errored", summaryState(summaries[2].PackageSummary["pkg"]))
	assert.Equal(t, "active", summaryState(summaries[3].PackageSummary["pkg"]))
	assert.Equal(t, "empty", summaryState(summaries[4].PackageSummary["pkg"]), "missing package is a nil *PackageSummary")
	assert.Equal(t, "empty", summaryState(summaries[4]))
	assert.Equal(t, "results", summaryState(summaries[0]))
	assert.Equal(t, "running", summaryState(summaries[1]))
	assert.Equal(t, "empty", summaryState((*tester.RunSummary)(nil)))

	// Errored takes precedence over running when there are no results.
	both := &tester.PackageSummary{
		RunIDs: []uuid.UUID{uuid.New()}, RunningRunIDs: []uuid.UUID{uuid.New()}, ErrorRunIDs: []uuid.UUID{uuid.New()},
	}
	assert.Equal(t, "errored", summaryState(both))
}

func TestUI_RendersRunSummaryStates(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockDB := db.NewMockDB(ctrl)
	packages := []*tester.Package{{Name: "pkg"}}
	ui := NewUIHandler(mockDB, packages)
	ts := httptest.NewServer(ui)
	defer ts.Close()

	begin := time.Now().Truncate(5 * time.Minute).Add(-time.Hour)
	summaries := uiTestSummaries(begin, 5*time.Minute)

	get := func(path string) (int, string) {
		resp, err := ts.Client().Get(ts.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}

	t.Run("dashboard", func(t *testing.T) {
		// The dashboard loads the hour, day and month strips in parallel.
		mockDB.EXPECT().ListRunSummariesInRange(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(summaries, nil).Times(3)

		status, body := get("/")
		require.Equal(t, http.StatusOK, status, body)
		assert.Contains(t, body, `class="summary-running"`)
		assert.Contains(t, body, `class="summary-active"`)
		assert.Contains(t, body, "1 running")
		assert.Contains(t, body, "1 erred")
		assert.Contains(t, body, "bg-success")
	})

	t.Run("run_summary", func(t *testing.T) {
		mockDB.EXPECT().
			ListRunSummariesInRange(gomock.Any(), gomock.Any(), gomock.Any(), 5*time.Minute).
			Return(summaries[1:2], nil)

		status, body := get(fmt.Sprintf("/run_summary?package=pkg&begin=%d&window=300", begin.Unix()))
		require.Equal(t, http.StatusOK, status, body)
		assert.Contains(t, body, "1 Runs (1 running)")
		assert.Contains(t, body, "/runs/"+summaries[1].PackageSummary["pkg"].RunningRunIDs[0].String())
		assert.Equal(t, 1, strings.Count(body, `class="badge bg-info rounded-pill"`))
	})

	t.Run("run_summary errored", func(t *testing.T) {
		mockDB.EXPECT().
			ListRunSummariesInRange(gomock.Any(), gomock.Any(), gomock.Any(), 5*time.Minute).
			Return(summaries[2:3], nil)

		status, body := get(fmt.Sprintf("/run_summary?package=pkg&begin=%d&window=300", begin.Unix()))
		require.Equal(t, http.StatusOK, status, body)
		assert.Contains(t, body, "/runs/"+summaries[2].PackageSummary["pkg"].ErrorRunIDs[0].String())
	})
}
