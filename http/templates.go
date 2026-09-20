package http

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/digitalocean/tester"
)

// templateFS holds the HTML templates compiled into the binary. Paths are
// relative to this package: "templates/layouts/default.html" etc.
//
//go:embed templates
var templateFS embed.FS

type errTemplateNotFound struct {
	path string
}

func (e *errTemplateNotFound) Error() string {
	return fmt.Sprintf("template not found: %s", e.path)
}

type errTemplateInvalid struct {
	path string
}

func (e *errTemplateInvalid) Error() string {
	return fmt.Sprintf("template invalid: %s", e.path)
}

// ExecuteTemplate runs the given template with the value
func (s *UIHandler) ExecuteTemplate(name string, w io.Writer, value interface{}) error {
	defaultLayoutPath := "templates/layouts/default.html"
	layoutContent, err := templateFS.ReadFile(defaultLayoutPath)
	if err != nil {
		return &errTemplateNotFound{defaultLayoutPath}
	}

	layout, err := template.New("layout_default").Funcs(s.templateFuncs()).Parse(string(layoutContent))
	if err != nil {
		return err
	}

	err = fs.WalkDir(templateFS, "templates/shared", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		templateData, err := templateFS.ReadFile(path)
		if err != nil {
			return &errTemplateNotFound{path}
		}

		layout, err = parseTemplate(layout, string(templateData))
		return err
	})
	if err != nil {
		return fmt.Errorf("loading shared partial: %w", err)
	}

	templatePath := "templates/" + name + ".html"
	templateData, err := templateFS.ReadFile(templatePath)
	if err != nil {
		return &errTemplateNotFound{templatePath}
	}

	t, err := parseTemplate(layout, string(templateData))
	if err != nil {
		return err
	}

	return t.Execute(w, value)
}

func parseTemplate(layout *template.Template, content string) (*template.Template, error) {
	t, err := layout.Clone()
	if err != nil {
		return nil, err
	}

	_, err = t.New("content").Parse(content)
	return t, err
}

type subTest struct {
	ParentTest *tester.T
	Test       *tester.T
	Level      int
	NextLevel  int
}

func (s *UIHandler) templateFuncs() template.FuncMap {
	return template.FuncMap{
		"asSubTest": func(parent *tester.T, level int, test *tester.T) subTest {
			return subTest{
				ParentTest: parent,
				Test:       test,
				Level:      level,
				NextLevel:  level + 1,
			}
		},
		"subTestNameIndent": func(level int) int {
			return level * 10
		},
		"trimPrefix": func(prefix, s string) string {
			return strings.TrimPrefix(s, prefix)
		},
		"formatTime": func(t time.Time) string {
			return t.Format("2006-01-02 15:04:05")
		},
		"formatTimeRFC3339": func(t time.Time) string {
			return t.Format(time.RFC3339)
		},
		"formatRelativeTime": func(t time.Time) string {
			d := time.Now().Sub(t)
			var suffix string
			if d > 0 {
				suffix = "ago"
			} else {
				suffix = "from now"
			}
			return fmt.Sprintf("%s %s", d.Round(time.Second).String(), suffix)
		},
		"formatDuration": func(d time.Duration) string {
			if d < 1*time.Millisecond {
				return d.Round(time.Microsecond).String()
			}
			if d < 1*time.Minute {
				return d.Round(time.Millisecond).String()
			}
			return d.Round(time.Second).String()
		},
		"formatPercent": func(f float64) float64 {
			return f * 100
		},
		"formatLogTime": func(t time.Time) string {
			return t.Format("15:04:05")
		},
		"formatLogOutput": func(o []byte) string {
			return string(o)
		},
		"testStateMessage": func(state tester.TBState) string {
			return string(state)
		},
		"testStateColour": func(state tester.TBState) string {
			switch state {
			case tester.TBStatePassed:
				return "success"
			case tester.TBStateFailed:
				return "danger"
			case tester.TBStateSkipped:
				return "warning"
			default:
				return "unknown"
			}
		},
		"runState": func(run *tester.Run) string {
			if run.StartedAt.IsZero() {
				return "pending"
			}
			if run.FinishedAt.IsZero() {
				return "running"
			}
			if run.Error == "" {
				return "finished"
			}
			return "failed"
		},
		"runTests": func(run *tester.Run) int {
			return len(run.Tests)
		},
		"runTestsPassed": func(run *tester.Run) int {
			num := 0
			for _, t := range run.Tests {
				if t.Result.State == tester.TBStatePassed {
					num++
				}
			}
			return num
		},
		"runTestsPassedPercent": func(run *tester.Run) float64 {
			num := 0
			for _, t := range run.Tests {
				if t.Result.State == tester.TBStatePassed {
					num++
				}
			}
			return float64(num) / float64(len(run.Tests)) * 100
		},
		"runTestsSkipped": func(run *tester.Run) int {
			num := 0
			for _, t := range run.Tests {
				if t.Result.State == tester.TBStateSkipped {
					num++
				}
			}
			return num
		},
		"runTestsSkippedPercent": func(run *tester.Run) float64 {
			num := 0
			for _, t := range run.Tests {
				if t.Result.State == tester.TBStateSkipped {
					num++
				}
			}
			return float64(num) / float64(len(run.Tests)) * 100
		},
		"runTestsFailed": func(run *tester.Run) int {
			num := 0
			for _, t := range run.Tests {
				if t.Result.State == tester.TBStateFailed {
					num++
				}
			}
			return num
		},
		"runTestsFailedPercent": func(run *tester.Run) float64 {
			num := 0
			for _, t := range run.Tests {
				if t.Result.State == tester.TBStateFailed {
					num++
				}
			}
			return float64(num) / float64(len(run.Tests)) * 100
		},
	}
}
