package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// workspaceRoot 是工作空间隔离的根目录。空=不限制。
// 启动时设置一次（-workspace），运行期间只读，无需加锁。
var workspaceRoot string

// SetWorkspace 开启工作空间隔离：文件类工具（read/write/edit）只允许
// 访问 root 之内的路径，符号链接逃逸也会被解析后拦截。
// 注意 bash 无法静态约束——它受权限确认机制管，不受此守卫管。
func SetWorkspace(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	workspaceRoot = abs
	return nil
}

// WorkspaceRoot 返回当前工作空间根（空=未开启）。
func WorkspaceRoot() string { return workspaceRoot }

// guardPath 校验一个路径在工作空间内。未开启时恒放行。
func guardPath(path string) error {
	if workspaceRoot == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("workspace: 解析路径 %s: %w", path, err)
	}
	resolved := resolveExisting(abs)
	if resolved != workspaceRoot && !strings.HasPrefix(resolved, workspaceRoot+string(filepath.Separator)) {
		return fmt.Errorf("工作空间模式：拒绝访问 %s（仅允许 %s 之内）", path, workspaceRoot)
	}
	return nil
}

// resolveExisting 解析符号链接。目标可能还不存在（write 新文件），
// 此时解析最近的已存在祖先，再把未存在的尾段拼回去——
// 防止经由「指向外部的已存在目录链接」创建文件逃逸。
func resolveExisting(abs string) string {
	p := abs
	var rest []string
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				r = filepath.Join(r, rest[i])
			}
			return r
		}
		parent := filepath.Dir(p)
		if parent == p {
			return abs // 一路到根都不存在：按字面路径判定
		}
		rest = append(rest, filepath.Base(p))
		p = parent
	}
}
