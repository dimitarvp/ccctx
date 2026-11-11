// ccctx reports how full a Claude Code conversation's context window is.
//
// The number comes from the session transcript: each assistant turn records the
// usage of the prompt Claude Code sent and of the reply, so context occupancy is
// input_tokens + cache_read_input_tokens + cache_creation_input_tokens + output_tokens.
// The reply counts because the next prompt carries it, and Claude Code's own
// compaction check adds it too.
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
// without one there are no limits to judge against and ccctx exits DRIFT.
//
// The window itself is never the limit. Claude Code keeps back a reserve for its own
// compaction and forces one at the window minus that reserve: 966,000 on a 1,000,000
// window. A checkpoint written before then needs room of its own, so the checkpoint
// line sits that room lower: 936,000. Both margins are token counts, the same on any
// window size, never fractions of it. At or past the checkpoint line ccctx exits OVER.
//
// The transcript is Claude Code's private format and its own docs warn it changes
// between versions. Every read is therefore checked against what the format is
// expected to contain, and anything unrecognized exits DRIFT naming the exact JSON
// path that disappeared — a renamed field must never read as an empty context.
//
// The report also carries the account's usage meters, read from the cache the
// statusline keeps of Anthropic's usage endpoint, so one command shows both how full
// the context is and how much of the usage limits is spent. The meters never change
// the exit code, which answers only whether the context reached the checkpoint line.
package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Exit codes. Anything other than OK or OVER means the answer is unusable.
const (
	exitOK    = 0  // under the checkpoint line
	exitFail  = 1  // bad invocation, unreadable file
	exitDrift = 2  // transcript read but did not contain what it must
	exitOver  = 10 // at or over the checkpoint line
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
	zeroPrompt       int
	zeroOutput       int
	compactions      int
	ccVersion        string
}

type sensor struct {
	Window int `json:"window"`
}

// judgment is what ccctx can say once it knows the window size. The report carries none
// when no window size is known.
type judgment struct {
	Window           int     `json:"window_tokens"`
	WindowSource     string  `json:"window_source"`
	Usable           int     `json:"usable_tokens"`             // window - compaction reserve: Claude Code forces a compaction here
	UsedPct          float64 `json:"used_pct"`                  // high water / usable * 100
	CheckpointLine   int     `json:"checkpoint_line_tokens"`    // usable - checkpoint room
	LeftToCheckpoint int     `json:"left_to_checkpoint_tokens"` // checkpoint line - high water; negative once past it
	LeftToCompaction int     `json:"left_to_compaction_tokens"` // usable - high water; negative once past it
	Over             bool    `json:"over_checkpoint_line"`      // high water >= checkpoint line
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
	*judgment
	Meters *meters `json:"meters"`
}

// usageCache is the part of the statusline's usage cache that the meters report: the
// payload of Anthropic's OAuth usage endpoint, stored as fetched. The numbers are
// pointers so a missing or renamed field is an error, never a reading of 0% used.
type usageCache struct {
	FiveHour *struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *string  `json:"resets_at"`
	} `json:"five_hour"`
	Limits []struct {
		Kind     string   `json:"kind"`
		Percent  *float64 `json:"percent"`
		ResetsAt *string  `json:"resets_at"`
	} `json:"limits"`
}

// meters is the report's usage block. AgeSeconds is the cache file's age: how long
// since the statusline last wrote it.
type meters struct {
	AgeSeconds int64   `json:"age_seconds"`
	FiveHour   meter   `json:"five_hour"`
	Limits     []meter `json:"limits"`
}

// meter is one usage window. ResetsAt stays null when the payload has no reset time.
type meter struct {
	Kind     string  `json:"kind,omitempty"` // empty for the five-hour window, which has its own key
	Percent  float64 `json:"percent"`
	ResetsAt *string `json:"resets_at"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole command. It returns the exit code and writes only to stdout and
// stderr, so a test can drive it the way a caller does.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ccctx", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		projects   = flags.String("projects", filepath.Join(home(), ".claude", "projects"), "Claude Code projects directory")
		transcript = flags.String("transcript", "", "explicit transcript path (skips session lookup)")
		latest     = flags.Bool("latest", false, "use the most recently modified transcript")
		window     = flags.Int("window", 0, "context window size in tokens (0 = read sensor file)")
		sensorDir  = flags.String("sensor-dir", filepath.Join(home(), ".cache", "claude_ctx"), "directory of statusline-written sensor files")
		plain      = flags.Bool("plain", false, "print the token count, the limits, then the usage meters, as text instead of JSON")
		noThresh   = flags.Bool("no-threshold", false, "report occupancy only; do not require a window size")
		reserve    = flags.Int("compaction-reserve", 34000, "tokens Claude Code keeps back for its forced compaction, which comes at the window minus this")
		room       = flags.Int("checkpoint-room", 30000, "tokens kept free below the forced compaction for the checkpoint itself; the checkpoint line sits this far below it")
	)
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, "usage: ccctx [flags] [session-uuid]\n\n"+
			"Reports context occupancy for a Claude Code conversation, how far it is from the\n"+
			"checkpoint line and from the forced compaction, and the usage meters from\n"+
			"~/.cache/claude_usage_limits.json (override with CCCTX_USAGE_FILE).\n\n"+
			"Exit codes:\n"+
			"  0   under the checkpoint line\n"+
			"  10  at or over the checkpoint line\n"+
			"  2   DRIFT: the transcript did not contain what it must (see stderr)\n"+
			"  1   bad invocation or unreadable file\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitFail // the flag package has already printed the error and the usage
	}
	if *window < 0 || *reserve < 0 || *room < 0 {
		failf(stderr, "-window (%d), -compaction-reserve (%d) and -checkpoint-room (%d) are token counts and cannot be negative",
			*window, *reserve, *room)
		return exitFail
	}

	path, sessionID, err := locate(stderr, *transcript, *projects, flags.Arg(0), *latest)
	if err != nil {
		failf(stderr, "%v", err)
		return exitFail
	}

	turns, st, err := scan(path)
	if err != nil {
		failf(stderr, "%v", err)
		return exitFail
	}
	if msg := verify(turns, &st, path); msg != "" {
		drift(stderr, msg)
		return exitDrift
	}
	warn(stderr, &st, path)

	rep := summarize(turns)
	rep.SessionID, rep.Transcript = sessionID, path
	rep.Compactions, rep.CCVersion = st.compactions, st.ccVersion

	windowSize, source := resolveWindow(stderr, *window, *sensorDir, sessionID)
	switch {
	case windowSize > 0:
		// Subtracting rather than adding, so huge flag values cannot overflow into a pass.
		if windowSize-*reserve <= *room {
			failf(stderr, "a window of %d tokens must be larger than the compaction reserve (%d) plus the checkpoint room (%d)",
				windowSize, *reserve, *room)
			return exitFail
		}
		j := judge(rep.HighWater, windowSize, *reserve, *room)
		j.WindowSource = source
		rep.judgment = &j
	case !*noThresh:
		drift(stderr, fmt.Sprintf("no context window size available, so %d tokens cannot be judged against the checkpoint line.\n"+
			"  Claude Code reveals the window size ONLY on the statusline's stdin (.context_window.context_window_size);\n"+
			"  it is never written to the transcript.\n"+
			"  Fix by either:\n"+
			"    - having the statusline write %s/<session-id>.json containing {\"window\": <tokens>}\n"+
			"    - passing -window <tokens> explicitly\n"+
			"    - passing -no-threshold to report occupancy without judging it",
			rep.HighWater, *sensorDir))
		return exitDrift
	}

	// Unlike the context number, the meters are optional: a cache that cannot be read
	// warns and reports null.
	usagePath := cmp.Or(os.Getenv("CCCTX_USAGE_FILE"), filepath.Join(home(), ".cache", "claude_usage_limits.json"))
	var metersErr error
	rep.Meters, metersErr = readMeters(usagePath, time.Now())
	if err := writeReport(stdout, stderr, &rep, *plain, metersErr); err != nil {
		failf(stderr, "cannot render report as JSON: %v", err)
		return exitFail
	}
	if rep.judgment != nil && rep.Over {
		return exitOver
	}
	return exitOK
}

// locate resolves which transcript to read. An explicit path wins, then a session
// uuid searched across every project directory, then the newest transcript on disk.
func locate(stderr io.Writer, explicit, projects, uuid string, latest bool) (path, sessionID string, err error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", "", fmt.Errorf("cannot read transcript %s: %w", explicit, err)
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
		// slugs (a macOS "-Users-<name>-…" beside a Linux "-home-<name>-…"). Picking one
		// silently would serve a stale copy the moment they diverge, so say so.
		if len(matches) > 1 {
			newest := newestOf(stderr, matches)
			_, _ = fmt.Fprintf(stderr, "ccctx warning: %d transcripts share session %s; reading the most recently modified one:\n", len(matches), uuid)
			for _, m := range matches {
				mark := "  "
				if m == newest {
					mark = "->"
				}
				_, _ = fmt.Fprintf(stderr, "  %s %s%s\n", mark, m, modSuffix(m))
			}
			_, _ = fmt.Fprintf(stderr, "  Pass -transcript <path> to choose deliberately.\n")
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
	newest := newestOf(stderr, matches)
	if newest == "" {
		return "", "", fmt.Errorf("found %d transcripts under %s but could not stat any of them", len(matches), projects)
	}
	return newest, strings.TrimSuffix(filepath.Base(newest), ".jsonl"), nil
}

// newestOf returns the most recently modified path, or "" if none can be stat'ed.
func newestOf(stderr io.Writer, paths []string) string {
	newest, newestMod := "", int64(-1)
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "ccctx warning: cannot stat %s: %v\n", p, err)
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
		return nil, st, fmt.Errorf("cannot open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)

	byID := map[string]turn{}
	order := []string{}

	for sc.Scan() {
		st.lines++
		line := sc.Bytes()
		if strings.TrimSpace(string(line)) == "" {
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
		prompt := u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
		if prompt == 0 {
			st.zeroPrompt++
		}
		if u.OutputTokens == 0 {
			st.zeroOutput++
		}
		t := turn{
			ctx:   prompt + u.OutputTokens,
			model: e.Message.Model,
			ts:    e.Timestamp,
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
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, st, fmt.Errorf("transcript line %d exceeds ccctx's %d MB line cap.\n"+
				"  Transcript lines hold whole tool results, so a huge one is plausible; raise maxLine in main.go and rebuild",
				st.lines+1, maxLine>>20)
		}
		return nil, st, fmt.Errorf("failed reading transcript at line %d: %w", st.lines, err)
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
func verify(turns []turn, st *stats, path string) string {
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

	case st.zeroPrompt == st.usable:
		return fmt.Sprintf("all %d usable turns reported a prompt of 0 tokens.\n"+
			"  Expected integer fields inside \"message\".\"usage\": \"input_tokens\", \"cache_read_input_tokens\", \"cache_creation_input_tokens\".\n"+
			"  A usage block that exists but sums to zero means those keys were renamed — treating this as 0 would silently\n"+
			"  report an empty context window forever, so it is a hard error instead.\n%s", st.usable, where)

	case st.zeroOutput == st.usable:
		return fmt.Sprintf("all %d usable turns reported 0 output tokens.\n"+
			"  Expected an integer field \"message\".\"usage\".\"output_tokens\": every reply has output, and it stays in the context.\n"+
			"  Zero in every turn means that key was renamed — treating this as 0 would silently report the context short\n"+
			"  by every reply's length, so it is a hard error instead.\n%s", st.usable, where)

	case len(turns) == 0:
		return fmt.Sprintf("no turns survived after the last of %d compaction boundaries.\n"+
			"  That is expected only if the session compacted and has not replied since.\n%s", st.compactions, where)
	}
	return ""
}

// warn reports non-fatal oddities. They do not invalidate the number but they are the
// early signal that the format is moving.
func warn(stderr io.Writer, st *stats, path string) {
	if st.parseErrors > 0 {
		_, _ = fmt.Fprintf(stderr, "ccctx warning: skipped %d of %d unparseable lines in %s\n"+
			"  first was line %d: %s\n",
			st.parseErrors, st.lines, path, st.firstParseErrNo, st.firstParseErrMsg)
	}
	if st.zeroPrompt > 0 && st.zeroPrompt < st.usable {
		_, _ = fmt.Fprintf(stderr, "ccctx warning: %d of %d turns reported a prompt of 0 tokens; expected every assistant turn to report usage\n",
			st.zeroPrompt, st.usable)
	}
	if st.usable > 0 && st.assistantNoUsage > st.usable {
		_, _ = fmt.Fprintf(stderr, "ccctx warning: %d assistant entries lacked \"message\".\"usage\" versus %d that had it\n",
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

// judge measures the high-water mark against the two limits. Both margins are fixed
// token counts, not fractions of the window: Claude Code keeps back the same reserve,
// and a checkpoint costs the same, on any window size.
func judge(highWater, window, reserve, room int) judgment {
	usable := window - reserve
	line := usable - room
	return judgment{
		Window:           window,
		Usable:           usable,
		UsedPct:          float64(highWater) / float64(usable) * 100,
		CheckpointLine:   line,
		LeftToCheckpoint: line - highWater,
		LeftToCompaction: usable - highWater,
		Over:             highWater >= line,
	}
}

// resolveWindow finds the context window size, which the transcript never records.
// The statusline receives it from Claude Code and writes it to a sensor file, so the
// size tracks whatever the model actually offers instead of a hardcoded constant.
func resolveWindow(stderr io.Writer, explicit int, sensorDir, sessionID string) (window int, source string) {
	if explicit > 0 {
		return explicit, "flag"
	}
	own := filepath.Join(sensorDir, sessionID+".json")
	if w, ok := readSensor(stderr, own, true); ok {
		return w, "sensor"
	}
	globbed, _ := filepath.Glob(filepath.Join(sensorDir, "*.json"))
	sort.Strings(globbed)
	for _, c := range globbed {
		if c == own {
			continue
		}
		if w, ok := readSensor(stderr, c, false); ok {
			_, _ = fmt.Fprintf(stderr, "ccctx warning: no sensor file for this session (%s); using window size from %s instead\n", own, c)
			return w, "sensor(other-session)"
		}
	}
	return 0, ""
}

// readSensor reads one sensor file. A sensor that exists but cannot be used is
// reported loudly, since silently ignoring it is how a limit stops being checked.
func readSensor(stderr io.Writer, path string, complain bool) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		if complain && !os.IsNotExist(err) {
			_, _ = fmt.Fprintf(stderr, "ccctx warning: cannot read sensor file %s: %v\n", path, err)
		}
		return 0, false
	}
	var s sensor
	if err := json.Unmarshal(b, &s); err != nil {
		if complain {
			_, _ = fmt.Fprintf(stderr, "ccctx warning: sensor file %s is not valid JSON: %v\n", path, err)
		}
		return 0, false
	}
	if s.Window <= 0 {
		if complain {
			_, _ = fmt.Fprintf(stderr, "ccctx warning: sensor file %s has no usable \"window\" value (got %d)\n", path, s.Window)
		}
		return 0, false
	}
	return s.Window, true
}

// readMeters reads the usage meters from the statusline's cache file and dates
// them against now.
func readMeters(path string, now time.Time) (*meters, error) {
	f, err := os.Open(path) //nolint:gosec // G703: CCCTX_USAGE_FILE names the user's own file; reading it is the override's job
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var c usageCache
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("cannot parse %s: %w", path, err)
	}
	if c.FiveHour == nil || c.FiveHour.Utilization == nil {
		return nil, fmt.Errorf("%s has no .five_hour.utilization", path)
	}
	if c.Limits == nil {
		return nil, fmt.Errorf("%s has no .limits", path)
	}
	m := &meters{
		AgeSeconds: int64(now.Sub(fi.ModTime()) / time.Second),
		FiveHour:   meter{Percent: *c.FiveHour.Utilization, ResetsAt: c.FiveHour.ResetsAt},
		Limits:     make([]meter, 0, len(c.Limits)),
	}
	for i, l := range c.Limits {
		switch {
		case l.Kind == "":
			return nil, fmt.Errorf("%s has no .limits[%d].kind", path, i)
		case l.Percent == nil:
			return nil, fmt.Errorf("%s has no .limits[%d].percent", path, i)
		}
		m.Limits = append(m.Limits, meter{Kind: l.Kind, Percent: *l.Percent, ResetsAt: l.ResetsAt})
	}
	return m, nil
}

// limitsText renders the judgment for -plain, on one line.
func limitsText(j *judgment) string {
	if j == nil {
		return "limits: null\n"
	}
	return fmt.Sprintf("limits: checkpoint line %d (%d left), forced compaction %d (%d left)\n",
		j.CheckpointLine, j.LeftToCheckpoint, j.Usable, j.LeftToCompaction)
}

// metersText renders the meters for -plain, one window per line.
func metersText(m *meters) string {
	if m == nil {
		return "meters: null\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "meters: cache age %ds\n", m.AgeSeconds)
	line := func(name string, w meter) {
		fmt.Fprintf(&b, "  %s %g%%", name, w.Percent)
		if w.ResetsAt != nil {
			fmt.Fprintf(&b, " resets %s", *w.ResetsAt)
		}
		b.WriteByte('\n')
	}
	line("five_hour", m.FiveHour)
	for _, l := range m.Limits {
		line(l.Kind, l)
	}
	return b.String()
}

// writeReport prints the report on stdout, then the meters warning, if any, on
// stderr. The warning goes last so a caller that merges the two streams (2>&1)
// reads the report first and whole: os.Stdout is not buffered, so the report has
// left the process before the warning is written.
func writeReport(stdout, stderr io.Writer, rep *report, plain bool, metersErr error) error {
	var out string
	if plain {
		out = fmt.Sprintf("%d\n%s%s", rep.HighWater, limitsText(rep.judgment), metersText(rep.Meters))
	} else {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		out = string(b) + "\n"
	}
	_, _ = io.WriteString(stdout, out)
	if metersErr != nil {
		_, _ = fmt.Fprintf(stderr, "ccctx warning: no usage meters: %v\n", metersErr)
	}
	return nil
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

func failf(stderr io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(stderr, "ccctx: %s\n", fmt.Sprintf(format, args...))
}

// drift means the transcript no longer looks the way ccctx requires. It is separated
// from ordinary failure so a hook can tell "this tool needs fixing" from "bad input".
func drift(stderr io.Writer, msg string) {
	_, _ = fmt.Fprintf(stderr, "ccctx: FORMAT DRIFT — %s\n\n"+
		"ccctx reads Claude Code's private transcript format, which its own documentation says changes between versions.\n"+
		"This message means the format moved. Re-check the JSON shape and update main.go.\n", msg)
}
