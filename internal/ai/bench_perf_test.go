package ai

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

// Benchmarks for the P0 performance optimizations: token estimation, SSE
// parsing, and streaming text accumulation. Each has a *_Old counterpart that
// reproduces the pre-optimization implementation for comparison.

// longText is a representative chunk of a long assistant reply (mix of ASCII
// code and CJK prose, as in real coding sessions).
var longText = strings.Repeat("func main() { fmt.Println(\"你好，这里是说明文字\") } // 注释行\n", 200)

func BenchmarkEstimateTextTokens(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = EstimateTextTokens(longText)
	}
}

// BenchmarkEstimateTextTokensOld is the pre-optimization implementation
// (len([]rune)).
func BenchmarkEstimateTextTokensOld(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = (len([]rune(longText)) + charsPerToken - 1) / charsPerToken
	}
}

func BenchmarkEstimateMessageTokens(b *testing.B) {
	msg := &Message{Role: RoleAssistant, Content: []ContentBlock{
		{Type: ContentTypeThinking, Thinking: strings.Repeat("思考过程若干。", 200)},
		{Type: ContentTypeText, Text: longText},
	}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EstimateMessageTokens(msg)
	}
}

// sseStream is a realistic sequence of SSE events: a few deltas, then done.
func sseStream() []byte {
	var buf bytes.Buffer
	write := func(ev, data string) {
		if ev != "" {
			buf.WriteString("event: " + ev + "\n")
		}
		for _, l := range strings.Split(data, "\n") {
			buf.WriteString("data: " + l + "\n")
		}
		buf.WriteString("\n")
	}
	write("response.start", `{"type":"response.start"}`)
	for i := 0; i < 50; i++ {
		write("", `{"type":"response.output_text.delta","delta":"func main() { fmt.Println(\"你好，世界\") } // 代码行\n"}`)
	}
	write("", `{"type":"response.completed","usage":{"total_tokens":1234}}`)
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

func BenchmarkIterateSSE(b *testing.B) {
	stream := sseStream()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		if err := IterateSSE(bytes.NewReader(stream), func(ev SSEEvent) error { count++; return nil }); err != nil || count == 0 {
			b.Fatal("bad parse")
		}
	}
}

// BenchmarkIterateSSEOld is the pre-optimization implementation (ReadString
// + strings).
func BenchmarkIterateSSEOld(b *testing.B) {
	stream := sseStream()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		if err := iterateSSEReadString(bytes.NewReader(stream), func(ev SSEEvent) error { count++; return nil }); err != nil || count == 0 {
			b.Fatal("bad parse")
		}
	}
}

func BenchmarkStreamAccumBuilder(b *testing.B) {
	deltas := []string{"func", " ", "main", "(", ")", " ", "// ", "你好，", "世界"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var acc textAccum
		var s string
		for _, d := range deltas {
			s = acc.Append(d)
		}
		if s == "" {
			b.Fatal("empty")
		}
	}
}

// BenchmarkStreamAccumConcat is the pre-optimization pattern (s += d).
func BenchmarkStreamAccumConcat(b *testing.B) {
	deltas := []string{"func", " ", "main", "(", ")", " ", "// ", "你好，", "世界"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s string
		for _, d := range deltas {
			s += d
		}
		if s == "" {
			b.Fatal("empty")
		}
	}
}

// iterateSSEReadString reproduces the pre-optimization IterateSSE exactly.
func iterateSSEReadString(r io.Reader, fn func(ev SSEEvent) error) error {
	br := bufio.NewReader(r)
	var eventName string
	var data []byte

	dispatch := func() error {
		if len(data) == 0 && eventName == "" {
			return nil
		}
		ev := SSEEvent{Event: eventName, Data: data}
		eventName = ""
		data = nil
		return fn(ev)
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			if line != "" && line[0] == ':' {
				// Comment; ignore.
			} else if strings.HasPrefix(line, "event:") {
				eventName = strings.TrimSpace(line[len("event:"):])
			} else if strings.HasPrefix(line, "data:") {
				value := strings.TrimPrefix(line, "data:")
				value = strings.TrimPrefix(value, " ")
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, value...)
			} else if line == "" {
				if err := dispatch(); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return dispatch()
			}
			return err
		}
	}
}
