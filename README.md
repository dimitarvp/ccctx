# ccctx

## Why does this exist

Claude Code compacts a conversation when its context window fills up. The
summary loses detail, and the session gets no say in what survives. A session
that knows its own context usage can checkpoint itself first, on its own terms.

Claude Code does not expose that number. ccctx recovers it from the session
transcript on disk.

## Usage

```
ccctx <session-uuid>          # by session id, searched under ~/.claude/projects
ccctx -latest                 # the most recently modified transcript
ccctx -transcript <path>      # an exact file
ccctx -plain <uuid>           # the token count, the limits line, the meters, as text
```

Give exactly one of a session id, `-transcript` and `-latest`. Two at once
exit `1` naming both.

The report ends with `meters` from `~/.cache/claude_usage_limits.json` (override:
`CCCTX_USAGE_FILE`): the five-hour window, each `.limits[]` entry and the file's
age in seconds. A missing or malformed file gives `null` and a warning; the
exit code ignores it. A file without a five-hour block gives
`"five_hour": null` and keeps the limits, with no warning.

Exit codes: `0` under the checkpoint line, `10` at or over it, `2` format
drift or no window size, `1` bad invocation or unreadable file. See `-h`
for `-window`, `-compaction-reserve`, `-checkpoint-room`, `-no-threshold`,
`-projects`, `-sensor-dir`.

Warnings come last on stderr: when ccctx fails, the first line of stderr
says why.

## Where the number comes from

Each assistant turn in the transcript records the usage of the prompt Claude
Code sent and of the reply it got:

```
input_tokens + cache_read_input_tokens + cache_creation_input_tokens + output_tokens
```

The reply counts because the next prompt carries it, and Claude Code's own
compaction check adds it too. The number is read from disk, not fetched, so it
is accurate even for a resumed session that has made no request yet.

Two corrections a naive reading needs:

- **Model switches change the accounting.** The same conversation measured
  275,398 tokens on one model and 177,507 on another. ccctx keeps a high-water
  mark; switches only ever read lower, so the peak recovers the true figure.
- **Compaction genuinely empties the window** (166k to 25k observed). The
  high-water mark resets at every compaction entry.

Right after a compaction, before the next reply, the transcript holds no
count of the emptied window. ccctx then exits `0` with `count_known` false,
`current_tokens`, `high_water_tokens` and `turns` at 0, and the limits
judged on 0. `-plain` says the count is unknown until the next reply. Every
other report has `count_known` true.

Sidechain (subagent) entries, their compactions included, and `<synthetic>`
placeholder entries are skipped.

## The window size

The transcript does not contain the window size. Claude Code reports it only
on the statusline's stdin, so a statusline script writes it to a sensor file:

```
~/.cache/claude_ctx/<session-id>.json    {"window": 1000000, ...}
```

ccctx reads the session's own file, or takes `-window <tokens>`. It never
uses another session's file, which may describe another model. Without a
window size it exits `2`, and the first line of stderr names `-window
<tokens>`; under `-no-threshold` it reports without limits instead.
Reading it live means a larger window on a future model moves both limits
with no edits.

## The two limits

The window size is never the limit. Claude Code keeps back 34,000 tokens for
its own compaction, on any window size, and forces one when the context
reaches the window minus that. A checkpoint written before then costs 25,000
to 30,000 tokens, so the checkpoint line sits 30,000 lower:

| window    | forced compaction | checkpoint line |
|-----------|-------------------|-----------------|
| 1,000,000 | 966,000           | 936,000         |
| 200,000   | 166,000           | 136,000         |

The report carries them as `usable_tokens` and `checkpoint_line_tokens`, and
the tokens left to each as `left_to_compaction_tokens` and
`left_to_checkpoint_tokens`, negative once past it. `used_pct` is the
high-water mark as a percentage of `usable_tokens`. ccctx exits `10` from the
checkpoint line on.

`-compaction-reserve` (default 34000) and `-checkpoint-room` (default 30000)
set the two margins; a window that cannot hold both is an error (exit `1`).
`-no-threshold` stops requiring a window size: without one, the report
carries occupancy with none of these fields; with one, the limits are judged
as usual and exit `10` stays possible.

## Format drift is a hard error

The transcript format is Claude Code private and changes between versions.
ccctx exits `2` and says what it expected when the transcript has no
parseable line, no assistant entry, or no usable entry (an assistant entry
that is not skipped and has a usage block). It does the same when every
usable entry has a prompt side of zero (the three prompt fields together),
or an output of zero.

Since the last compaction, every usage block must carry all four counts,
each from 0 to 1,000,000,000. A missing key or a count out of range exits
`2` naming the key and the line. So does an unparseable line, or an
assistant entry without a usage block, after the last counted reply: that
is what a format change partway through a transcript looks like. Followed
by a counted reply, such lines stay warnings. A last line with no newline
after it is one Claude Code is still writing, and ccctx leaves it out.

## Use as a Stop hook

A Stop hook runs ccctx when a turn ends and, from the checkpoint line on,
returns `{"decision":"block","reason":"..."}` asking the session to checkpoint.
Checks happen at turn boundaries, so one very long turn can still overshoot.

## Install

```
go install github.com/dimitarvp/ccctx@latest
```
