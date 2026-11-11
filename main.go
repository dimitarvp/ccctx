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
//     high-water mark resets at every isCompactSummary entry of the main thread, or
//     it would report a peak that no longer exists. Until the next reply the count
//     is unknown: count_known is false, the counts read 0 and the limits are judged
//     on 0.
//
// The window size is not in the transcript. Claude Code only reports it on the
// statusline's stdin, so ccctx reads it from the sensor file the statusline writes for
// the same session, or takes -window. Another session's file is never used: it may
// describe another model. Without a window size there are no limits to judge against
// and ccctx exits 2.
//
// The window itself is never the limit. Claude Code keeps back a reserve for its own
// compaction and forces one at the window minus that reserve: 966,000 on a 1,000,000
// window. A checkpoint written before then needs room of its own, so the checkpoint
// line sits that room lower: 936,000. Both margins are token counts, the same on any
// window size, never fractions of it. At or past the checkpoint line ccctx exits OVER.
//
// The transcript is Claude Code's private format and its own docs warn it changes
// between versions. Every read is therefore checked against what the format is
// expected to contain: a transcript with no line that is a JSON object, no assistant
// entry, no usage block, a prompt side of zero in every turn or an output of zero in
// every turn exits DRIFT naming the JSON path that disappeared. Those whole-file checks
// wait while the count is unknown right after a compaction. Since the last compaction,
// every usage block must carry all four counts, each an integer from 0 to
// 1,000,000,000, and no line that is not a JSON object, nor an assistant entry without
// usage, may come after the last counted reply; each breach exits DRIFT naming the key
// or the line. A last line with no newline after it is still being written and is left
// out. Anywhere in the transcript, the fields that sort an entry must hold their own
// JSON type: type, isSidechain, isCompactSummary, and a main-thread assistant entry's
// message and model. Any other value of an unexpected type matters only in an entry
// that is counted.
//
// The report also carries the account's usage meters, read from the cache the
// statusline keeps of Anthropic's usage endpoint, so one command shows both how full
// the context is and how much of the usage limits is spent. The meters never change
// the exit code, which answers only whether the context reached the checkpoint line.
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Exit codes. Anything other than OK or OVER means the answer is unusable.
const (
	exitOK    = 0  // under the checkpoint line
	exitFail  = 1  // bad invocation, unreadable file
	exitDrift = 2  // transcript read but did not contain what it must, or no window size known
	exitOver  = 10 // at or over the checkpoint line
)

// maxLine caps a transcript line. Lines hold whole tool results and run big; the
// bufio default of 64KB truncates them mid-JSON.
const maxLine = 64 << 20

// syntheticModel marks Claude Code's own placeholder entries rather than a model reply.
const syntheticModel = "<synthetic>"

// entry is what every transcript line is read for. The message stays raw until the
// entry turns out to be a main-thread assistant entry.
type entry struct {
	Type             string
	IsSidechain      bool
	IsCompactSummary bool
	Timestamp        string
	Version          string
	Message          json.RawMessage
}

// parseLine fails only when the line is not a JSON object. A field that decides how the
// entry is sorted but holds another JSON type comes back as mistyped. Each field is
// decoded on its own because after a value of the wrong type, encoding/json does not
// promise to fill the fields that follow it.
func parseLine(line []byte) (e entry, mistyped string, err error) {
	fields, err := object(line)
	if err != nil {
		return entry{}, "", err
	}
	mistyped = cmp.Or(
		typed(fields, "type", &e.Type),
		typed(fields, "isSidechain", &e.IsSidechain),
		typed(fields, "isCompactSummary", &e.IsCompactSummary),
	)
	field(fields, "timestamp", &e.Timestamp)
	field(fields, "version", &e.Version)
	e.Message = fields["message"]
	return e, mistyped, nil
}

func object(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	err := json.Unmarshal(raw, &fields)
	switch {
	case errors.As(err, new(*json.UnmarshalTypeError)), err == nil && fields == nil:
		return nil, errors.New("valid JSON of another type")
	case err != nil:
		return nil, err
	}
	return fields, nil
}

// field decodes one value. A value that is absent or of another type leaves dst empty.
func field(fields map[string]json.RawMessage, key string, dst any) {
	_ = json.Unmarshal(fields[key], dst)
}

// typed decodes a field that decides how an entry is sorted. An absent key leaves dst
// empty. A value of another JSON type, null included, is returned as found.
func typed(fields map[string]json.RawMessage, key string, dst any) string {
	raw, ok := fields[key]
	if ok && (string(raw) == "null" || json.Unmarshal(raw, dst) != nil) {
		return fmt.Sprintf("%q is %s", key, raw)
	}
	return ""
}

// maxCount bounds one usage count. No context window comes near it, so a count past
// it is a misread.
const maxCount = 1_000_000_000

func usageCounts(usage map[string]json.RawMessage) (prompt, output int, err error) {
	var n [4]int
	for i, key := range [...]string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens"} {
		raw, ok := usage[key]
		if !ok || string(raw) == "null" {
			return 0, 0, fmt.Errorf(`"message"."usage" has no %q`, key)
		}
		c, err := strconv.Atoi(string(raw))
		if err != nil || c < 0 || c > maxCount {
			return 0, 0, fmt.Errorf(`"message"."usage".%q is %s, not an integer from 0 to %d`, key, raw, maxCount)
		}
		n[i] = c
	}
	return n[0] + n[1] + n[2], n[3], nil
}

type turn struct {
	ctx   int
	model string
	ts    string
}

// stats records what the scan actually saw, so a disappointing result can be
// explained instead of guessed at.
type stats struct {
	lines              int
	parseErrors        int
	firstParseErrNo    int
	firstParseErrMsg   string
	assistant          int
	assistantNoUsage   int
	sidechain          int
	synthetic          int
	usable             int
	zeroPrompt         int
	zeroOutput         int
	compactions        int
	awaitingReply      bool
	ccVersion          string
	mistyped           string
	badUsage           string
	unusableAfterCount string
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
	CountKnown   bool           `json:"count_known"`
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
	FiveHour   *meter  `json:"five_hour"`
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
		window     = flags.Int("window", 0, "context window size in tokens (0 = read this session's sensor file)")
		sensorDir  = flags.String("sensor-dir", filepath.Join(home(), ".cache", "claude_ctx"), "directory of statusline-written sensor files")
		plain      = flags.Bool("plain", false, "print the token count, the limits, then the usage meters, as text instead of JSON")
		noThresh   = flags.Bool("no-threshold", false, "stop requiring a window size: without one, report occupancy alone; with one, judge the limits as usual, so exit 10 stays possible")
		reserve    = flags.Int("compaction-reserve", 34000, "tokens Claude Code keeps back for its forced compaction, which comes at the window minus this")
		room       = flags.Int("checkpoint-room", 30000, "tokens kept free below the forced compaction for the checkpoint itself; the checkpoint line sits this far below it")
	)
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, "usage: ccctx [flags] [session-uuid]\n\n"+
			"Reports context occupancy for a Claude Code conversation, how far it is from the\n"+
			"checkpoint line and from the forced compaction, and the usage meters from\n"+
			"~/.cache/claude_usage_limits.json (override with CCCTX_USAGE_FILE).\n\n"+
			"Name the conversation with exactly one of a session uuid, -transcript <path> or -latest.\n\n"+
			"Exit codes:\n"+
			"  0   under the checkpoint line\n"+
			"  10  at or over the checkpoint line\n"+
			"  2   DRIFT: the transcript did not contain what it must, or no window size is known (see stderr)\n"+
			"  1   bad invocation or unreadable file\n\n"+
			"Right after a compaction, until the next reply, count_known is false and the counts read 0.\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitFail // the flag package has already printed the error and the usage
	}
	// The flag package stops at the first argument that is not a flag, so anything after
	// the session id would be dropped without a word.
	if extra := flags.Args(); len(extra) > 1 {
		failf(stderr, "unexpected arguments after the session id %q: %q\n"+
			"  Flags go before the session id: ccctx [flags] [session-uuid]", extra[0], extra[1:])
		return exitFail
	}
	if *window < 0 || *reserve < 0 || *room < 0 {
		failf(stderr, "-window (%d), -compaction-reserve (%d) and -checkpoint-room (%d) are token counts and cannot be negative",
			*window, *reserve, *room)
		return exitFail
	}

	var warnings bytes.Buffer
	defer func() { _, _ = warnings.WriteTo(stderr) }()

	path, sessionID, err := locate(&warnings, *transcript, *projects, flags.Arg(0), *latest)
	if err != nil {
		failf(stderr, "%v", err)
		return exitFail
	}

	turns, st, err := scan(path)
	if err != nil {
		failf(stderr, "%v", err)
		return exitFail
	}
	if msg := verify(&st, path); msg != "" {
		drift(stderr, msg)
		return exitDrift
	}
	warn(&warnings, &st, path)

	rep := summarize(turns)
	rep.SessionID, rep.Transcript = sessionID, path
	rep.Compactions, rep.CCVersion = st.compactions, st.ccVersion

	sensorPath := filepath.Join(*sensorDir, sessionID+".json")
	windowSize, source := resolveWindow(&warnings, *window, sensorPath)
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
		// Callers may show only the first line, so that line names the way out.
		failf(stderr, "no context window size available for session %s; pass -window <tokens>\n"+
			"  Without it, %d tokens cannot be judged against the checkpoint line.\n"+
			"  Claude Code reveals the window size ONLY on the statusline's stdin (.context_window.context_window_size);\n"+
			"  it is never written to the transcript. Other ways out:\n"+
			"    - have the statusline write %s containing {\"window\": <tokens>}\n"+
			"    - pass -no-threshold, which stops requiring a window size: without one, occupancy is reported\n"+
			"      alone; with one, the limits are judged as usual and exit 10 stays possible",
			sessionID, rep.HighWater, sensorPath)
		return exitDrift
	}

	// Unlike the context number, the meters are optional: a cache that cannot be read
	// warns and reports null.
	usagePath := cmp.Or(os.Getenv("CCCTX_USAGE_FILE"), filepath.Join(home(), ".cache", "claude_usage_limits.json"))
	var metersErr error
	rep.Meters, metersErr = readMeters(usagePath, time.Now())
	if err := writeReport(stdout, &warnings, &rep, *plain, metersErr); err != nil {
		failf(stderr, "cannot render report as JSON: %v", err)
		return exitFail
	}
	if rep.judgment != nil && rep.Over {
		return exitOver
	}
	return exitOK
}

// locate resolves which transcript to read.
func locate(stderr io.Writer, explicit, projects, uuid string, latest bool) (path, sessionID string, err error) {
	var given []string
	if uuid != "" {
		given = append(given, "a session id")
	}
	if explicit != "" {
		given = append(given, "-transcript")
	}
	if latest {
		given = append(given, "-latest")
	}
	switch {
	case len(given) == 0:
		return "", "", fmt.Errorf("nothing to read: pass a session uuid, -transcript <path>, or -latest")
	case len(given) > 1:
		return "", "", fmt.Errorf("%s conflict: pass exactly one of a session uuid, -transcript <path>, or -latest",
			strings.Join(given, " and "))
	}

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

// scan walks the transcript and returns the main thread's turns since its last
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
	unterminated := false
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, token, err := bufio.ScanLines(data, atEOF)
		unterminated = atEOF && bytes.IndexByte(data[:advance], '\n') < 0
		return advance, token, err
	})

	byID := map[string]turn{}
	order := []string{}

	for sc.Scan() {
		st.lines++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		e, mistyped, err := parseLine(line)
		if err != nil {
			if unterminated {
				continue // the last line, which Claude Code may still be writing
			}
			st.parseErrors++
			if st.firstParseErrNo == 0 {
				st.firstParseErrNo, st.firstParseErrMsg = st.lines, err.Error()
			}
			st.unusableAfterCount = cmp.Or(st.unusableAfterCount, fmt.Sprintf("line %d is not a JSON object (%v)", st.lines, err))
			continue
		}
		if e.Version != "" {
			st.ccVersion = e.Version // keep the last: that is the version still writing
		}
		if mistyped != "" {
			st.mistyped = cmp.Or(st.mistyped, fmt.Sprintf("line %d: %s", st.lines, mistyped))
			continue
		}
		// A subagent compacting its own context leaves the main thread's window as it was.
		if e.IsCompactSummary && !e.IsSidechain {
			st.compactions++
			byID, order = map[string]turn{}, nil // the window really did empty here
			st.badUsage, st.unusableAfterCount = "", ""
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
		msg, _ := object(e.Message)
		if msg == nil && e.Message != nil {
			st.mistyped = cmp.Or(st.mistyped, fmt.Sprintf(`line %d: "message" is %s`, st.lines, e.Message))
			continue
		}
		var model, id string
		if bad := typed(msg, "model", &model); bad != "" {
			st.mistyped = cmp.Or(st.mistyped, fmt.Sprintf(`line %d: "message".%s`, st.lines, bad))
			continue
		}
		// Claude Code writes placeholder entries (model "<synthetic>") for refusals and
		// API errors. They carry an all-zero usage block and describe no prompt, so one
		// landing last would report an empty context.
		if model == syntheticModel {
			st.synthetic++
			continue
		}
		usage, _ := object(msg["usage"])
		if usage == nil {
			st.assistantNoUsage++
			st.unusableAfterCount = cmp.Or(st.unusableAfterCount, fmt.Sprintf(`line %d is an assistant entry without a "message"."usage" object`, st.lines))
			continue
		}
		prompt, output, err := usageCounts(usage)
		if err != nil {
			st.badUsage = cmp.Or(st.badUsage, fmt.Sprintf("line %d: %v", st.lines, err))
			continue
		}
		st.usable++
		st.unusableAfterCount = ""

		if prompt == 0 {
			st.zeroPrompt++
		}
		if output == 0 {
			st.zeroOutput++
		}
		t := turn{
			ctx:   prompt + output,
			model: model,
			ts:    e.Timestamp,
		}
		field(msg, "id", &id)
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

	st.awaitingReply = st.compactions > 0 && len(order) == 0
	turns := make([]turn, 0, len(order))
	for _, id := range order {
		turns = append(turns, byID[id])
	}
	return turns, st, nil
}

// verify decides whether the scan found what the format must contain. It returns a
// message naming the exact JSON path that is missing, because the failure this guards
// against is a renamed field quietly reading as an empty context window.
func verify(st *stats, path string) string {
	where := fmt.Sprintf("  transcript: %s\n  Claude Code version in transcript: %s", path, orUnknown(st.ccVersion))

	if st.mistyped != "" {
		return fmt.Sprintf("%s.\n"+
			"  Where present, \"type\" must be a string and \"isSidechain\" and \"isCompactSummary\" true or false, and a\n"+
			"  main-thread assistant entry's \"message\" an object with a string \"model\". These fields decide which entries\n"+
			"  count. Read as empty, a value of another type could count a subagent's reply or miss a compaction.\n%s", st.mistyped, where)
	}
	if st.badUsage != "" {
		return fmt.Sprintf("%s.\n"+
			"  Every assistant entry since the last compaction must carry all four counts, each an integer from 0 to %d.\n"+
			"  Read as 0, a missing or misread count would report the context smaller than it is.\n%s", st.badUsage, maxCount, where)
	}
	if msg := wholeFileDrift(st, where); msg != "" && !st.awaitingReply {
		return msg
	}
	if st.unusableAfterCount != "" {
		return fmt.Sprintf("%s, and no counted reply comes after it.\n"+
			"  A format change partway through a transcript looks like this: the count would stay at the last\n"+
			"  reply ccctx could read while the context grows.\n%s", st.unusableAfterCount, where)
	}
	return ""
}

func wholeFileDrift(st *stats, where string) string {
	switch {
	case st.lines == 0:
		return fmt.Sprintf("transcript is empty (0 lines).\n%s", where)

	case st.parseErrors == st.lines:
		return fmt.Sprintf("none of the %d lines is a JSON object.\n"+
			"  first failure was line %d: %s\n"+
			"  The transcript is expected to be JSON Lines, one object per line. That no longer holds.\n%s",
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
			"  \"message\".\"usage\" carries \"input_tokens\", \"cache_read_input_tokens\" and \"cache_creation_input_tokens\", but they sum to 0 in every turn.\n"+
			"  A real prompt is never empty, so the prompt is now counted somewhere else. Read as 0, these counts would\n"+
			"  report an empty context window forever, so it is a hard error instead.\n%s", st.usable, where)

	case st.zeroOutput == st.usable:
		return fmt.Sprintf("all %d usable turns reported 0 output tokens.\n"+
			"  \"message\".\"usage\".\"output_tokens\" is present but 0 in every turn. Every reply has output, and it stays in the context,\n"+
			"  so the output is now counted somewhere else. Read as 0, this count would report the context short\n"+
			"  by every reply's length, so it is a hard error instead.\n%s", st.usable, where)
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
	rep := report{CountKnown: len(turns) > 0, Turns: len(turns), PerModelPeak: map[string]int{}}
	for _, t := range turns {
		if t.ctx > rep.HighWater {
			rep.HighWater = t.ctx
		}
		if t.model != "" && t.ctx > rep.PerModelPeak[t.model] {
			rep.PerModelPeak[t.model] = t.ctx
		}
	}
	if rep.CountKnown {
		last := turns[len(turns)-1]
		rep.Current, rep.Model = last.ctx, last.model
	}
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
func resolveWindow(stderr io.Writer, explicit int, sensorPath string) (window int, source string) {
	if explicit > 0 {
		return explicit, "flag"
	}
	if w, ok := readSensor(stderr, sensorPath); ok {
		return w, "sensor"
	}
	return 0, ""
}

// readSensor reads one sensor file. A sensor that exists but cannot be used is
// reported loudly, since silently ignoring it is how a limit stops being checked.
func readSensor(stderr io.Writer, path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			_, _ = fmt.Fprintf(stderr, "ccctx warning: cannot read sensor file %s: %v\n", path, err)
		}
		return 0, false
	}
	var s sensor
	if err := json.Unmarshal(b, &s); err != nil {
		_, _ = fmt.Fprintf(stderr, "ccctx warning: sensor file %s is not valid JSON: %v\n", path, err)
		return 0, false
	}
	if s.Window <= 0 {
		_, _ = fmt.Fprintf(stderr, "ccctx warning: sensor file %s has no usable \"window\" value (got %d)\n", path, s.Window)
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
	if c.FiveHour != nil && c.FiveHour.Utilization == nil {
		return nil, fmt.Errorf("%s has no .five_hour.utilization", path)
	}
	if c.Limits == nil {
		return nil, fmt.Errorf("%s has no .limits", path)
	}
	m := &meters{
		AgeSeconds: int64(now.Sub(fi.ModTime()) / time.Second),
		Limits:     make([]meter, 0, len(c.Limits)),
	}
	if c.FiveHour != nil {
		m.FiveHour = &meter{Percent: *c.FiveHour.Utilization, ResetsAt: c.FiveHour.ResetsAt}
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
	if m.FiveHour != nil {
		line("five_hour", *m.FiveHour)
	}
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
		unknown := ""
		if !rep.CountKnown {
			unknown = "count unknown until the next reply (the conversation compacted and has not replied since)\n"
		}
		out = fmt.Sprintf("%d\n%s%s%s", rep.HighWater, limitsText(rep.judgment), metersText(rep.Meters), unknown)
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
