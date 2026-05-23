package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"time"
)

// templatesFS holds the html templates rendered by tmweb. We embed at compile
// time so the binary is fully self-contained — no separate asset deployment.
//
//go:embed templates/*.html
var templatesFS embed.FS

// staticFS holds the css + vendored htmx.min.js served at /static/. Same
// rationale as templatesFS: embed everything into the binary.
//
//go:embed static
var staticFS embed.FS

// funcMap is the set of helper funcs available to all templates. Keep small
// and additive — every entry expands the template surface area.
var funcMap = template.FuncMap{
	"ago": humanAgo,
}

// renderer holds the parsed templates tmweb serves. layout is the full
// page shell rendered for GET /; rows is the table-rows fragment swapped in
// by htmx after each search; confirm is the modal fragment swapped into the
// #modal portal div by the enqueue-confirm flow.
type renderer struct {
	layout  *template.Template
	rows    *template.Template
	confirm *template.Template
}

// newRenderer parses the embedded templates and returns a ready-to-execute
// renderer. Returns an error if any template fails to parse so main can fail
// fast at startup rather than at first request.
func newRenderer() (*renderer, error) {
	layout, err := template.New("layout.html").Funcs(funcMap).ParseFS(templatesFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("parse layout: %w", err)
	}
	rows, err := template.New("_rows.html").Funcs(funcMap).ParseFS(templatesFS, "templates/_rows.html")
	if err != nil {
		return nil, fmt.Errorf("parse rows: %w", err)
	}
	confirm, err := template.New("_confirm.html").Funcs(funcMap).ParseFS(templatesFS, "templates/_confirm.html")
	if err != nil {
		return nil, fmt.Errorf("parse confirm: %w", err)
	}
	return &renderer{layout: layout, rows: rows, confirm: confirm}, nil
}

// humanAgo returns a coarse human-readable relative time given a unix-seconds
// timestamp. Returns "just now" for deltas < 60s, "?" for zero or future
// timestamps. We deliberately don't bother with i18n / months / years —
// the rss item index is short-lived (days, not months).
func humanAgo(ts int64) string {
	if ts == 0 {
		return "?"
	}
	d := time.Since(time.Unix(ts, 0))
	if d < 0 {
		return "?"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// staticHandler serves the embedded static/ subtree under /static/. The
// io/fs.Sub call strips the leading "static/" so /static/app.css resolves
// to static/app.css inside the embedded fs.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// embed guarantees this directory exists — panic would only fire if
		// someone removed the //go:embed directive without updating this.
		panic(fmt.Sprintf("static subtree missing: %v", err))
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}
