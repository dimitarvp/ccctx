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
ccctx -plain <uuid>           # print only the token count
```

Exit codes: `0` under the threshold, `10` at or over it, `2` format drift,
`1` bad invocation or unreadable file. See `-h` for `-threshold`, `-window`,
`-no-threshold`, `-projects`, `-sensor-dir`.

## Where the number comes from

Each assistant turn in the transcript records the usage of the prompt Claude
Code sent: `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`.
The number is read from disk, not fetched, so it is accurate even for a
resumed session that has made no request yet.

Two corrections a naive reading needs:

- **Model switches change the accounting.** The same conversation measured
  275,398 tokens on one model and 177,507 on another. ccctx keeps a high-water
  mark; switches only ever read lower, so the peak recovers the true figure.
- **Compaction genuinely empties the window** (166k to 25k observed). The
  high-water mark resets at every compaction entry.

Sidechain (subagent) turns and `<synthetic>` placeholder entries are skipped.

## The window size

The transcript does not contain the window size. Claude Code reports it only
on the statusline's stdin, so a statusline script writes it to a sensor file:

```
~/.cache/claude_ctx/<session-id>.json    {"window": 1000000, ...}
```

ccctx reads that, or takes `-window <tokens>`, or exits `2` and says how to
supply one. Reading it live means a larger window on a future model raises
the threshold with no edits.

## Format drift is a hard error

The transcript format is Claude Code private and changes between versions.
Every read is validated; anything unrecognized exits `2` and names the exact
JSON path that broke. The dangerous case is a renamed token field: it would
sum to zero and report an empty window forever, so it is a loud stop instead.

## Use as a Stop hook

A Stop hook runs ccctx when a turn ends and, past the threshold, returns
`{"decision":"block","reason":"..."}` asking the session to checkpoint.
Checks happen at turn boundaries, so one very long turn can still overshoot.

## Install

```
go install github.com/dimitarvp/ccctx@latest
```
