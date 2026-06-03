package repository

import "testing"

// TestNormalizeFileName 验证 repo 入口归一：去掉末尾 .md（含历史遗留的多重 .md），
// 大小写敏感（只剥小写 .md，与 save 补的小写 .md 对齐），中间的 .md 保留。
// 这是 #122 的核心——保证 repo 内部 FileName 始终是裸名。
func TestNormalizeFileName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"裸名", "hello-world", "hello-world"},
		{"单个 .md", "hello-world.md", "hello-world"},
		{"双 .md（历史 bug 状态）", "hello-world.md.md", "hello-world"},
		{"三 .md", "a.md.md.md", "a"},
		{"空串", "", ""},
		{"仅 .md", ".md", ""},
		{"大写不剥", "hello.MD", "hello.MD"},
		{"中间 .md 保留", "a.md.b", "a.md.b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeFileName(tt.in); got != tt.want {
				t.Errorf("normalizeFileName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
