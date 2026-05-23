package aiclassify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// PickDirResult is the structured response from PickDir.
//
// ShowDir is the chosen show directory — either an entry from existingDirs
// (case-sensitive match) or a freshly-proposed lowercase dot-separated name.
//
// Subdir is the subdirectory to nest under ShowDir, decided by the model
// based on the torrent's own structure:
//   - "season3" / "season12" / ... when the torrent doesn't carry its own
//     season folder (single episode, or a flat batch under the release dir)
//   - "" when the torrent already wraps its files in a season folder, or is
//     a multi-season pack (both cases: let the torrent's own layout decide).
//
// The caller composes the final save_dir as filepath.Join(root, ShowDir, Subdir).
type PickDirResult struct {
	ShowDir  string `json:"show_dir"`
	Subdir   string `json:"subdir"`
	IsNewDir bool   `json:"is_new_dir"`
}

// TorrentContents is the lightweight view of a torrent's payload passed to
// PickDir as additional context. Files is the list of file paths relative to
// info.name (joined with "/"), capped before passing to claude so a multi-
// hundred-file season pack doesn't blow the prompt budget.
type TorrentContents struct {
	Name       string
	SingleFile bool
	Files      []string
}

const pickDirSystemPrompt = `You are matching a torrent release against an existing list of TV show
directory names. The user will give you:
  - the release title
  - the torrent's internal file layout (info.name + a sample of the files
    inside, relative to info.name)
  - a list of existing TV show directory names already on disk

Pick which existing directory the release belongs to. If none of them clearly
match, propose a new lowercase dot-separated name (e.g. "the.bear" not
"The Bear").

Then decide the subdirectory under that show dir where this torrent should
land. The torrent's own wrapper directory (info.name) matters here:
  - single-file torrent (info.name IS the file): the torrent has no wrapper —
    use subdir = "season<N>" so the file lands in a season folder.
  - multi-file torrent whose wrapper directory IS a season folder (e.g.
    "foobar.s03" or "Show Name Season 3"): the torrent already brings the
    season folder. Use subdir = "" — saving directly under ShowDir lets the
    wrapper become the season folder.
  - multi-file torrent that is a multi-season pack (the files break down into
    multiple season subdirs inside the wrapper): use subdir = "" — the
    wrapper holds all the seasons already.
  - multi-file torrent whose wrapper is just the show name with no season
    marker, but the files are all from one season: use subdir = "season<N>"
    so the files land in a fresh season folder.

Respond with strict JSON only, matching this schema (no prose, no code fence):

{
  "show_dir": "<directory name>",
  "subdir":   "<season folder name, or empty>",
  "is_new_dir": <true if you proposed a new name, false if it matches an existing entry>
}

Rules:
  - "show_dir" must equal an element of the existingDirs list exactly, or be a
    new short lowercase dot-separated name you propose.
  - "show_dir" must contain ONLY characters [a-z0-9.-] — lowercase letters,
    digits, dots as word separators, and dashes only when the source title
    carries a hyphenated word (e.g. "x-men", "spider-man"). Avoid dashes
    otherwise — prefer dots between words. No commas, apostrophes,
    parentheses, spaces, or other punctuation. Year suffixes ("2024") are
    fine when the source title carries a year. Never leave a trailing dot,
    dash, or comma.
  - "subdir" is either "" (empty string) or "season<N>" where N is the
    season number from the title. Use the same lowercase [a-z0-9] rule
    (no dashes — season folders are always plain).
  - "is_new_dir" must be true if show_dir is not present in existingDirs
    (case-sensitive), otherwise false.`

var pickDirOutputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"show_dir": map[string]any{
			"type":        "string",
			"description": "Either an existing directory name from the supplied list, or a new lowercase dot-separated name.",
		},
		"subdir": map[string]any{
			"type":        "string",
			"description": "Subdirectory under show_dir (e.g. \"season3\"), or \"\" when the torrent already carries its own season folder.",
		},
		"is_new_dir": map[string]any{
			"type":        "boolean",
			"description": "True if show_dir is not present in existingDirs.",
		},
	},
	"required":             []string{"show_dir", "subdir", "is_new_dir"},
	"additionalProperties": false,
}

// maxFilesInPrompt caps how many file paths we send to claude. A season pack
// can carry hundreds of files; the first ~30 are enough to identify the
// layout (single file vs flat batch vs nested-by-season). The rest add token
// cost without changing the answer.
const maxFilesInPrompt = 30

// PickDir asks Claude to match a torrent title against a list of existing
// show directory names and to decide a season subdirectory based on the
// torrent's actual layout.
//
// On a confident match it returns an existing entry from existingDirs. When
// none of the existing dirs fit, it proposes a new dot-separated lowercase
// directory name (e.g. "severance") and sets IsNewDir=true.
//
// The model is asked to respond as strict JSON; non-JSON or schema-violating
// replies return a wrapped error so the caller can fall back to a manual
// edit form.
func (c *Client) PickDir(ctx context.Context, title string, existingDirs []string, contents TorrentContents) (PickDirResult, error) {
	var sb strings.Builder
	sb.WriteString("title: ")
	sb.WriteString(title)
	sb.WriteString("\n\ntorrent contents:\n")
	sb.WriteString("  info.name: ")
	sb.WriteString(contents.Name)
	sb.WriteString("\n  single-file: ")
	if contents.SingleFile {
		sb.WriteString("yes")
	} else {
		sb.WriteString("no")
	}
	sb.WriteString("\n  files (relative to info.name):\n")
	shown := contents.Files
	truncated := 0
	if len(shown) > maxFilesInPrompt {
		truncated = len(shown) - maxFilesInPrompt
		shown = shown[:maxFilesInPrompt]
	}
	for _, f := range shown {
		sb.WriteString("    - ")
		sb.WriteString(f)
		sb.WriteString("\n")
	}
	if truncated > 0 {
		sb.WriteString(fmt.Sprintf("    (+ %d more files not shown)\n", truncated))
	}
	sb.WriteString("\nexistingDirs:\n")
	for _, d := range existingDirs {
		sb.WriteString("- ")
		sb.WriteString(d)
		sb.WriteString("\n")
	}

	resp, err := c.inner.Messages.New(ctx, anthropic.MessageNewParams{
		// Opus 4.7: sonnet 4.6 stumbled on season-pack layouts (picking the
		// show root instead of a SeasonN subdir, or inventing a subdir name),
		// so pickdir stays on opus despite the latency hit.
		Model:     anthropic.ModelClaudeOpus4_7,
		MaxTokens: 300,
		System: []anthropic.TextBlockParam{
			{Text: pickDirSystemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(sb.String())),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{
				Schema: pickDirOutputSchema,
			},
		},
	})
	if err != nil {
		return PickDirResult{}, fmt.Errorf("aiclassify pickdir: api call: %w", err)
	}

	for _, block := range resp.Content {
		if block.Type != "text" {
			continue
		}
		var res PickDirResult
		if err := json.Unmarshal([]byte(block.Text), &res); err != nil {
			return PickDirResult{}, fmt.Errorf("aiclassify pickdir: parse response json: %w", err)
		}

		// defence in depth: even with explicit prompt rules, the model occasionally
		// emits stray punctuation that would land in the save_dir path. normalise
		// here so callers never see invalid filename characters. only applies when
		// is_new_dir would be true post-membership-check — exact matches against
		// existingDirs are trusted as-is.
		inExisting := false
		for _, d := range existingDirs {
			if d == res.ShowDir {
				inExisting = true
				break
			}
		}
		if !inExisting {
			res.ShowDir = sanitiseShowDir(res.ShowDir)
		}
		// Subdir is always a plain "seasonN" form — strip dashes too so a
		// stray "season-3" collapses to "season.3" rather than passing through.
		// "" stays "" (sanitise trims to empty for an all-non-alnum input).
		res.Subdir = sanitiseSubdir(res.Subdir)

		// reject obviously bogus replies so the caller knows to fall back to manual.
		if res.ShowDir == "" {
			return PickDirResult{}, fmt.Errorf("aiclassify pickdir: model returned empty show_dir")
		}

		res.IsNewDir = !inExisting
		return res, nil
	}
	return PickDirResult{}, fmt.Errorf("aiclassify pickdir: no text block in response")
}

// sanitiseShowDir lowercases the input and keeps only [a-z0-9.-]. Runs of
// disallowed characters collapse to a single dot; leading and trailing
// dots/dashes are trimmed. Internal dashes are preserved verbatim so the
// model can keep hyphenated names (e.g. "x-men", "spider-man") intact, but
// stray punctuation (commas, quotes, spaces) is still dropped.
func sanitiseShowDir(s string) string {
	var b strings.Builder
	inSep := true
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			inSep = false
		case r == '-' && !inSep:
			b.WriteRune(r)
			// dash is its own separator; do NOT set inSep so consecutive
			// alphanumerics on the right keep flowing as one word.
		case !inSep:
			b.WriteByte('.')
			inSep = true
		}
	}
	return strings.Trim(b.String(), ".-")
}

// sanitiseSubdir is the strict variant used for season-folder names. Same
// rules as sanitiseShowDir but dashes are NOT preserved — a season folder is
// always plain alphanumerics + dots. "season-3" becomes "season.3" (and any
// caller-facing oddity is on the model for proposing it).
func sanitiseSubdir(s string) string {
	var b strings.Builder
	inSep := true
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			inSep = false
		} else if !inSep {
			b.WriteByte('.')
			inSep = true
		}
	}
	return strings.Trim(b.String(), ".")
}
