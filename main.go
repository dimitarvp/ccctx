// ccctx reports how full a Claude Code conversation's context window is.
//
// The number comes from the session transcript: each assistant turn records the
// usage of the prompt Claude Code sent, so context occupancy is
// input_tokens + cache_read_input_tokens + cache_creation_input_tokens.
//
// Two things make a naive "read the last turn" wrong:
//
//   - A forced model switch changes the accounting mid-conversation. The same
//     conversation reported 215k on one model and 119k on another with no content
//     lost, so the last turn alone understates occupancy. We take a high-water mark.
//   - Compaction genuinely empties the window (~166k to ~25k observed). The
//     high-water mark resets at every isCompactSummary entry, or it would report a
//     peak that no longer exists.
//
// The window size is not in the transcript. Claude Code only reports it on the
// statusline's stdin, so -window takes it from a sensor file the statusline writes;
// without one there is no threshold to judge and ccctx exits DRIFT.
//
// The transcript is Claude Code's private format and its own docs warn it changes
// between versions. Every read is therefore checked against what the format is
// expected to contain, and anything unrecognized exits DRIFT naming the exact JSON
// path that disappeared — a renamed field must never read as an empty context.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Exit codes. Anything other than OK or OVER means the answer is unusable.
const (
	exitOK    = 0  // under threshold
	exitFail  = 1  // bad invocation, unreadable file
	exitDrift = 2  // transcript read but did not contain what it must
	exitOver  = 10 // at or over threshold
)

// maxLine caps a transcript line. Lines hold whole tool results and run big; the
// bufio default of 64KB truncates them mid-JSON.
const maxLine = 64 << 20

// syntheticModel marks Claude Code's own placeholder entries rather than a model reply.
const syntheticModel = "<synthetic>"

type entry struct {
	Type             string `json:"type"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Timestamp        string `json:"timestamp"`
	Version          string `json:"version"`
	Message          struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens         int `json:"input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			OutputTokens        int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type turn struct {
	ctx   int
	model string
	ts    string
}

// stats records what the scan actually saw, so a disappointing result can be
// explained instead of guessed at.
type stats struct {
	lines            int
	parseErrors      int
	firstParseErrNo  int
	firstParseErrMsg string
	assistant        int
	assistantNoUsage int
	sidechain        int
	synthetic        int
	usable           int
	zeroCtx          int
	compactions      int
	ccVersion        string
}

type sensor struct {
	Window int `json:"window"`
}

type report struct {
	SessionID    string         `json:"session_id"`
	Transcript   string         `json:"transcript"`
	CCVersion    string         `json:"cc_version,omitempty"`
	Current      int            `json:"current_tokens"`
	HighWater    int            `json:"high_water_tokens"`
	Model        string         `json:"model"`
	Turns        int            `json:"turns"`
	Compactions  int            `json:"compactions"`
	PerModelPeak map[string]int `json:"per_model_peak"`
	Window       int            `json:"window_tokens,omitempty"`
	WindowSource string         `json:"window_source,omitempty"`
	UsedPct      float64        `json:"used_pct,omitempty"`
	Threshold    int            `json:"threshold_tokens,omitempty"`
	Over         bool           `json:"over_threshold"`
}

func main() {
	var (
		projects   = flag.String("projects", filepath.Join(home(), ".claude", "projects"), "Claude Code projects directory")
		transcript = flag.String("transcript", "", "explicit transcript path (skips session lookup)")
		latest     = flag.Bool("latest", false, "use the most recently modified transcript")
		window     = flag.Int("window", 0, "context window size in tokens (0 = read sensor file)")
		sensorDir  = flag.String("sensor-dir", filepath.Join(home(), ".cache", "claude_ctx"), "directory of statusline-written sensor files")
		frac       = flag.Float64("threshold", 0.96, "fraction of the window that counts as over")
		plain      = flag.Bool("plain", false, "print one token count instead of JSON")
		noThresh   = flag.Bool("no-threshold", false, "report occupancy only; do not require a window size")
	)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: ccctx [flags] [session-uuid]\n\n"+
			"Reports context occupancy for a Claude Code conversation.\n\n"+
			"Exit codes:\n"+
			"  0   under threshold\n"+
			"  10  at or over threshold\n"+
			"  2   DRIFT: the transcript did not contain what it must (see stderr)\n"+
			"  1   bad invocation or unreadable file\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *frac <= 0 || *frac > 1 {
		failf("-threshold must be between 0 and 1, got %v", *frac)
	}

	path, sessionID, err := locate(*transcript, *projects, flag.Arg(0), *latest)
	if err != nil {
		failf("%v", err)
	}

	turns, st, err := scan(path)
	if err != nil {
		failf("%v", err)
	}
	if msg := verify(turns, st, path); msg != "" {
		drift(msg)
	}
	warn(st, path)

	rep := summarize(turns)
	rep.SessionID, rep.Transcript = sessionID, path
	rep.Compactions, rep.CCVersion = st.compactions, st.ccVersion

	rep.Window, rep.WindowSource = resolveWindow(*window, *sensorDir, sessionID)
	switch {
	case rep.Window > 0:
		rep.UsedPct = float64(rep.HighWater) / float64(rep.Window) * 100
		rep.Threshold = int(float64(rep.Window) * *frac)
		rep.Over = rep.HighWater >= rep.Threshold
	case !*noThresh:
		drift(fmt.Sprintf("no context window size available, so %d tokens cannot be judged against a threshold.\n"+
			"  Claude Code reveals the window size ONLY on the statusline's stdin (.context_window.context_window_size);\n"+
			"  it is never written to the transcript.\n"+
			"  Fix by either:\n"+
			"    - having the statusline write %s/<session-id>.json containing {\"window\": <tokens>}\n"+
			"    - passing -window <tokens> explicitly\n"+
			"    - passing -no-threshold to report occupancy without judging it",
			rep.HighWater, *sensorDir))
	}

	if *plain {
		fmt.Println(rep.HighWater)
	} else {
		out, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			failf("cannot render report as JSON: %v", err)
		}
		fmt.Println(string(out))
	}
	if rep.Over {
		os.Exit(exitOver)
	}
}

// locate resolves which transcript to read. An explicit path wins, then a session
// uuid searched across every project directory, then the newest transcript on disk.
func locate(explicit, projects, uuid string, latest bool) (path, sessionID string, err error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", "", fmt.Errorf("cannot read transcript %s: %v", explicit, err)
		}
		return explicit, strings.TrimSuffix(filepath.Base(explicit), ".jsonl"), nil
	}
	if uuid != "" {
		matches, _ := filepath.Glob(filepath.Join(projects, "*", uuid+".jsonl"))
		if len(matches) == 0 {
			return "", "", fmt.Errorf("no transcript named %s.jsonl under %s\n"+
				"  Claude Code stores one transcript per session at <projects>/<project-slug>/<session-uuid>.jsonl.\n"+
				"  Check the uuid, or pass -transcript with a full path", uuid, projects)
		}
		// Syncing sessions between machines leaves the same uuid under several project
		// slugs (a macOS "-Users-dimi-…" beside a Linux "-home-dimi-…"). Picking one
		// silently would serve a stale copy the moment they diverge, so say so.
		if len(matches) > 1 {
			newest := newestOf(matches)
			fmt.Fprintf(os.Stderr, "ccctx warning: %d transcripts share session %s; reading the most recently modified one:\n", len(matches), uuid)
			for _, m := range matches {
				mark := "  "
				if m == newest {
					mark = "->"
				}
				fmt.Fprintf(os.Stderr, "  %s %s%s\n", mark, m, modSuffix(m))
			}
			fmt.Fprintf(os.Stderr, "  Pass -transcript <path> to choose deliberately.\n")
			return newest, uuid, nil
		}
		return matches[0], uuid, nil
	}
	if !latest {
		return "", "", fmt.Errorf("nothing to read: pass a session uuid, -transcript <path>, or -latest")
	}
	matches, _ := filepath.Glob(filepath.Join(projects, "*", "*.jsonl"))
	if len(matches) == 0 {
		return "", "", fmt.Errorf("no transcripts found under %s\n"+
			"  Either that is the wrong directory (override with -projects) or Claude Code now stores sessions elsewhere", projects)
	}
	newest := newestOf(matches)
	if newest == "" {
		return "", "", fmt.Errorf("found %d transcripts under %s but could not stat any of them", len(matches), projects)
	}
	return newest, strings.TrimSuffix(filepath.Base(newest), ".jsonl"), nil
}

// newestOf returns the most recently modified path, or "" if none can be stat'ed.
func newestOf(paths []string) string {
	newest, newestMod := "", int64(-1)
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ccctx warning: cannot stat %s: %v\n", p, err)
			continue
		}
		if mod := fi.ModTime().UnixNano(); mod > newestMod {
			newest, newestMod = p, mod
		}
	}
	return newest
}

func modSuffix(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("  (modified %s, %d bytes)", fi.ModTime().Format("2006-01-02 15:04:05"), fi.Size())
}

// scan walks the transcript and returns the main-thread turns since the last
// compaction, in order, alongside a tally of everything it saw. Streaming writes the
// same message id repeatedly with growing usage, so a turn keeps its largest reading.
func scan(path string) ([]turn, stats, error) {
	var st stats

	f, err := os.Open(path)
	if err != nil {
		return nil, st, fmt.Errorf("cannot open transcript: %v", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)

	byID := map[string]turn{}
	order := []string{}

	for sc.Scan() {
		st.lines++
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			st.parseErrors++
			if st.firstParseErrNo == 0 {
				st.firstParseErrNo, st.firstParseErrMsg = st.lines, err.Error()
			}
			continue
		}
		if e.Version != "" {
			st.ccVersion = e.Version // keep the last: that is the version still writing
		}
		if e.IsCompactSummary {
			st.compactions++
			byID, order = map[string]turn{}, nil // the window really did empty here
			continue
		}
		if e.Type != "assistant" {
			continue
		}
		st.assistant++
		if e.IsSidechain {
			st.sidechain++
			continue
		}
		// Claude Code writes placeholder entries (model "<synthetic>") for refusals and
		// API errors. They carry an all-zero usage block and describe no prompt, so one
		// landing last would report an empty context.
		if e.Message.Model == syntheticModel {
			st.synthetic++
			continue
		}
		if e.Message.Usage == nil {
			st.assistantNoUsage++
			continue
		}
		st.usable++

		u := e.Message.Usage
		t := turn{
			ctx:   u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens,
			model: e.Message.Model,
			ts:    e.Timestamp,
		}
		if t.ctx == 0 {
			st.zeroCtx++
		}
		id := e.Message.ID
		if id == "" {
			id = e.Timestamp
		}
		if prev, seen := byID[id]; !seen {
			order = append(order, id)
			byID[id] = t
		} else if t.ctx > prev.ctx {
			byID[id] = t
		}
	}
	if err := sc.Err(); err != nil {
		if err == bufio.ErrTooLong {
			return nil, st, fmt.Errorf("transcript line %d exceeds ccctx's %d MB line cap.\n"+
				"  Transcript lines hold whole tool results, so a huge one is plausible; raise maxLine in main.go and rebuild",
				st.lines+1, maxLine>>20)
		}
		return nil, st, fmt.Errorf("failed reading transcript at line %d: %v", st.lines, err)
	}

	turns := make([]turn, 0, len(order))
	for _, id := range order {
		turns = append(turns, byID[id])
	}
	return turns, st, nil
}

// verify decides whether the scan found what the format must contain. It returns a
// message naming the exact JSON path that is missing, because the failure this guards
// against is a renamed field quietly reading as an empty context window.
func verify(turns []turn, st stats, path string) string {
	where := fmt.Sprintf("  transcript: %s\n  Claude Code version in transcript: %s", path, orUnknown(st.ccVersion))

	switch {
	case st.lines == 0:
		return fmt.Sprintf("transcript is empty (0 lines).\n%s", where)

	case st.parseErrors == st.lines:
		return fmt.Sprintf("every one of the %d lines failed to parse as JSON.\n"+
			"  first failure was line %d: %s\n"+
			"  The transcript is expected to be JSON Lines, one object per line — that no longer holds.\n%s",
			st.lines, st.firstParseErrNo, st.firstParseErrMsg, where)

	case st.assistant == 0:
		return fmt.Sprintf("parsed %d lines but not one had \"type\":\"assistant\".\n"+
			"  Context occupancy is only recorded on assistant entries, so either this session has no model replies\n"+
			"  or the entry type was renamed.\n%s", st.lines, where)

	case st.usable == 0 && st.sidechain > 0 && st.assistant == st.sidechain:
		return fmt.Sprintf("all %d assistant entries are sidechain (subagent) turns, so none describe the main conversation's context.\n"+
			"  If this is a subagent-only transcript that is expected; otherwise \"isSidechain\" may have changed meaning.\n%s",
			st.sidechain, where)

	case st.usable == 0:
		return fmt.Sprintf("found %d assistant entries but none usable (%d lacked \"message\".\"usage\", %d sidechain, %d synthetic placeholders).\n"+
			"  The usage block is where token counts live; without it there is no context number to report.\n%s",
			st.assistant, st.assistantNoUsage, st.sidechain, st.synthetic, where)

	case st.zeroCtx == st.usable:
		return fmt.Sprintf("all %d usable turns computed a context of 0 tokens.\n"+
			"  Expected integer fields inside \"message\".\"usage\": \"input_tokens\", \"cache_read_input_tokens\", \"cache_creation_input_tokens\".\n"+
			"  A usage block that exists but sums to zero means those keys were renamed — treating this as 0 would silently\n"+
			"  report an empty context window forever, so it is a hard error instead.\n%s", st.usable, where)

	case len(turns) == 0:
		return fmt.Sprintf("no turns survived after the last of %d compaction boundaries.\n"+
			"  That is expected only if the session compacted and has not replied since.\n%s", st.compactions, where)
	}
	return ""
}

// warn reports non-fatal oddities. They do not invalidate the number but they are the
// early signal that the format is moving.
func warn(st stats, path string) {
	if st.parseErrors > 0 {
		fmt.Fprintf(os.Stderr, "ccctx warning: skipped %d of %d unparseable lines in %s\n"+
			"  first was line %d: %s\n",
			st.parseErrors, st.lines, path, st.firstParseErrNo, st.firstParseErrMsg)
	}
	if st.zeroCtx > 0 && st.zeroCtx < st.usable {
		fmt.Fprintf(os.Stderr, "ccctx warning: %d of %d turns computed 0 tokens of context; expected every assistant turn to report usage\n",
			st.zeroCtx, st.usable)
	}
	if st.usable > 0 && st.assistantNoUsage > st.usable {
		fmt.Fprintf(os.Stderr, "ccctx warning: %d assistant entries lacked \"message\".\"usage\" versus %d that had it\n",
			st.assistantNoUsage, st.usable)
	}
}

// summarize takes the high-water mark across turns. A forced model switch only ever
// reads lower, so the peak recovers the true occupancy and errs toward triggering early.
func summarize(turns []turn) report {
	rep := report{Turns: len(turns), PerModelPeak: map[string]int{}}
	for _, t := range turns {
		if t.ctx > rep.HighWater {
			rep.HighWater = t.ctx
		}
		if t.model != "" && t.ctx > rep.PerModelPeak[t.model] {
			rep.PerModelPeak[t.model] = t.ctx
		}
	}
	last := turns[len(turns)-1]
	rep.Current, rep.Model = last.ctx, last.model
	return rep
}

// resolveWindow finds the context window size, which the transcript never records.
// The statusline receives it from Claude Code and writes it to a sensor file, so the
// size tracks whatever the model actually offers instead of a hardcoded constant.
func resolveWindow(explicit int, sensorDir, sessionID string) (int, string) {
	if explicit > 0 {
		return explicit, "flag"
	}
	own := filepath.Join(sensorDir, sessionID+".json")
	if w, ok := readSensor(own, true); ok {
		return w, "sensor"
	}
	globbed, _ := filepath.Glob(filepath.Join(sensorDir, "*.json"))
	sort.Strings(globbed)
	for _, c := range globbed {
		if c == own {
			continue
		}
		if w, ok := readSensor(c, false); ok {
			fmt.Fprintf(os.Stderr, "ccctx warning: no sensor file for this session (%s); using window size from %s instead\n", own, c)
			return w, "sensor(other-session)"
		}
	}
	return 0, ""
}

// readSensor reads one sensor file. A sensor that exists but cannot be used is
// reported loudly, since silently ignoring it is how a threshold stops being checked.
func readSensor(path string, complain bool) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		if complain && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "ccctx warning: cannot read sensor file %s: %v\n", path, err)
		}
		return 0, false
	}
	var s sensor
	if err := json.Unmarshal(b, &s); err != nil {
		if complain {
			fmt.Fprintf(os.Stderr, "ccctx warning: sensor file %s is not valid JSON: %v\n", path, err)
		}
		return 0, false
	}
	if s.Window <= 0 {
		if complain {
			fmt.Fprintf(os.Stderr, "ccctx warning: sensor file %s has no usable \"window\" value (got %d)\n", path, s.Window)
		}
		return 0, false
	}
	return s.Window, true
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown (no \"version\" field found)"
	}
	return s
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ccctx: %s\n", fmt.Sprintf(format, args...))
	os.Exit(exitFail)
}

// drift means the transcript no longer looks the way ccctx requires. It is separated
// from ordinary failure so a hook can tell "this tool needs fixing" from "bad input".
func drift(msg string) {
	fmt.Fprintf(os.Stderr, "ccctx: FORMAT DRIFT — %s\n\n"+
		"ccctx reads Claude Code's private transcript format, which its own documentation says changes between versions.\n"+
		"This message means the format moved. Re-check the JSON shape and update main.go.\n", msg)
	os.Exit(exitDrift)
}
