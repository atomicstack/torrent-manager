// Package aiclassify uses Claude to extract {show, season} from a torrent release
// title when our regex-based parser can't.
package aiclassify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Result holds the extracted show name and season number.
type Result struct {
	Show   string `json:"show"`
	Season int    `json:"season"`
}

// Client wraps the Anthropic SDK client for title classification.
type Client struct {
	inner anthropic.Client
}

// New returns a Client that calls the Anthropic API with the given API key.
func New(apiKey string, opts ...option.RequestOption) *Client {
	all := append([]option.RequestOption{option.WithAPIKey(apiKey)}, opts...)
	c := anthropic.NewClient(all...)
	return &Client{inner: c}
}

const systemPrompt = `You extract show name and season number from torrent release titles.

Return JSON with these fields:
- show: lowercase, words separated by single dots, no special characters or spaces. Example: "its.always.sunny.in.philadelphia". Empty string if the title doesn't look like a TV episode.
- season: integer season number (e.g. 10 for S10E23). 0 if the title doesn't have a season.

Do not include episode number, quality, codec, or release group in the show name.`

var outputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"show": map[string]any{
			"type":        "string",
			"description": "Show name, lowercase, dot-separated (e.g. its.always.sunny.in.philadelphia). Empty string if not extractable.",
		},
		"season": map[string]any{
			"type":        "integer",
			"description": "Season number. 0 if not extractable.",
		},
	},
	"required":             []string{"show", "season"},
	"additionalProperties": false,
}

// Classify calls claude-opus-4-7 with structured outputs to extract a show name
// and season number. show is returned lowercase + dot-separated.
// 30s timeout via the passed context.
func (c *Client) Classify(ctx context.Context, title string) (Result, error) {
	start := time.Now()
	resp, err := c.inner.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeOpus4_7,
		MaxTokens: 200,
		System: []anthropic.TextBlockParam{
			{Text: systemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(title)),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{
				Schema: outputSchema,
			},
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("aiclassify: api call: %w", err)
	}
	logUsage("classify", string(anthropic.ModelClaudeOpus4_7), start, resp.Usage)

	for _, block := range resp.Content {
		if block.Type == "text" {
			var res Result
			if err := json.Unmarshal([]byte(block.Text), &res); err != nil {
				return Result{}, fmt.Errorf("aiclassify: parse response json: %w", err)
			}
			return res, nil
		}
	}
	return Result{}, fmt.Errorf("aiclassify: no text block in response")
}

// logUsage emits one structured log line per anthropic API round-trip with
// model + latency + token usage. Pricing isn't in the response; callers can
// cross-reference model+token counts against the anthropic pricing page.
func logUsage(call, model string, start time.Time, u anthropic.Usage) {
	slog.Info("anthropic_call",
		"call", call,
		"model", model,
		"latency_ms", time.Since(start).Milliseconds(),
		"input_tokens", u.InputTokens,
		"output_tokens", u.OutputTokens,
		"cache_creation_tokens", u.CacheCreationInputTokens,
		"cache_read_tokens", u.CacheReadInputTokens,
		"service_tier", string(u.ServiceTier))
}
