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
	return ccctxWithUsageFile(t, filepath.Join("testdata", "claude_usage_limits.json"), args...)
}

func ccctxWithUsageFile(t *testing.T, usageFile string, args ...string) result {
	t.Helper()
	t.Setenv("CCCTX_USAGE_FILE", usageFile)
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"-sensor-dir", t.TempDir(), "-projects", t.TempDir()}, args...), &stdout, &stderr)
	return result{code, stdout.String(), stderr.String()}
}

// writeSession writes a transcript of the given entries and returns its path.
func writeSession(t *testing.T, entries ...string) string {
	t.Helper()
	return writeTranscript(t, strings.Join(entries, "\n")+"\n")
}

func writeTranscript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const compaction = `{"type":"user","isCompactSummary":true,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"summary"}}`

const noUsage = `{"type":"assistant","version":"2.1.0","timestamp":"2026-01-01T00:00:00Z","message":{"id":"mx","model":"claude-test"}}`

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

// sidechain marks an entry as a subagent's.
func sidechain(entry string) string {
	return `{"isSidechain":true,` + strings.TrimPrefix(entry, "{")
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
	wantKeys := "session_id transcript cc_version count_known current_tokens high_water_tokens model turns compactions per_model_peak " +
		"window_tokens window_source usable_tokens used_pct checkpoint_line_tokens " +
		"left_to_checkpoint_tokens left_to_compaction_tokens over_checkpoint_line meters"
	if got := strings.Join(keys, " "); got != wantKeys {
		t.Errorf("keys:\n got %s\nwant %s", got, wantKeys)
	}
	checkValues(t, values, map[string]string{
		"count_known":               "true",
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

func TestRunSyntheticLast(t *testing.T) {
	synthetic := strings.Replace(assistant("m2", 0, 0, 0, 0), "claude-test", "<synthetic>", 1)
	path := writeSession(t, reply("m1", 500_000, 3_010), synthetic)
	res := ccctx(t, "-window", "1000000", "-transcript", path)
	if res.code != exitOK {
		t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
	}
	_, values := decodeReport(t, res.stdout)
	checkValues(t, values, map[string]string{
		"count_known":       "true",
		"current_tokens":    "503010",
		"high_water_tokens": "503010",
		"model":             "claude-test",
		"turns":             "1",
	})
}

// A subagent that compacts empties its own context, not the main thread's: the main
// turn before it still stands, and no compaction is counted.
func TestRunSidechainCompaction(t *testing.T) {
	path := writeSession(t,
		reply("m1", 948_000, 3_000),
		sidechain(compaction),
		sidechain(reply("s1", 20_000, 500)),
		sidechain(reply("s2", 21_000, 400)),
	)
	res := ccctx(t, "-window", "1000000", "-transcript", path)
	if res.code != exitOver {
		t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOver, res.stderr)
	}
	_, values := decodeReport(t, res.stdout)
	checkValues(t, values, map[string]string{
		"current_tokens":    "951000",
		"high_water_tokens": "951000",
		"turns":             "1",
		"compactions":       "0",
	})
}

// Right after a compaction, before the next reply, the transcript holds no count of
// the emptied window. The peak from before the compaction must not ask for a checkpoint.
func TestRunAfterCompaction(t *testing.T) {
	path := writeSession(t, reply("m1", 948_000, 3_000), compaction)

	res := ccctx(t, "-window", "1000000", "-transcript", path)
	if res.code != exitOK {
		t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
	}
	_, values := decodeReport(t, res.stdout)
	checkValues(t, values, map[string]string{
		"count_known":               "false",
		"current_tokens":            "0",
		"high_water_tokens":         "0",
		"turns":                     "0",
		"compactions":               "1",
		"left_to_checkpoint_tokens": "936000",
		"over_checkpoint_line":      "false",
	})
	checkInvariant(t, values)

	res = ccctx(t, "-plain", "-window", "1000000", "-transcript", path)
	lines := strings.SplitN(res.stdout, "\n", 3)
	if res.code != exitOK || len(lines) != 3 || lines[0] != "0" || !strings.Contains(lines[1], "unknown until the next reply") {
		t.Errorf("-plain: exit %d, stdout:\n%s\nwant exit %d, then 0, then a line saying the count is unknown until the next reply",
			res.code, res.stdout, exitOK)
	}
}

// A transcript can open with a compaction. Until the first reply after it the count is
// unknown, whatever else stands in the file that cannot be counted.
func TestRunOpensWithCompaction(t *testing.T) {
	synthetic := strings.Replace(assistant("m2", 0, 0, 0, 0), "claude-test", "<synthetic>", 1)
	cases := []struct {
		name    string
		entries []string
	}{
		{"the compaction alone", []string{compaction}},
		{"lines that are not replies, then the compaction", []string{
			`{"type":"custom-title","customTitle":"t"}`,
			`{"type":"system","timestamp":"2026-01-01T00:00:00Z","content":"c"}`,
			compaction,
		}},
		{"a user entry after it", []string{compaction, `{"type":"user","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":"hi"}}`}},
		{"a subagent reply after it", []string{compaction, sidechain(reply("s1", 20_000, 500))}},
		{"a placeholder entry after it", []string{compaction, synthetic}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ccctx(t, "-window", "1000000", "-transcript", writeSession(t, c.entries...))
			if res.code != exitOK {
				t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
			}
			_, values := decodeReport(t, res.stdout)
			checkValues(t, values, map[string]string{
				"count_known":       "false",
				"current_tokens":    "0",
				"high_water_tokens": "0",
				"turns":             "0",
				"compactions":       "1",
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
	wantKeys := "session_id transcript cc_version count_known current_tokens high_water_tokens model turns compactions per_model_peak meters"
	if got := strings.Join(keys, " "); got != wantKeys {
		t.Errorf("keys:\n got %s\nwant %s", got, wantKeys)
	}

	res = ccctx(t, "-transcript", path)
	if res.code != exitDrift || !strings.Contains(res.stderr, "no context window size available") {
		t.Errorf("no window: exit %d, stderr:\n%s\nwant exit %d naming the missing window size", res.code, res.stderr, exitDrift)
	}
}

func TestRunWindowSource(t *testing.T) {
	path := writeSession(t, reply("m1", 500_000, 2_000))
	sensorDir := func(t *testing.T, session string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, session+".json"), []byte(`{"window":1000000}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	res := ccctx(t, "-sensor-dir", sensorDir(t, "session"), "-transcript", path)
	if res.code != exitOK {
		t.Fatalf("own sensor file: exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
	}
	_, values := decodeReport(t, res.stdout)
	checkValues(t, values, map[string]string{"window_tokens": "1000000", "window_source": "sensor"})

	res = ccctx(t, "-sensor-dir", sensorDir(t, "another-session"), "-transcript", path)
	first, _, _ := strings.Cut(res.stderr, "\n")
	if res.code != exitDrift || res.stdout != "" || !strings.Contains(first, "-window <tokens>") {
		t.Errorf("another session's sensor file: exit %d, stdout %q, stderr:\n%s\nwant exit %d, no stdout, a first line naming -window <tokens>",
			res.code, res.stdout, res.stderr, exitDrift)
	}
}

// A caller may show only the first line of stderr. Without a window size that line names
// the way out, whatever else there is to warn about, and the warnings follow it.
func TestRunNoWindowFirstLine(t *testing.T) {
	const id = "22222222-2222-2222-2222-222222222222"
	projects := t.TempDir()
	for _, slug := range []string{"-Users-someone-project", "-home-someone-project"} {
		dir := filepath.Join(projects, slug)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(reply("m1", 500_000, 2_000)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sensorDir := func(body string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	path := writeSession(t, reply("m1", 500_000, 2_000))
	cases := []struct {
		name, warning string
		args          []string
	}{
		{"the session's sensor file is not JSON", "not valid JSON", []string{"-sensor-dir", sensorDir(`{"window":`), "-transcript", path}},
		{"the session's sensor file says 0", `no usable "window"`, []string{"-sensor-dir", sensorDir(`{"window":0}`), "-transcript", path}},
		{"an earlier unparseable line", "unparseable", []string{"-transcript", writeSession(t, "{this is not json}", reply("m1", 500_000, 2_000))}},
		{"two transcripts share the session id", "2 transcripts share session", []string{"-projects", projects, id}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ccctx(t, c.args...)
			first, rest, _ := strings.Cut(res.stderr, "\n")
			if res.code != exitDrift || res.stdout != "" || !strings.Contains(first, "-window <tokens>") || !strings.Contains(rest, c.warning) {
				t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant exit %d, no stdout, a first line naming -window <tokens>, then a warning containing %q",
					res.code, res.stdout, res.stderr, exitDrift, c.warning)
			}
		})
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
		{"negative window", []string{"-window", "-5", "-transcript", path}, exitFail, []string{"-window (-5)"}},
		{"negative checkpoint room with no window to judge", []string{"-no-threshold", "-checkpoint-room", "-1", "-transcript", path}, exitFail, []string{"-checkpoint-room (-1)"}},
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

// The flag package stops at the first argument that is not a flag. A session is judged
// on the window a flag gives before its id; a flag after the id is refused, not dropped.
func TestRunFlagAfterSessionID(t *testing.T) {
	const id = "33333333-2222-3333-4444-555555555555"
	projects := t.TempDir()
	dir := filepath.Join(projects, "-project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(reply("m1", 147_000, 3_000)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := ccctx(t, "-projects", projects, "-window", "200000", id)
	if res.code != exitOver {
		t.Errorf("flag before the id: exit %d, want %d; stderr:\n%s", res.code, exitOver, res.stderr)
	}

	for _, extra := range [][]string{{"-window", "200000"}, {"-plain"}} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			res := ccctx(t, append([]string{"-projects", projects, id}, extra...)...)
			quoted := fmt.Sprintf("%q", extra)
			if res.code != exitFail || res.stdout != "" || !strings.Contains(res.stderr, quoted) ||
				!strings.Contains(res.stderr, "Flags go before the session id") {
				t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant exit %d, no stdout, stderr quoting %s and saying flags go first",
					res.code, res.stdout, res.stderr, exitFail, quoted)
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

func TestRunMeters(t *testing.T) {
	const limits = `"limits": [{"kind": "weekly_all", "percent": 7, "resets_at": null}]`
	path := writeSession(t, reply("m1", 500_000, 2_000))
	cases := []struct{ name, cache, warning string }{
		{"five_hour null", `{"five_hour": null, ` + limits + `}`, ""},
		{"five_hour absent", `{` + limits + `}`, ""},
		{"five_hour without utilization", `{"five_hour": {"resets_at": null}, ` + limits + `}`, ".five_hour.utilization"},
		{"no limits", `{"five_hour": null}`, ".limits"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "usage.json")
			if err := os.WriteFile(cache, []byte(c.cache), 0o600); err != nil {
				t.Fatal(err)
			}
			res := ccctxWithUsageFile(t, cache, "-window", "1000000", "-transcript", path)
			if res.code != exitOK {
				t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
			}
			_, values := decodeReport(t, res.stdout)
			if c.warning != "" {
				if values["meters"] != nil || !strings.Contains(res.stderr, c.warning) {
					t.Errorf("meters %v, stderr:\n%s\nwant meters null and a warning naming %s", values["meters"], res.stderr, c.warning)
				}
				return
			}
			m, _ := values["meters"].(map[string]any)
			five, present := m["five_hour"]
			if !present || five != nil || fmt.Sprint(m["limits"]) != "[map[kind:weekly_all percent:7 resets_at:<nil>]]" || res.stderr != "" {
				t.Errorf("meters %v, stderr:\n%s\nwant five_hour null, the weekly_all limit, no warning", values["meters"], res.stderr)
			}

			res = ccctxWithUsageFile(t, cache, "-plain", "-window", "1000000", "-transcript", path)
			if strings.Contains(res.stdout, "five_hour") || !strings.Contains(res.stdout, "  weekly_all 7%") {
				t.Errorf("-plain stdout:\n%s\nwant the weekly_all line and no five_hour line", res.stdout)
			}
		})
	}
}

// Two selectors at once is a mistake to report by name, never a silent choice of one.
func TestRunSelectors(t *testing.T) {
	const id = "11111111-1111-1111-1111-111111111111"
	projects := t.TempDir()
	dir := filepath.Join(projects, "-project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(reply("m1", 500_000, 2_000)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeSession(t, reply("m1", 600_000, 2_000))
	cases := []struct {
		name   string
		args   []string
		stderr []string
	}{
		{"-transcript and a session id", []string{"-transcript", path, id}, []string{"-transcript", "session id"}},
		{"-latest and a session id", []string{"-latest", id}, []string{"-latest", "session id"}},
		{"-latest and -transcript", []string{"-latest", "-transcript", path}, []string{"-latest", "-transcript"}},
		{"none", nil, []string{"nothing to read"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ccctx(t, append([]string{"-window", "1000000", "-projects", projects}, c.args...)...)
			missing := []string{}
			for _, s := range c.stderr {
				if !strings.Contains(res.stderr, s) {
					missing = append(missing, s)
				}
			}
			if res.code != exitFail || res.stdout != "" || len(missing) > 0 {
				t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant exit %d, no stdout, stderr containing %q",
					res.code, res.stdout, res.stderr, exitFail, missing)
			}
		})
	}
}

func TestRunUsageDrift(t *testing.T) {
	renamed := strings.Replace(reply("m1", 500_000, 2_000), "cache_read_input_tokens", "cache_read_tokens", 1)
	cases := []struct {
		name, want string
		entries    []string
	}{
		{"a renamed key, a good reply after it", `"cache_read_input_tokens"`, []string{renamed, reply("m2", 600_000, 2_000)}},
		{"a count above 1e9", `"output_tokens"`, []string{reply("m1", 500_000, 1_000_000_001)}},
		{"a negative count", `"input_tokens"`, []string{assistant("m1", -5, 500_000, 1_000, 2_000)}},
		{"no usage after the last counted reply", "line 2", []string{reply("m1", 500_000, 2_000), noUsage}},
		{"an unparseable line after the last counted reply", "line 2", []string{reply("m1", 500_000, 2_000), "{this is not json}"}},
		{"no usage after a compaction, nothing counted since", "line 3", []string{reply("m1", 500_000, 2_000), compaction, noUsage}},
		{"no usage after an opening compaction", "line 2", []string{compaction, noUsage}},
		{"a renamed key after an opening compaction", `"cache_read_input_tokens"`, []string{compaction, renamed}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ccctx(t, "-window", "1000000", "-transcript", writeSession(t, c.entries...))
			if res.code != exitDrift || res.stdout != "" || !strings.Contains(res.stderr, c.want) {
				t.Errorf("exit %d, stdout %q, stderr:\n%s\nwant exit %d naming %s", res.code, res.stdout, res.stderr, exitDrift, c.want)
			}
		})
	}
}

// What came before the last counted reply, or before the last compaction, no longer
// decides the count, so it is no drift.
func TestRunUsageNoDrift(t *testing.T) {
	renamed := strings.Replace(reply("m1", 500_000, 2_000), "cache_read_input_tokens", "cache_read_tokens", 1)
	cases := []struct {
		name, current string
		entries       []string
	}{
		{"no usage before a counted reply", "502000", []string{noUsage, reply("m2", 500_000, 2_000)}},
		{"an unparseable line before a counted reply", "502000", []string{"{this is not json}", reply("m2", 500_000, 2_000)}},
		{"a renamed key before a compaction", "302000", []string{renamed, compaction, reply("m2", 300_000, 2_000)}},
		{"a count of exactly 1e9", "1000500000", []string{reply("m1", 500_000, 1_000_000_000)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := ccctx(t, "-no-threshold", "-transcript", writeSession(t, c.entries...))
			if res.code != exitOK {
				t.Fatalf("exit %d, want %d; stderr:\n%s", res.code, exitOK, res.stderr)
			}
			_, values := decodeReport(t, res.stdout)
			checkValues(t, values, map[string]string{"current_tokens": c.current})
		})
	}
}

func TestRunHalfWrittenLastLine(t *testing.T) {
	path := writeTranscript(t, reply("m1", 500_000, 2_000)+"\n"+`{"type":"assistant","message":{"id":"m2","usa`)
	res := ccctx(t, "-window", "1000000", "-transcript", path)
	if res.code != exitOK || res.stderr != "" {
		t.Fatalf("exit %d, stderr:\n%s\nwant exit %d and an empty stderr", res.code, res.stderr, exitOK)
	}
	_, values := decodeReport(t, res.stdout)
	checkValues(t, values, map[string]string{"current_tokens": "502000"})
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
