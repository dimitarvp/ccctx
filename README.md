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

The report ends with `meters` from `~/.cache/claude_usage_limits.json` (override:
`CCCTX_USAGE_FILE`): the five-hour window, each `.limits[]` entry and the file's
age in seconds. A missing or malformed file gives `null`; the exit code ignores it.

Exit codes: `0` under the checkpoint line, `10` at or over it, `2` format
drift, `1` bad invocation or unreadable file. See `-h` for `-window`,
`-compaction-reserve`, `-checkpoint-room`, `-no-threshold`, `-projects`,
`-sensor-dir`.

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

Sidechain (subagent) entries, their compactions included, and `<synthetic>`
placeholder entries are skipped.

## The window size

The transcript does not contain the window size. Claude Code reports it only
on the statusline's stdin, so a statusline script writes it to a sensor file:

```
~/.cache/claude_ctx/<session-id>.json    {"window": 1000000, ...}
```

ccctx reads that, or takes `-window <tokens>`, or exits `2` and says how to
supply one. Reading it live means a larger window on a future model moves
both limits with no edits.

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
Every read is validated; anything unrecognized exits `2` and names the exact
JSON path that broke. The dangerous case is a renamed token field: it would
sum to zero and report an empty window forever, so it is a loud stop instead.
The prompt fields and `output_tokens` are checked apart, so a renamed field on
one side cannot hide behind the other's tokens.

## Use as a Stop hook

A Stop hook runs ccctx when a turn ends and, from the checkpoint line on,
returns `{"decision":"block","reason":"..."}` asking the session to checkpoint.
Checks happen at turn boundaries, so one very long turn can still overshoot.

## Install

```
go install github.com/dimitarvp/ccctx@latest
```
