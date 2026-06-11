package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// workspaceRoot 是全局工作空间隔离的根目录。空=不限制。
// 启动时设置一次（-workspace），运行期间只读，无需加锁。
var workspaceRoot string

// SetWorkspace 开启全局工作空间隔离：文件类工具（read/write/edit）只允许
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

// WorkspaceRoot 返回当前全局工作空间根（空=未开启）。
func WorkspaceRoot() string { return workspaceRoot }

// ctxRootKey 是 per-agent 工具根在 ctx 里的键。与全局 workspace 不同，
// 它是每次执行的属性：Registry.Execute 按自身 Root 注入，竞赛等多写空间
// 场景下不同 agent 的根互不相同。
type ctxRootKey struct{}

// WithRoot 把工具根写进 ctx：文件工具的相对路径基于它解析、访问被守卫
// 在它之内，bash 在它之下执行。Registry.Execute 自动注入，一般无需手调。
func WithRoot(ctx context.Context, root string) context.Context {
	if root == "" {
		return ctx
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ctx
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	return context.WithValue(ctx, ctxRootKey{}, abs)
}

// rootFrom 取 ctx 里的工具根（空=未设置）。
func rootFrom(ctx context.Context) string {
	r, _ := ctx.Value(ctxRootKey{}).(string)
	return r
}

// ctxCheckpointKey 是写盘前快照钩子在 ctx 里的键（Registry.Execute 注入，
// 已绑定工具名）。
type ctxCheckpointKey struct{}

// withCheckpoint 把快照钩子写进 ctx。
func withCheckpoint(ctx context.Context, fn func(path string)) context.Context {
	return context.WithValue(ctx, ctxCheckpointKey{}, fn)
}

// notifyCheckpoint 在覆盖/创建文件前回调快照钩子（未挂载时零开销）。
// path 必须是 resolvePath 之后的绝对路径。
func notifyCheckpoint(ctx context.Context, path string) {
	if fn, _ := ctx.Value(ctxCheckpointKey{}).(func(string)); fn != nil {
		fn(path)
	}
}

// resolvePath 把工具收到的路径解析为绝对路径并做隔离校验：
// 相对路径基于 ctx 根（无根则进程 cwd）解析；结果必须同时落在
// ctx 根与全局 workspace 之内（各自未开启则不约束）。
func resolvePath(ctx context.Context, path string) (string, error) {
	root := rootFrom(ctx)
	var abs string
	if filepath.IsAbs(path) {
		abs = filepath.Clean(path)
	} else {
		base := root
		if base == "" {
			var err error
			if base, err = os.Getwd(); err != nil {
				return "", fmt.Errorf("解析路径 %s: %w", path, err)
			}
		}
		abs = filepath.Join(base, path)
	}
	if root != "" {
		if err := checkWithin(abs, root, "agent 工作目录"); err != nil {
			return "", err
		}
	}
	if workspaceRoot != "" {
		if err := checkWithin(abs, workspaceRoot, "工作空间"); err != nil {
			return "", err
		}
	}
	return abs, nil
}

// checkWithin 校验 abs（符号链接解析后）落在 root 之内。
func checkWithin(abs, root, what string) error {
	resolved := resolveExisting(abs)
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return fmt.Errorf("%s隔离：拒绝访问 %s（仅允许 %s 之内）", what, abs, root)
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
