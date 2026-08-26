package helpdocs

import (
	"strings"
	"testing"
)

func TestRenderStaticSanitizesActiveContent(t *testing.T) {
	html, search, err := RenderStatic("# 安全标题\n\n[危险链接](javascript:alert(1))\n\n<script>alert(2)</script>\n\n正文内容")
	if err != nil {
		t.Fatalf("RenderStatic() error = %v", err)
	}
	if strings.Contains(strings.ToLower(html), "javascript:") || strings.Contains(strings.ToLower(html), "<script") {
		t.Fatalf("RenderStatic() unsafe HTML = %q", html)
	}
	if !strings.Contains(html, "<h1") || !strings.Contains(html, "正文内容") {
		t.Fatalf("RenderStatic() HTML = %q", html)
	}
	if !strings.Contains(search, "安全标题") || !strings.Contains(search, "正文内容") {
		t.Fatalf("RenderStatic() search = %q", search)
	}
}

func TestRenderStaticRejectsEmptyAndControlCharacters(t *testing.T) {
	for _, source := range []string{"", "   ", "正文\x00内容"} {
		if _, _, err := RenderStatic(source); err == nil {
			t.Fatalf("RenderStatic(%q) unexpectedly succeeded", source)
		}
	}
}
