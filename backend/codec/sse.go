package codec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aceaura/model-surge-agent/backend/ir"
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
			// id / retry 本服务不用，但它们的出现说明帧已开始。
			// 名字不在这几个之内的行不算帧的开始，理由见 knownSSEField。
			if knownSSEField(field) {
				sawField = true
			}
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

// maxLineBytes 是单行读入的上限：帧上限管的是 data 内容，行长还要容纳
// 「data:」前缀与行尾 CRLF 这些框架开销，故比 maxFrameBytes 宽两字节。
// 超过即拒：单行是帧的组成部分，没有理由允许它比整帧大出一个量级。
const maxLineBytes = maxFrameBytes + 2

// readLine 读一行并去掉行尾的 CR/LF。
//
// 不用 ReadString 是因为它读到换行为止、没有任何长度上限：上游发一条
// 不带换行的巨行时内存在 maxFrameBytes 检查（只对已切成行的 data 生效）
// 介入之前先无界增长。手工按 ReadSlice 分块累积并设 maxLineBytes 上限。
//
// EOF 契约与 ReadString 一致：半行内容先交出去（err 为 nil），
// 下一次调用才报 EOF，Scan 借此把流断开处的最后一帧交给调用方。
func (s *FrameScanner) readLine() (string, error) {
	var buf []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLineBytes {
			return "", io.ErrShortBuffer
		}
		switch {
		case err == nil:
			return strings.TrimRight(string(buf), "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			// 行超过读缓冲（64KB），继续攒下一段。
			continue
		case len(chunk) == 0 && len(buf) == 0:
			return "", err
		default:
			// 底层读出错但已攒到内容（典型是 EOF 前的半行，或前几段
			// 撑满读缓冲后 EOF 交回空段）：先交行，错误留到下一次调用
			// 再报——与原 ReadString 的契约一致。
			return strings.TrimRight(string(buf), "\r\n"), nil
		}
	}
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

// knownSSEField 判断字段名是否是 SSE 规范定义的四个之一。
//
// 需要这个白名单是因为「未知字段也算帧已开始」那条宽容会被一行裸 JSON
// 骗过去：`{"type":"message"}` 含冒号，切出来的字段名是 `{"type"`，于是
// 上游忽略流式请求回的一整份 JSON 看起来像一个合法的空 data 帧，
// 「上游一帧都没发」的截断保护因此失效，解码器还会给它补一个终止事件，
// 客户端最终拿到一个静默的空答案。
//
// 规范说未知字段应被忽略——白名单正是在忽略它们，只是不再让它们
// 充当「帧已开始」的证据。
func knownSSEField(name string) bool {
	switch name {
	case "event", "data", "id", "retry":
		return true
	default:
		return false
	}
}

// maxJSONDocsPerLine 是单行允许拆出的文档份数上限。
//
// 16 取自实际见过的形态：兼容层攒帧时最多把一个 HTTP 读缓冲里的几帧粘在
// 一起，远不到两位数。设上限是为了让畸形输入（比如一行里几万个 {}）
// 在这里就止住，而不是解出几万个事件送进下游。
const maxJSONDocsPerLine = 16

// maxJSONLineBytes 是参与拆分的单行总长上限，与 maxFrameBytes 同量级但更紧：
// 需要拆分的行本身就是异常形态，没有理由允许它比正常帧更大。
const maxJSONLineBytes = 16 << 20

// MultipleJSONDocsNote 是一行里多个 JSON 文档被拆开处理的说明。
const MultipleJSONDocsNote = "split a stream line that carried several JSON documents"

// UpstreamIgnoredStreamNote 是上游忽略流式请求、回了一整份响应的说明。
//
// 记它是因为逐字输出这一项确实丢了：客户端要的是流，拿到的是一次性到齐的
// 全部内容。内容本身没损失，所以不是错误，但客户端有权知道。
const UpstreamIgnoredStreamNote = "upstream ignored the streaming request and returned a whole response"

// SplitJSONDocuments 把一行里首尾相接的多个 JSON 文档拆成若干份。
//
// 只在整帧解码失败后调用，不做无条件前置拆分：正常帧一行一个文档，
// 无条件先拆会给每一帧加一次解析，而流式路径上每帧都要过这里。
//
// ok 为假表示这行不是「多个合法文档相接」的形态——可能只有一份、
// 也可能真的坏了。调用方据此走原有错误路径，不要吞掉错误。
func SplitJSONDocuments(data string) ([]string, bool) {
	if len(data) > maxJSONLineBytes {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(data))
	var out []string
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		out = append(out, string(raw))
		if len(out) > maxJSONDocsPerLine {
			return nil, false
		}
	}
	// 一份或零份都不算「多文档行」：一份说明原始解码失败另有原因，
	// 报成拆分成功会把真正的错误藏起来。
	if len(out) < 2 {
		return nil, false
	}
	return out, true
}

// ErrSkipFrame 标记一帧「结构上不可解但可安全跳过」：外层 JSON 都解不开，
// 取不出任何 type 与正文。SSE 以事件边界自同步，坏一帧不污染后续帧——跳过
// 续流比终止整流更保内容，故归结构损坏而非内容损坏。
//
// 与 FeedWithSplit 的契约：解码器的 feedOne 在「帧外层解不开」时返回包裹了
// 本哨兵的错误（而不是直接吞掉），FeedWithSplit 据此仍会尝试把一行里首尾相接
// 的多文档拆开；只有拆开也无望（整行确属坏帧）时，哨兵才会原样回到解码器的
// Feed，由 Feed 计数后吞成 (nil, nil)，读流循环因此无须改动地续流。
//
// 内容损坏帧（认得出的事件、载荷语义坏了）不包裹本哨兵，照常报错终止——
// 那是 fail-fast 的对象，跳过去会让客户端收到半截却看不出丢了东西的内容。
var ErrSkipFrame = errors.New("skippable malformed stream frame")

// ClassifyBadFrame 把「帧外层 JSON 解不开」的错误按可跳过性分类，供四个解码器
// 的 feedOne 共用。判据落在「有没有一个完整文档已经解出来」：解出来了就意味着
// 这一行带着正文，丢掉它而不报错正是本仓最忌讳的静默缺失。
//
//   - 整行连第一个 JSON 文档都解不开（纯垃圾帧）：结构损坏，取不出任何正文，
//     返回包裹 ErrSkipFrame 的错误。FeedWithSplit 拆它也无望，哨兵原样回到
//     Feed，由 Feed 计数后吞成 (nil, nil) 跳帧续流。
//   - 行首有完整文档、其后跟着解不开的残余（残缺多文档行）：返回内容损坏错误
//     （不包裹哨兵），fail-fast 终止整流。唯一例外是残余其实也是一个完整文档——
//     那时 FeedWithSplit 会把整行干净拆开、各自可解，这个错误根本不会浮出来。
func ClassifyBadFrame(err error, data string) error {
	if hasLeadingJSONDocument(data) {
		return ir.NewError(ir.ErrUpstream, 0, "",
			fmt.Sprintf("undecodable stream frame: %v", err))
	}
	return fmt.Errorf("%w: %v", ErrSkipFrame, err)
}

// hasLeadingJSONDocument 报告 data 是否以一个完整 JSON 文档开头、且其后还有
// 非空白残余。两者都成立才是「残缺多文档行」；纯垃圾（第一个文档就解不开）
// 与规规矩矩的单文档（无残余，本就轮不到这里报错）都返回 false。
func hasLeadingJSONDocument(data string) bool {
	dec := json.NewDecoder(strings.NewReader(data))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return false
	}
	return strings.TrimSpace(data[dec.InputOffset():]) != ""
}

// FeedWithSplit 是四个解码器共用的「先按单文档解，失败再试拆分」流程。
//
// 抽到这里而不是各自写一遍：四份同样的回退逻辑会各自漂移，而其中任何
// 一份漏掉上报说明，客户端就在不知情的情况下收到被我们重组过的内容。
//
// split 为真表示确实拆了，调用方据此记说明。
func FeedWithSplit(event, data string, feed func(event, data string) ([]ir.Event, error),
) (events []ir.Event, split bool, err error) {

	out, err := feed(event, data)
	if err == nil {
		return out, false, nil
	}
	docs, ok := SplitJSONDocuments(strings.TrimSpace(data))
	if !ok {
		return nil, false, err
	}
	var all []ir.Event
	for _, doc := range docs {
		// 拆出的某一份解不动仍算整行失败：部分解码会让客户端收到半截内容，
		// 却看不出后面丢了东西。
		part, docErr := feed(event, doc)
		if docErr != nil {
			return nil, false, docErr
		}
		all = append(all, part...)
	}
	return all, true, nil
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
