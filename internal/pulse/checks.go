package pulse

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceCheck 监测工作区文件自上一拍后是否有变化（mtime/大小/文件集指纹）。
// 首拍只建立基线不报告。选 mtime 扫描而非 git status：porcelain 输出对
// 「已脏文件再次被改」不敏感，指纹反而漏报。
func WorkspaceCheck(dir string) Check {
	var last string
	first := true
	return func() string {
		cur := mtimeFingerprint(dir)
		changed := cur != last
		last = cur
		if first {
			first = false
			return ""
		}
		if changed {
			return "工作区文件自上一拍后有改动"
		}
		return ""
	}
}

// mtimeFingerprint 把目录下所有文件的 路径|大小|mtime 揉成一个指纹。
// 跳过隐藏目录（.git、.tokencode 等）与 node_modules，避免扫描噪声和巨树。
func mtimeFingerprint(dir string) string {
	h := fnv.New64a()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path == dir {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := d.Info(); err == nil {
			fmt.Fprintf(h, "%s|%d|%d;", path, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	return fmt.Sprintf("%x", h.Sum64())
}

// TodoCheck 在待办文件非空（按内容 trim 后）时报告。
func TodoCheck(path string) Check {
	return func() string {
		b, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return ""
		}
		return fmt.Sprintf("待办文件 %s 非空，确认是否有可推进的事项", path)
	}
}
