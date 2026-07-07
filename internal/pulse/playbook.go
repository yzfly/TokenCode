// playbook 是长期记忆的条目化形态（ACE 式）：记忆文件不是一团自由文本，
// 而是一组 `- [id] 内容` 条目。做梦（Reflector）只输出增量操作，
// 确定性合并（Curator）在这里用纯 Go 完成——LLM 永远不重写整个文件，
// 一次坏梦最多污染几条，不会毁掉全部记忆（context collapse 防线）。
package pulse

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	maxPlaybookEntries = 60  // 条目上限：溢出时淘汰最久未更新的（尾部）
	maxEntryChars      = 300 // 单条内容硬上限
	maxOpsPerDream     = 8   // 单个梦最多接受的操作数，防失控
)

// playEntry 是一条 playbook 条目。切片顺序即新鲜度：最近更新的在前。
type playEntry struct {
	ID   string
	Text string
}

// playOp 是 Reflector 输出的一个增量操作。
type playOp struct {
	Kind string // add / update / delete
	ID   string
	Text string
}

var entryRe = regexp.MustCompile(`^\s*-\s*\[([^\]\s]+)\]\s*(.+)$`)

// parsePlaybook 解析记忆文件：条目行进 entries，其余非空行原样收进 legacy
// （旧版自由格式记忆——喂给梦让它用 ADD 收编，新文件不再保留）。
func parsePlaybook(content string) (entries []playEntry, legacy string) {
	var legacyLines []string
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		if m := entryRe.FindStringSubmatch(line); m != nil {
			if seen[m[1]] {
				continue // 重复 id 保首条（首条更新鲜）
			}
			seen[m[1]] = true
			entries = append(entries, playEntry{ID: m[1], Text: strings.TrimSpace(m[2])})
			continue
		}
		t := strings.TrimSpace(line)
		// 标题与说明行属于渲染骨架，不算旧自由格式内容。
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "（") || strings.HasPrefix(t, "(") {
			continue
		}
		legacyLines = append(legacyLines, t)
	}
	return entries, strings.Join(legacyLines, "\n")
}

// parseOps 解析 Reflector 输出：逐行认 ADD/UPDATE/DELETE/NOOP，
// 其余行（解释、围栏、废话）一律忽略——对模型输出保持宽容。
func parseOps(s string) []playOp {
	var ops []playOp
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		if t == "" || strings.HasPrefix(t, "```") {
			continue
		}
		fields := strings.SplitN(t, " ", 2)
		kind := strings.ToUpper(fields[0])
		if kind == "NOOP" {
			continue
		}
		if len(fields) < 2 {
			continue
		}
		rest := strings.TrimSpace(fields[1])
		switch kind {
		case "DELETE":
			if id := sanitizeID(rest); id != "" {
				ops = append(ops, playOp{Kind: "delete", ID: id})
			}
		case "ADD", "UPDATE":
			id, text, ok := strings.Cut(rest, "|")
			if !ok {
				continue
			}
			sid := sanitizeID(id)
			text = strings.TrimSpace(text)
			if sid == "" || text == "" {
				continue
			}
			ops = append(ops, playOp{Kind: strings.ToLower(kind), ID: sid, Text: text})
		}
		if len(ops) >= maxOpsPerDream {
			break
		}
	}
	return ops
}

// sanitizeID 收紧条目 id：只留字母数字与 -_.，防止怪字符破坏条目行格式。
func sanitizeID(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			return "" // 带空格/中文的不是 id，多半是模型输出走样，整个操作作废
		}
	}
	return strings.ToLower(b.String())
}

// applyOps 确定性合并：add/update 都是「删旧、插最前」（最近更新的最新鲜），
// delete 移除；最后按上限截尾——被挤掉的正是最久没被任何梦碰过的条目。
func applyOps(entries []playEntry, ops []playOp) []playEntry {
	for _, op := range ops {
		entries = removeEntry(entries, op.ID)
		if op.Kind == "delete" {
			continue
		}
		entries = append([]playEntry{{ID: op.ID, Text: clipHead(op.Text, maxEntryChars)}}, entries...)
	}
	if len(entries) > maxPlaybookEntries {
		entries = entries[:maxPlaybookEntries]
	}
	return entries
}

func removeEntry(entries []playEntry, id string) []playEntry {
	out := entries[:0]
	for _, e := range entries {
		if e.ID != id {
			out = append(out, e)
		}
	}
	return out
}

// renderPlaybook 渲染 playbook 全文（注入 system prompt 的就是这份）。
func renderPlaybook(entries []playEntry) string {
	var b strings.Builder
	b.WriteString("# 长期记忆 playbook\n")
	b.WriteString("（做梦机制增量维护；最近更新的在前）\n\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s] %s\n", e.ID, e.Text)
	}
	return b.String()
}
