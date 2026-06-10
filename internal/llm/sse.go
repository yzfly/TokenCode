package llm

import (
	"bufio"
	"io"
	"strings"
)

// readSSE 逐事件读取一条 SSE 流，把每个事件的 data 载荷交给 onData。
// 只关心 data 字段（多行 data 按规范以 \n 连接），注释与其它字段忽略；
// onData 返回 error 即中止。流自然结束（EOF）返回 nil。
func readSSE(r io.Reader, onData func(data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 工具参数可能很大

	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := data.String()
		data.Reset()
		return onData(payload)
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" { // 空行 = 事件边界
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(rest, " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush() // 流末尾可能没有结尾空行
}
