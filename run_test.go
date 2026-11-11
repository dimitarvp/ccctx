package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// checkInvariant holds for every report with limits: over_checkpoint_line is true
// exactly when nothing is left to the checkpoint line.
func checkInvariant(t *testing.T, values map[string]any) {
	t.Helper()
	n, _ := values["left_to_checkpoint_tokens"].(json.Number)
	left, err := n.Int64()
	if err != nil {
		t.Fatalf("left_to_checkpoint_tokens = %v, want an integer", values["left_to_checkpoint_tokens"])
	}
	if over := values["over_checkpoint_line"] == true; over != (left <= 0) {
		t.Errorf("over_checkpoint_line is %v with %d tokens left to the checkpoint line", over, left)
	}
}

// A session under the checkpoint line on a known window: every field of the report,
// in order, and exit 0.
func TestRunUnderTheLine(t *testing.T) {
	path := writeSession(t, reply("m1", 400_000, 1_000), reply("m2", 860_000, 4_444))
	res := ccctx(t, "-window", "1000000", "-transcript", path)
	if res.code != exitOK {
		t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
	}
	keys, values := decodeReport(t, res.stdout)
	wantKeys := "session_id transcript cc_version current_tokens high_water_tokens model turns compactions per_model_peak " +
		"window_tokens window_source usable_tokens used_pct checkpoint_line_tokens " +
		"left_to_checkpoint_tokens left_to_compaction_tokens over_checkpoint_line meters"
	if got := strings.Join(keys, " "); got != wantKeys {
		t.Errorf("keys:\n got %s\nwant %s", got, wantKeys)
	}
	checkValues(t, values, map[string]string{
		"current_tokens":            "864444",
		"high_water_tokens":         "864444",
		"window_tokens":             "1000000",
		"window_source":             "flag",
		"usable_tokens":             "966000",
		"checkpoint_line_tokens":    "936000",
		"left_to_checkpoint_tokens": "71556",
		"left_to_compaction_tokens": "101556",
		"over_checkpoint_line":      "false",
	})
	if pct := usedPct(t, values); pct != "89.49" {
		t.Errorf("used_pct = %s, want 89.49", pct)
	}
	checkInvariant(t, values)
}

// The checkpoint line is a token count: one token under it passes, the line itself
// asks for the checkpoint, and past it the distance to the line goes negative.
func TestRunCheckpointLine(t *testing.T) {
	cases := []struct {
		highWater, code                    int
		leftToCheckpoint, leftToCompaction string
		over                               string
	}{
		{935_999, exitOK, "1", "30001", "false"},
		{936_000, exitOver, "0", "30000", "true"},
		{940_000, exitOver, "-4000", "26000", "true"},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.highWater), func(t *testing.T) {
			path := writeSession(t, reply("m1", c.highWater-3_000, 3_000))
			res := ccctx(t, "-window", "1000000", "-transcript", path)
			if res.code != c.code {
				t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, c.code, res.stderr)
			}
			_, values := decodeReport(t, res.stdout)
			checkValues(t, values, map[string]string{
				"high_water_tokens":         strconv.Itoa(c.highWater),
				"checkpoint_line_tokens":    "936000",
				"left_to_checkpoint_tokens": c.leftToCheckpoint,
				"left_to_compaction_tokens": c.leftToCompaction,
				"over_checkpoint_line":      c.over,
			})
			checkInvariant(t, values)
		})
	}
}

// The margins are token counts, so a 200,000 window keeps the same 34,000 and 30,000
// below it.
func TestRunSmallWindow(t *testing.T) {
	path := writeSession(t, reply("m1", 120_000, 2_000))
	res := ccctx(t, "-window", "200000", "-transcript", path)
	if res.code != exitOK {
		t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
	}
	_, values := decodeReport(t, res.stdout)
	checkValues(t, values, map[string]string{
		"usable_tokens":             "166000",
		"checkpoint_line_tokens":    "136000",
		"left_to_checkpoint_tokens": "14000",
		"left_to_compaction_tokens": "44000",
	})
	checkInvariant(t, values)
}

// The reply is part of the context. The figures are a real forced compaction: the
// last prompt held 953,841 tokens and its reply 11,865, which left 294 tokens to the
// compaction where the prompt alone said 12,159.
func TestRunCountsOutput(t *testing.T) {
	path := writeSession(t, reply("m1", 500_000, 2_000), reply("m2", 953_841, 11_865))
	res := ccctx(t, "-window", "1000000", "-transcript", path)
	if res.code != exitOver {
		t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOver, res.stderr)
	}
	_, values := decodeReport(t, res.stdout)
	checkValues(t, values, map[string]string{
		"current_tokens":            "965706",
		"high_water_tokens":         "965706",
		"left_to_checkpoint_tokens": "-29706",
		"left_to_compaction_tokens": "294",
	})
	checkInvariant(t, values)
}

// Claude Code writes a message id again as its reply streams in. The message is its
// larger reading, whichever order the two arrive in.
func TestRunKeepsLargestReading(t *testing.T) {
	for _, outputs := range [][2]int{{100, 5_000}, {5_000, 100}} {
		t.Run(fmt.Sprintf("output %d then %d", outputs[0], outputs[1]), func(t *testing.T) {
			path := writeSession(t, reply("m1", 700_000, outputs[0]), reply("m1", 700_000, outputs[1]))
			res := ccctx(t, "-no-threshold", "-transcript", path)
			if res.code != exitOK {
				t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
			}
			_, values := decodeReport(t, res.stdout)
			checkValues(t, values, map[string]string{
				"current_tokens": "705000",
				"turns":          "1",
			})
		})
	}
}

// Without a window size there is nothing to judge: -no-threshold reports occupancy
// alone, with none of the limit fields, and without it the missing size is drift.
func TestRunNoWindow(t *testing.T) {
	path := writeSession(t, reply("m1", 500_000, 2_000))

	res := ccctx(t, "-no-threshold", "-transcript", path)
	if res.code != exitOK {
		t.Fatalf("-no-threshold: exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
	}
	keys, _ := decodeReport(t, res.stdout)
	wantKeys := "session_id transcript cc_version current_tokens high_water_tokens model turns compactions per_model_peak meters"
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
		stderr []string
	}{
		{"unknown flag", []string{"-bogus", "-transcript", path}, exitFail, []string{"flag provided but not defined: -bogus"}},
		{"-threshold is not a flag", []string{"-threshold", "0.9", "-window", "1000000", "-transcript", path}, exitFail, []string{"flag provided but not defined: -threshold"}},
		{"flag value that is not a number", []string{"-window", "lots", "-transcript", path}, exitFail, []string{`invalid value "lots"`}},
		{"window smaller than reserve plus room", []string{"-window", "60000", "-transcript", path}, exitFail, []string{"60000", "34000", "30000"}},
		{"window equal to reserve plus room", []string{"-window", "64000", "-transcript", path}, exitFail, []string{"64000", "34000", "30000"}},
		{"negative checkpoint room", []string{"-window", "1000000", "-checkpoint-room", "-1", "-transcript", path}, exitFail, []string{"1000000", "34000", "(-1)"}},
		{"negative compaction reserve", []string{"-window", "1000000", "-compaction-reserve", "-5", "-transcript", path}, exitFail, []string{"1000000", "(-5)", "30000"}},
		{"missing transcript", []string{"-transcript", filepath.Join(t.TempDir(), "absent.jsonl")}, exitFail, []string{"cannot read transcript"}},
		{"help", []string{"-h"}, exitOK, []string{"usage: ccctx", "-checkpoint-room"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ccctx(t, c.args...)
			missing := []string{}
			for _, s := range c.stderr {
				if !strings.Contains(res.stderr, s) {
					missing = append(missing, s)
				}
			}
			if res.code != c.code || res.stdout != "" || len(missing) > 0 {
				t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant exit %d, no stdout, stderr containing %q",
					res.code, res.stdout, res.stderr, c.code, missing)
			}
		})
	}
}

// A usage block whose token keys were renamed must stop the run, never read as a
// smaller context. The prompt side and the output are checked apart, so a rename on
// one side cannot hide behind the other's tokens.
func TestRunDrift(t *testing.T) {
	cases := []struct {
		name, key string
		entries   []string
	}{
		{"prompt side all zero", `"input_tokens"`, []string{assistant("m1", 0, 0, 0, 900), assistant("m2", 0, 0, 0, 1_200)}},
		{"output all zero", `"message"."usage"."output_tokens"`, []string{reply("m1", 400_000, 0), reply("m2", 500_000, 0)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ccctx(t, "-window", "1000000", "-transcript", writeSession(t, c.entries...))
			if res.code != exitDrift || res.stdout != "" || !strings.Contains(res.stderr, c.key) {
				t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant exit %d naming %s", res.code, res.stdout, res.stderr, exitDrift, c.key)
			}
		})
	}
}

// -plain prints the high water, then the limits on one line, then the meters.
func TestRunPlain(t *testing.T) {
	cases := []struct {
		name      string
		highWater int
		args      []string
		code      int
		limits    string
	}{
		{
			"under the line", 864_444,
			[]string{"-window", "1000000"},
			exitOK,
			"limits: checkpoint line 936000 (71556 left), forced compaction 966000 (101556 left)",
		},
		{
			"past the line", 940_000,
			[]string{"-window", "1000000"},
			exitOver,
			"limits: checkpoint line 936000 (-4000 left), forced compaction 966000 (26000 left)",
		},
		{"no window", 864_444, []string{"-no-threshold"}, exitOK, "limits: null"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeSession(t, reply("m1", c.highWater-4_444, 4_444))
			res := ccctx(t, append([]string{"-plain", "-transcript", path}, c.args...)...)
			lines := strings.SplitN(res.stdout, "\n", 3)
			if res.code != c.code || len(lines) != 3 || lines[0] != strconv.Itoa(c.highWater) ||
				lines[1] != c.limits || !strings.HasPrefix(lines[2], "meters: cache age ") {
				t.Errorf("exit %d, stdout:\n%s\nwant exit %d, then %d, then %q, then the meters",
					res.code, res.stdout, c.code, c.highWater, c.limits)
			}
		})
	}
}
