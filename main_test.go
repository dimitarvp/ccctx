package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixture is the statusline's real payload, trimmed. Its values are chosen so a
// swapped field shows: the five-hour utilization differs from the session limit's
// rounded percent, and one limit has no reset time.
func TestReadMeters(t *testing.T) {
	path := filepath.Join("testdata", "claude_usage_limits.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	const age = 90
	m, err := readMeters(path, fi.ModTime().Add(age*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	got, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := fmt.Sprintf(`{"age_seconds":%d,`+
		`"five_hour":{"percent":41.5,"resets_at":"2026-10-06T08:20:01.206561+00:00"},`+
		`"limits":[`+
		`{"kind":"session","percent":42,"resets_at":"2026-10-06T08:20:01.206561+00:00"},`+
		`{"kind":"weekly_all","percent":7,"resets_at":"2026-10-13T01:00:01.206580+00:00"},`+
		`{"kind":"weekly_scoped","percent":0,"resets_at":null}]}`, age)
	if string(got) != wantJSON {
		t.Errorf("JSON:\n got %s\nwant %s", got, wantJSON)
	}

	wantText := fmt.Sprintf("meters: cache age %ds\n", age) +
		"  five_hour 41.5% resets 2026-10-06T08:20:01.206561+00:00\n" +
		"  session 42% resets 2026-10-06T08:20:01.206561+00:00\n" +
		"  weekly_all 7% resets 2026-10-13T01:00:01.206580+00:00\n" +
		"  weekly_scoped 0%\n"
	if text := metersText(m); text != wantText {
		t.Errorf("text:\n got %q\nwant %q", text, wantText)
	}
}

// A missing cache is the common failure, as on a machine where the statusline has
// not fetched yet. Both outputs must say null.
func TestReadMetersMissingFile(t *testing.T) {
	m, err := readMeters(filepath.Join(t.TempDir(), "absent.json"), time.Now())
	if m != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got meters %v and error %v; want nil and a not-exist error", m, err)
	}
	out, err := json.Marshal(report{Meters: m})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"meters":null`) {
		t.Errorf(`report JSON lacks "meters":null: %s`, out)
	}
	if text := metersText(m); text != "meters: null\n" {
		t.Errorf("text = %q, want %q", text, "meters: null\n")
	}
}

// A cache that parses but lacks a number must fail, never read as 0% used.
func TestReadMetersMalformed(t *testing.T) {
	const fiveHour = `"five_hour": {"utilization": 3.0, "resets_at": null}`
	cases := []struct{ name, body, wantErr string }{
		{"truncated", `{"five_hour": `, "cannot parse"},
		{"no five_hour", `{"limits": []}`, "has no .five_hour.utilization"},
		{"null utilization", `{"five_hour": {"utilization": null}, "limits": []}`, "has no .five_hour.utilization"},
		{"no limits", `{` + fiveHour + `}`, "has no .limits"},
		{"limit without kind", `{` + fiveHour + `, "limits": [{"percent": 1, "resets_at": null}]}`, "has no .limits[0].kind"},
		{"limit without percent", `{` + fiveHour + `, "limits": [{"kind": "session", "resets_at": null}]}`, "has no .limits[0].percent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.json")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			m, err := readMeters(path, time.Now())
			if m != nil || err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("got meters %v and error %v; want nil and an error containing %q", m, err, c.wantErr)
			}
		})
	}
}
