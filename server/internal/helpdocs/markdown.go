package helpdocs

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

var markdownSearchMarkup = regexp.MustCompile(`[\x60*_#\[\]()>~|\\-]+`)

// RenderStatic converts trusted-editor Markdown into the immutable HTML stored
// in a publication snapshot. Goldmark does not enable raw HTML and the result
// is sanitized again so a compromised editor account cannot publish scripts or
// unsafe URL protocols.
func RenderStatic(source string) (string, string, error) {
	if !utf8.ValidString(source) || len(strings.TrimSpace(source)) == 0 || utf8.RuneCountInString(source) > 100000 {
		return "", "", ErrDocumentInvalid
	}
	for _, r := range source {
		if r == 0 || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') {
			return "", "", ErrDocumentInvalid
		}
	}

	engine := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	var rendered bytes.Buffer
	if err := engine.Convert([]byte(source), &rendered); err != nil {
		return "", "", errors.Join(ErrDocumentInvalid, err)
	}
	policy := bluemonday.UGCPolicy()
	policy.RequireNoFollowOnLinks(true)
	policy.RequireNoReferrerOnLinks(true)
	staticHTML := strings.TrimSpace(policy.Sanitize(rendered.String()))
	if staticHTML == "" {
		return "", "", ErrDocumentInvalid
	}

	searchText := strings.ToLower(markdownSearchMarkup.ReplaceAllString(source, " "))
	searchText = strings.Join(strings.Fields(searchText), " ")
	return staticHTML, searchText, nil
}
