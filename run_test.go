package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// result is what a caller of the command observes.
type result struct {
	code           int
	stdout, stderr string
}

// ccctx runs the command against an empty sensor directory, an empty projects
// directory and the meters fixture, so nothing on the machine is read.
func ccctx(t *testing.T, args ...string) result {
	t.Helper()
	t.Setenv("CCCTX_USAGE_FILE", filepath.Join("testdata", "claude_usage_limits.json"))
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"-sensor-dir", t.TempDir(), "-projects", t.TempDir()}, args...), &stdout, &stderr)
	return result{code, stdout.String(), stderr.String()}
}

// writeSession writes a transcript of the given entries and returns its path.
func writeSession(t *testing.T, entries ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(entries, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assistant is one assistant entry with its usage block, as Claude Code writes it.
func assistant(id string, input, cacheRead, cacheCreation, output int) string {
	return fmt.Sprintf(`{"type":"assistant","version":"2.1.0","timestamp":"2026-01-01T00:00:00Z",`+
		`"message":{"id":%q,"model":"claude-test","usage":{"input_tokens":%d,"cache_read_input_tokens":%d,`+
		`"cache_creation_input_tokens":%d,"output_tokens":%d}}}`, id, input, cacheRead, cacheCreation, output)
}

// reply is an assistant entry whose prompt is spread over the three usage fields that
// carry it, so a sum that leaves one of them out shows.
func reply(id string, prompt, output int) string {
	input, cacheCreation := prompt/10, prompt/5
	return assistant(id, input, prompt-input-cacheCreation, cacheCreation, output)
}

// decodeReport parses the JSON report into its keys, in the order written, and their
// values, with numbers kept exact.
func decodeReport(t *testing.T, stdout string) (keys []string, values map[string]any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("stdout does not open with a JSON object (token %v, error %v):\n%s", tok, err, stdout)
	}
	values = map[string]any{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		key, _ := tok.(string)
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
		values[key] = v
	}
	return keys, values
}

// checkValues compares report values in their printed form, so 864444, false and
// "flag" are written the way they read.
func checkValues(t *testing.T, values map[string]any, want map[string]string) {
	t.Helper()
	for key, w := range want {
		if got := fmt.Sprint(values[key]); got != w {
			t.Errorf("%s = %s, want %s", key, got, w)
		}
	}
}

func usedPct(t *testing.T, values map[string]any) string {
	t.Helper()
	n, _ := values["used_pct"].(json.Number)
	pct, err := n.Float64()
	if err != nil {
		t.Fatalf("used_pct = %v, want a number", values["used_pct"])
	}
	return fmt.Sprintf("%.2f", pct)
}

func TestRunUnderThreshold(t *testing.T) {
	path := writeSession(t, reply("m1", 400_000, 1_000), reply("m2", 860_000, 4_444))
	res := ccctx(t, "-window", "1000000", "-transcript", path)
	if res.code != exitOK {
		t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
	}
	keys, values := decodeReport(t, res.stdout)
	wantKeys := "session_id transcript cc_version current_tokens high_water_tokens model turns compactions per_model_peak " +
		"window_tokens window_source usable_tokens used_pct threshold_tokens over_threshold meters"
	if got := strings.Join(keys, " "); got != wantKeys {
		t.Errorf("keys:\n got %s\nwant %s", got, wantKeys)
	}
	checkValues(t, values, map[string]string{
		"current_tokens":    "860000",
		"high_water_tokens": "860000",
		"window_tokens":     "1000000",
		"window_source":     "flag",
		"usable_tokens":     "966000",
		"threshold_tokens":  "927360",
		"over_threshold":    "false",
	})
	if pct := usedPct(t, values); pct != "89.03" {
		t.Errorf("used_pct = %s, want 89.03", pct)
	}
}

func TestRunOverThreshold(t *testing.T) {
	path := writeSession(t, reply("m1", 940_000, 1_000))
	res := ccctx(t, "-window", "1000000", "-transcript", path)
	if res.code != exitOver {
		t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOver, res.stderr)
	}
	_, values := decodeReport(t, res.stdout)
	checkValues(t, values, map[string]string{
		"high_water_tokens": "940000",
		"threshold_tokens":  "927360",
		"over_threshold":    "true",
	})
}

// Without a window size there is nothing to judge: -no-threshold reports occupancy
// alone, and without it the missing size is drift.
func TestRunNoWindow(t *testing.T) {
	path := writeSession(t, reply("m1", 500_000, 2_000))

	res := ccctx(t, "-no-threshold", "-transcript", path)
	if res.code != exitOK {
		t.Fatalf("-no-threshold: exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
	}
	keys, _ := decodeReport(t, res.stdout)
	wantKeys := "session_id transcript cc_version current_tokens high_water_tokens model turns compactions per_model_peak over_threshold meters"
	if got := strings.Join(keys, " "); got != wantKeys {
		t.Errorf("keys:\n got %s\nwant %s", got, wantKeys)
	}

	res = ccctx(t, "-transcript", path)
	if res.code != exitDrift || !strings.Contains(res.stderr, "no context window size available") {
		t.Errorf("no window: exit %d, stderr:\n%s\nwant exit %d naming the missing window size", res.code, res.stderr, exitDrift)
	}
}

func TestRunBadInvocation(t *testing.T) {
	path := writeSession(t, reply("m1", 500_000, 2_000))
	cases := []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{"unknown flag", []string{"-bogus", "-transcript", path}, exitFail, "flag provided but not defined: -bogus"},
		{"flag value that is not a number", []string{"-window", "lots", "-transcript", path}, exitFail, `invalid value "lots"`},
		{"threshold out of range", []string{"-threshold", "1.5", "-window", "1000000", "-transcript", path}, exitFail, "-threshold must be between 0 and 1"},
		{"missing transcript", []string{"-transcript", filepath.Join(t.TempDir(), "absent.jsonl")}, exitFail, "cannot read transcript"},
		{"help", []string{"-h"}, exitOK, "usage: ccctx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ccctx(t, c.args...)
			if res.code != c.code || res.stdout != "" || !strings.Contains(res.stderr, c.stderr) {
				t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant exit %d, no stdout, stderr containing %q",
					res.code, res.stdout, res.stderr, c.code, c.stderr)
			}
		})
	}
}

// A usage block whose token keys were renamed must stop the run, never read as an
// empty context.
func TestRunDrift(t *testing.T) {
	path := writeSession(t, assistant("m1", 0, 0, 0, 0), assistant("m2", 0, 0, 0, 0))
	res := ccctx(t, "-window", "1000000", "-transcript", path)
	if res.code != exitDrift || res.stdout != "" || !strings.Contains(res.stderr, `"input_tokens"`) {
		t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant exit %d naming the usage keys", res.code, res.stdout, res.stderr, exitDrift)
	}
}

func TestRunPlain(t *testing.T) {
	path := writeSession(t, reply("m1", 860_000, 4_444))
	res := ccctx(t, "-plain", "-window", "1000000", "-transcript", path)
	lines := strings.SplitN(res.stdout, "\n", 2)
	if res.code != exitOK || len(lines) != 2 || lines[0] != "860000" || !strings.HasPrefix(lines[1], "meters: cache age ") {
		t.Errorf("exit %d, stdout:\n%s\nwant exit %d, the high water on line 1, then the meters", res.code, res.stdout, exitOK)
	}
}
