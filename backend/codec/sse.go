package codec

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// Frame 是一个 SSE 事件。Event 为空表示上游没发 event 行
// （Gemini 的 alt=sse 与部分 Chat Completions 实现就是这样），
// 此时解码器只能靠 data 的内容判断帧类型。
type Frame struct {
	Event string
	Data  string
}

// FrameScanner 按 SSE 规范切帧：空行分隔事件，`field: value` 形式的字段行，
// 同一事件内多个 data 行以换行拼接。
//
// 不用 bufio.Scanner 是因为它默认 64KB 行上限，而单个 data 行可以很长
// （responses 的 completed 帧带完整响应，工具入参也可能很大）。
type FrameScanner struct {
	r     *bufio.Reader
	frame Frame
	data  strings.Builder
	err   error
}

// maxFrameBytes 是单帧上限，防御上游不断发数据却不发空行导致内存无界增长。
const maxFrameBytes = 32 << 20

func NewFrameScanner(r io.Reader) *FrameScanner {
	return &FrameScanner{r: bufio.NewReaderSize(r, 64<<10)}
}

// Scan 读出下一帧。返回 false 时用 Err 区分正常结束与出错。
func (s *FrameScanner) Scan() bool {
	if s.err != nil {
		return false
	}
	s.frame = Frame{}
	s.data.Reset()
	sawField := false

	for {
		line, err := s.readLine()
		if err != nil {
			s.err = err
			// 流在半帧处断开：已读到的字段仍要交出去，
			// 否则最后一帧（可能是终止帧）会被丢掉。
			if sawField {
				s.finishFrame()
				return true
			}
			return false
		}

		if line == "" {
			if !sawField {
				// 连续空行或前导空行，跳过。
				continue
			}
			s.finishFrame()
			return true
		}

		// 注释行，SSE 规范里用于保活。
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value := splitField(line)
		switch field {
		case "event":
			s.frame.Event = value
			sawField = true
		case "data":
			if s.data.Len() > 0 {
				s.data.WriteByte('\n')
			}
			s.data.WriteString(value)
			sawField = true
		default:
			// id / retry 等字段本服务不用，但它们的出现说明帧已开始。
			sawField = true
		}

		if s.data.Len() > maxFrameBytes {
			s.err = io.ErrShortBuffer
			return false
		}
	}
}

func (s *FrameScanner) finishFrame() {
	s.frame.Data = s.data.String()
}

func (s *FrameScanner) Frame() Frame { return s.frame }

// Err 返回终止原因；正常读完为 nil。
func (s *FrameScanner) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}

// readLine 读一行并去掉行尾的 CR/LF。
func (s *FrameScanner) readLine() (string, error) {
	line, err := s.r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// splitField 切 `field: value`。按 SSE 规范，冒号后的单个空格要去掉，
// 更多空格保留；无冒号时整行是字段名、值为空。
func splitField(line string) (string, string) {
	name, value, found := strings.Cut(line, ":")
	if !found {
		return name, ""
	}
	return name, strings.TrimPrefix(value, " ")
}

// EncodeFrame 编码一帧 SSE。event 为空则不写 event 行。
// data 内含换行时拆成多个 data 行，否则客户端会把换行当帧边界。
func EncodeFrame(event string, data []byte) []byte {
	var buf bytes.Buffer
	if event != "" {
		buf.WriteString("event: ")
		buf.WriteString(event)
		buf.WriteByte('\n')
	}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}
