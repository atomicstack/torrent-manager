package aiclassify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// PickDirResult is the structured response from PickDir.
type PickDirResult struct {
	ShowDir  string `json:"show_dir"`   // either a member of existingDirs or a new proposed name
	Season   int    `json:"season"`     // 1+; 0 if not extractable from the title
	IsNewDir bool   `json:"is_new_dir"` // true when ShowDir is not an exact match in existingDirs
}

const pickDirSystemPrompt = `You are matching a torrent release title against an existing list of TV show
directory names. The user will give you:
  - the release title (e.g. "Severance.S02E01.1080p.WEB-DL.AAC2.0.H.264-NTb.mkv")
  - a list of existing directory names already on disk (e.g. ["beef",
    "fallout.2024", "severance", "guy.montgomerys.guy.mont.spelling.bee.au"])

Pick which existing directory the release belongs to. If none of them clearly
match, propose a new lowercase dot-separated name (e.g. "the.bear" not
"The Bear"). Always extract the season number if the title contains an
SxxEyy or "season N" marker.

Respond with strict JSON only, matching this schema (no prose, no code fence):

{
  "show_dir": "<directory name>",
  "season": <integer; 0 if not determinable>,
  "is_new_dir": <true if you proposed a new name, false if it matches an existing entry>
}

Rules:
  - "show_dir" must equal an element of the existingDirs list exactly, or be a
    new short lowercase dot-separated name you propose.
  - "season" is an integer >= 0. 0 means the title didn't carry a season number.
  - "is_new_dir" must be true if show_dir is not present in existingDirs (case-
    sensitive), otherwise false.`

var pickDirOutputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"show_dir": map[string]any{
			"type":        "string",
			"description": "Either an existing directory name from the supplied list, or a new lowercase dot-separated name.",
		},
		"season": map[string]any{
			"type":        "integer",
			"description": "Season number extracted from the title. 0 if not determinable.",
		},
		"is_new_dir": map[string]any{
			"type":        "boolean",
			"description": "True if show_dir is not present in existingDirs.",
		},
	},
	"required":             []string{"show_dir", "season", "is_new_dir"},
	"additionalProperties": false,
}

// PickDir asks Claude to match a torrent title against a list of existing
// show directory names and to extract the season number from the title.
//
// On a confident match it returns an existing entry from existingDirs. When
// none of the existing dirs fit, it proposes a new dot-separated lowercase
// directory name (e.g. "severance") and sets IsNewDir=true.
//
// The model is asked to respond as strict JSON; non-JSON or schema-violating
// replies return a wrapped error so the caller can fall back to a manual
// edit form.
func (c *Client) PickDir(ctx context.Context, title string, existingDirs []string) (PickDirResult, error) {
	var sb strings.Builder
	sb.WriteString("title: ")
	sb.WriteString(title)
	sb.WriteString("\n\nexistingDirs:\n")
	for _, d := range existingDirs {
		sb.WriteString("- ")
		sb.WriteString(d)
		sb.WriteString("\n")
	}

	resp, err := c.inner.Messages.New(ctx, anthropic.MessageNewParams{
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

		// reject obviously bogus replies so the caller knows to fall back to manual.
		if res.ShowDir == "" {
			return PickDirResult{}, fmt.Errorf("aiclassify pickdir: model returned empty show_dir")
		}
		if res.Season < 0 {
			return PickDirResult{}, fmt.Errorf("aiclassify pickdir: model returned negative season %d", res.Season)
		}

		// double-check the is_new_dir flag matches reality so a confused model can't
		// silently claim a new dir is an existing one or vice versa.
		inExisting := false
		for _, d := range existingDirs {
			if d == res.ShowDir {
				inExisting = true
				break
			}
		}
		res.IsNewDir = !inExisting
		return res, nil
	}
	return PickDirResult{}, fmt.Errorf("aiclassify pickdir: no text block in response")
}
