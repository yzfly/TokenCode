// Package permrules 实现声明式权限规则：allow / ask / deny 三表，
// 规则语法对标 CC——`工具名` 或 `工具名(参数模式)`，如 `read`、`bash(git log *)`、
// `mcp__github__*`、`agent(explore)`。三表优先级 deny > ask > allow > 不命中
// （不命中时由调用方按权限模式走默认逻辑）。
//
// 参数模式按工具取义：
//   - bash：对 command 字符串做 glob（`*` 匹配任意字符序列，大小写敏感）。
//     尾部 ` *` 允许零参数：`git log *` 同时命中 "git log -5" 与 "git log"。
//     复合命令（a && b、a; b、管道）拆段逐判：任一段命中 deny 即 deny，
//     全部段命中 allow 才 allow（保守）；含命令替换（$(…) 或反引号）的段
//     永不命中 allow——防 `git log $(危险命令)` 借壳放行。
//   - read / write / edit：对 path 做同一套 glob。`*` 跨路径分隔符
//     （即天然含 `**` 语义），按工具收到的原样字符串匹配、不做绝对化。
//   - agent：对 subagent_type 做 glob。
//   - 其它工具（websearch、mcp__…、workflow 等）没有参数模式语义：
//     只有裸工具名规则能命中，带参数模式的规则一律不命中。
//
// 工具名本身也走 glob 且大小写不敏感：`mcp__github__*` 命中该 server 全部工具。
package permrules

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Decision 是规则评估的结论。
type Decision int

const (
	NoMatch Decision = iota // 三表均未命中：交回调用方按模式默认裁决
	Allow                   // 命中 allow：直接放行
	Ask                     // 命中 ask：强制人工确认（即使 yolo/auto）
	Deny                    // 命中 deny：直接拒绝
)

// String 给日志与测试失败信息用。
func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Ask:
		return "ask"
	case Deny:
		return "deny"
	default:
		return "no-match"
	}
}

// Rule 是一条已解析的规则：工具名（glob、忽略大小写）+ 可选参数模式。
type Rule struct {
	Tool    string // 已转小写
	Pattern string // 空 = 无参数模式（裸工具名规则）
	HasPat  bool   // 区分 `bash()`（空模式）与 `bash`（无模式）
}

// ParseRule 解析一条规则文本：`工具名` 或 `工具名(参数模式)`。
func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, errors.New("空规则")
	}
	i := strings.IndexByte(s, '(')
	if i < 0 {
		return Rule{Tool: strings.ToLower(s)}, nil
	}
	if i == 0 || !strings.HasSuffix(s, ")") {
		return Rule{}, fmt.Errorf("规则 %q 不是 工具名 或 工具名(参数模式) 形式", s)
	}
	return Rule{Tool: strings.ToLower(s[:i]), Pattern: s[i+1 : len(s)-1], HasPat: true}, nil
}

// Lists 是配置文件里的三表原文（config.json 的 permissions 字段
// 与项目级 .tokencode/permissions.json 共用此结构）。
type Lists struct {
	Allow []string `json:"allow"`
	Ask   []string `json:"ask"`
	Deny  []string `json:"deny"`
}

// Merge 合并两份三表（接收方在前）。三表是并集语义：deny 永远最高，
// 项目级无法放宽全局 deny，只能追加自己的 allow/ask/deny。
func (l Lists) Merge(other Lists) Lists {
	return Lists{
		Allow: append(append([]string{}, l.Allow...), other.Allow...),
		Ask:   append(append([]string{}, l.Ask...), other.Ask...),
		Deny:  append(append([]string{}, l.Deny...), other.Deny...),
	}
}

// Rules 是编译后的三表规则集。零值与 nil 均可用（恒 NoMatch）。
type Rules struct {
	allow, ask, deny []Rule
}

// Empty 报告规则集是否一条规则都没有（nil 安全）。
func (r *Rules) Empty() bool {
	return r == nil || len(r.allow)+len(r.ask)+len(r.deny) == 0
}

// Compile 把三表文本编译成规则集。坏规则跳过并收进 warns（一条坏规则
// 不应废掉整个规则集，但调用方应把 warns 提示给用户）。
func Compile(l Lists) (rules *Rules, warns []string) {
	r := &Rules{}
	build := func(specs []string, dst *[]Rule, table string) {
		for _, s := range specs {
			if strings.TrimSpace(s) == "" {
				continue
			}
			rule, err := ParseRule(s)
			if err != nil {
				warns = append(warns, fmt.Sprintf("permissions.%s: %v", table, err))
				continue
			}
			*dst = append(*dst, rule)
		}
	}
	build(l.Allow, &r.allow, "allow")
	build(l.Ask, &r.ask, "ask")
	build(l.Deny, &r.deny, "deny")
	return r, warns
}

// ProjectFile 是项目级规则文件的相对路径（与给 auto 裁决器的自然语言
// 规则 .tokencode/permissions.md 并存：.json 是硬规则，.md 是软提示）。
const ProjectFile = ".tokencode/permissions.json"

// LoadProject 读 dir 下的项目级三表。文件不存在返回零值（不是错误）。
func LoadProject(dir string) (Lists, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ProjectFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Lists{}, nil
	}
	if err != nil {
		return Lists{}, err
	}
	var l Lists
	if err := json.Unmarshal(raw, &l); err != nil {
		return Lists{}, fmt.Errorf("解析 %s: %w", ProjectFile, err)
	}
	return l, nil
}

// Load 合并全局三表与 dir 下的项目级三表并编译。项目级文件读不动/解析
// 失败降级为只用全局（进 warns），规则问题绝不阻塞启动。
func Load(dir string, global Lists) (*Rules, []string) {
	proj, err := LoadProject(dir)
	if err != nil {
		rules, warns := Compile(global)
		return rules, append(warns, err.Error())
	}
	return Compile(global.Merge(proj))
}

// Evaluate 评估一次工具调用。bash 走复合命令拆段逻辑，其余工具按
// deny > ask > allow 找第一张命中的表。
func (r *Rules) Evaluate(tool string, input json.RawMessage) Decision {
	if r.Empty() {
		return NoMatch
	}
	if strings.EqualFold(tool, "bash") {
		var v struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &v)
		return r.evaluateBash(tool, v.Command)
	}
	arg, hasArg := extractArg(tool, input)
	if matchAny(r.deny, tool, arg, hasArg) {
		return Deny
	}
	if matchAny(r.ask, tool, arg, hasArg) {
		return Ask
	}
	if matchAny(r.allow, tool, arg, hasArg) {
		return Allow
	}
	return NoMatch
}

// evaluateBash 对（可能复合的）命令拆段逐判：
// 任一段命中 deny → Deny；否则任一段命中 ask → Ask；
// 否则全部段命中 allow（且无命令替换）→ Allow；其余 NoMatch。
func (r *Rules) evaluateBash(tool, command string) Decision {
	segs := SplitCommand(command)
	if len(segs) == 0 {
		return NoMatch
	}
	anyAsk, allAllow := false, true
	for _, seg := range segs {
		if matchAny(r.deny, tool, seg, true) {
			return Deny
		}
		if matchAny(r.ask, tool, seg, true) {
			anyAsk = true
		}
		if risky(seg) || !matchAny(r.allow, tool, seg, true) {
			allAllow = false
		}
	}
	if anyAsk {
		return Ask
	}
	if allAllow {
		return Allow
	}
	return NoMatch
}

// matchAny 报告表中是否有规则命中。hasArg=false 表示该工具没有参数模式
// 语义（带模式的规则一律不命中）。
func matchAny(table []Rule, tool, arg string, hasArg bool) bool {
	lt := strings.ToLower(tool)
	for _, rule := range table {
		if !globMatch(rule.Tool, lt) {
			continue
		}
		if !rule.HasPat {
			return true // 裸工具名规则：命中工具即命中
		}
		if !hasArg {
			continue
		}
		if patternMatch(rule.Pattern, arg) {
			return true
		}
	}
	return false
}

// patternMatch 是参数模式匹配：glob，外加 CC 的零参数语义——
// 模式以 " *" 结尾时也命中去掉该尾部的精确形态（`git log *` 命中 "git log"）。
func patternMatch(pattern, arg string) bool {
	if globMatch(pattern, arg) {
		return true
	}
	if t, ok := strings.CutSuffix(pattern, " *"); ok && globMatch(t, arg) {
		return true
	}
	return false
}

// extractArg 取参数模式作用的字段。第二个返回值=false 表示该工具
// 没有参数模式语义。
func extractArg(tool string, input json.RawMessage) (string, bool) {
	switch strings.ToLower(tool) {
	case "read", "write", "edit":
		var v struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(input, &v)
		return v.Path, true
	case "agent":
		var v struct {
			SubagentType string `json:"subagent_type"`
		}
		_ = json.Unmarshal(input, &v)
		return v.SubagentType, true
	default:
		return "", false
	}
}

// risky 报告命令段是否含命令替换——这类段不允许命中 allow（仍可命中
// deny/ask）。不解析引号语义，单引号里的 $( 也算：保守的代价是多问一次。
func risky(seg string) bool {
	return strings.Contains(seg, "$(") || strings.Contains(seg, "`")
}

// SplitCommand 把复合命令拆成顺序段：在引号外按 && || ; | & 与换行切分，
// 段首尾空白剔除、空段丢弃。引号内的分隔符不切（含反斜杠转义）。
func SplitCommand(cmd string) []string {
	var segs []string
	var cur strings.Builder
	var quote byte // 0=不在引号内，否则是 ' 或 "
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segs = append(segs, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			if c == '\\' && quote == '"' && i+1 < len(cmd) {
				cur.WriteByte(c)
				i++
				cur.WriteByte(cmd[i])
				continue
			}
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case '\\':
			cur.WriteByte(c)
			if i+1 < len(cmd) {
				i++
				cur.WriteByte(cmd[i])
			}
		case ';', '\n':
			flush()
		case '|':
			flush()
			if i+1 < len(cmd) && cmd[i+1] == '|' {
				i++
			}
		case '&':
			flush()
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				i++
			}
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segs
}

// globMatch 是极简 glob：`*` 匹配任意（含空）字符序列，其余字符精确比较。
// 经典两指针 + 回溯实现，不支持 ? 与字符类。
func globMatch(pattern, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && pattern[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(pattern) && pattern[pi] == s[si]:
			pi++
			si++
		case star >= 0:
			mark++
			pi, si = star+1, mark
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
