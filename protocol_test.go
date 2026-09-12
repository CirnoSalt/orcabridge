package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCleanAnswerPreservesMarkdownAndKeywords(t *testing.T) {
	in := "# Lighthouse 周年运维清单\n\n- 查看日志\n- 执行命令\n\n```go\nfmt.Println(\"<think>保留</think>\")\n```"
	if got := cleanAnswer(in); got != in {
		t.Fatalf("正常 Markdown 被改写:\n got=%q\nwant=%q", got, in)
	}
}

func TestCleanAnswerOnlyStripsTrustedEnvelope(t *testing.T) {
	in := "正文\n```json\n{\"action\":\"completion\",\"taskCompletion\":\"内部答案\"}\n```\n```json\n{\"action\":\"deploy\",\"taskCompletion\":\"普通示例\"}\n```"
	want := "正文\n\n```json\n{\"action\":\"deploy\",\"taskCompletion\":\"普通示例\"}\n```"
	if got := cleanAnswer(in); got != want {
		t.Fatalf("包络清洗错误:\n got=%q\nwant=%q", got, want)
	}
}

func TestStripThoughtsFenceAndUnclosed(t *testing.T) {
	fenced := "```text\n<think>示例</think>\n```"
	if got := stripThoughts(fenced); got != fenced {
		t.Fatalf("围栏内 think 被删除: %q", got)
	}
	unclosed := "答案前缀<think>未闭合内容"
	if got := stripThoughts(unclosed); got != unclosed {
		t.Fatalf("未闭合 think 导致截断: %q", got)
	}
	if got := stripThoughts("a<think>secret</think>b"); got != "ab" {
		t.Fatalf("闭合 think 未删除: %q", got)
	}
}

func TestSSEEventReader(t *testing.T) {
	reader := NewSSEEventReader(strings.NewReader(": ping\r\ndata: first\r\ndata: second\r\n\r\ndata: tail"), 64)
	data, done, err := reader.Next()
	if err != nil || done || data != "first\nsecond" {
		t.Fatalf("多 data/CRLF 解析错误: data=%q done=%v err=%v", data, done, err)
	}
	data, done, err = reader.Next()
	if err != nil || done || data != "tail" {
		t.Fatalf("EOF 尾事件解析错误: data=%q done=%v err=%v", data, done, err)
	}
	_, done, err = reader.Next()
	if err != nil || !done {
		t.Fatalf("EOF 未结束: done=%v err=%v", done, err)
	}

	doneReader := NewSSEEventReader(strings.NewReader("data: [DONE]\n\n"), 64)
	if data, done, err := doneReader.Next(); err != nil || !done || data != "" {
		t.Fatalf("DONE 解析错误: data=%q done=%v err=%v", data, done, err)
	}
	limited := NewSSEEventReader(strings.NewReader("data: 12345\n\n"), 4)
	if _, _, err := limited.Next(); err == nil {
		t.Fatal("超过事件上限未报错")
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func TestSSEEventReaderReadError(t *testing.T) {
	want := errors.New("boom")
	reader := NewSSEEventReader(&failingReader{data: []byte("data: partial"), err: want}, 64)
	if _, _, err := reader.Next(); !errors.Is(err, want) {
		t.Fatalf("读取错误未透传: %v", err)
	}
}

func TestParseRawUpstream(t *testing.T) {
	stream := "data: {\"type\":\"ai\",\"content\":\"```json\\n{\\\"action\\\":\\\"completion\\\",\"}\n\n" +
		"data: {\"type\":\"ai\",\"content\":\"\\\"taskCompletion\\\":\\\"答案\\\"}\\n```\"}\n\n" +
		"data: [DONE]\n\n"
	outcome, err := ParseRawUpstream(strings.NewReader(stream), 1024, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != "completion" || outcome.Text != "答案" {
		t.Fatalf("raw outcome 错误: %+v", outcome)
	}

	badCode := "data: {\"code\":7,\"message\":\"denied\"}\n\n"
	if _, err := ParseRawUpstream(strings.NewReader(badCode), 1024, 4096); err == nil {
		t.Fatal("上游非零 code 未报错")
	}
	badJSON := "data: not-json\n\n"
	if _, err := ParseRawUpstream(strings.NewReader(badJSON), 1024, 4096); err == nil {
		t.Fatal("损坏帧未报错")
	}
}

func TestOutcomeValidation(t *testing.T) {
	valid := &upstreamResp{Data: &upstreamData{Action: "review", Tool: &UpstreamTool{Name: "fetch", Args: []byte(`{"url":"x"}`)}}}
	outcome, err := outcomeFromStructured(valid, "raw")
	if err != nil || len(outcome.ToolCalls()) != 1 {
		t.Fatalf("合法 review 未通过: outcome=%+v err=%v", outcome, err)
	}
	cases := []*upstreamResp{
		{Code: 3, Message: "bad"},
		{Data: &upstreamData{Action: "unknown"}},
		{Data: &upstreamData{Action: "completion"}},
		{Data: &upstreamData{Action: "review", Tool: &UpstreamTool{Name: "fetch", Args: []byte(`[]`)}}},
	}
	for i, c := range cases {
		if _, err := outcomeFromStructured(c, "raw"); err == nil {
			t.Errorf("非法结果 %d 未报错", i)
		}
	}
}

func TestReadLimitedErrorAndLimit(t *testing.T) {
	want := errors.New("read failed")
	if _, err := readLimited(&failingReader{err: want}, 10); !errors.Is(err, want) {
		t.Fatalf("readLimited 未返回读取错误: %v", err)
	}
	if _, err := readLimited(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("readLimited 超限未报错")
	}
	if got, err := readLimited(strings.NewReader("1234"), 4); err != nil || got != "1234" {
		t.Fatalf("边界读取失败: got=%q err=%v", got, err)
	}
}

var _ io.Reader = (*failingReader)(nil)
