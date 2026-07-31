package main

import (
	"encoding/json"
	"testing"

	bs "github.com/rasimio/blueship"
)

func chatReq(msgs string) openaiChatRequest {
	var req openaiChatRequest
	if err := json.Unmarshal([]byte(`{"model":"claude-sonnet-5","messages":`+msgs+`}`), &req); err != nil {
		panic(err)
	}
	return req
}

func blocks(t *testing.T, m bs.Message) []bs.ContentBlock {
	t.Helper()
	b, ok := m.Content.([]bs.ContentBlock)
	if !ok {
		t.Fatalf("message content is %T, want []ContentBlock", m.Content)
	}
	return b
}

// Parallel tool_calls arrive as several consecutive tool-role messages —
// they must fold into ONE user turn with all tool_results, results first.
func TestParallelToolResultsMergeIntoOneUserTurn(t *testing.T) {
	req := chatReq(`[
	 {"role":"user","content":"hi"},
	 {"role":"assistant","content":null,"tool_calls":[
	   {"id":"tu_1","type":"function","function":{"name":"a","arguments":"{}"}},
	   {"id":"tu_2","type":"function","function":{"name":"b","arguments":"{}"}}]},
	 {"role":"tool","tool_call_id":"tu_1","content":"r1"},
	 {"role":"tool","tool_call_id":"tu_2","content":"r2"},
	 {"role":"user","content":"and?"}]`)
	out, err := openaiToBlueship(req, "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages (user, assistant, merged user), got %d: %+v", len(out.Messages), out.Messages)
	}
	last := blocks(t, out.Messages[2])
	if len(last) != 3 || last[0].Type != "tool_result" || last[1].Type != "tool_result" || last[2].Type != "text" {
		t.Fatalf("merged user turn wrong: %+v", last)
	}
	ids := map[string]bool{last[0].ToolUseID: true, last[1].ToolUseID: true}
	if !ids["tu_1"] || !ids["tu_2"] {
		t.Fatalf("tool_result ids wrong: %+v", ids)
	}
}

// A tool_use whose result was clipped by history trimming gets a
// synthetic tool_result so Anthropic doesn't 400.
func TestMissingToolResultSynthesized(t *testing.T) {
	req := chatReq(`[
	 {"role":"user","content":"hi"},
	 {"role":"assistant","content":null,"tool_calls":[
	   {"id":"tu_1","type":"function","function":{"name":"a","arguments":"{}"}}]},
	 {"role":"user","content":"continue"}]`)
	out, err := openaiToBlueship(req, "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	next := blocks(t, out.Messages[2])
	if next[0].Type != "tool_result" || next[0].ToolUseID != "tu_1" {
		t.Fatalf("synthetic tool_result missing: %+v", next)
	}
}

// tool_use as the very last message also gets a synthetic result turn.
func TestTrailingToolUseGetsResultTurn(t *testing.T) {
	req := chatReq(`[
	 {"role":"user","content":"hi"},
	 {"role":"assistant","content":null,"tool_calls":[
	   {"id":"tu_9","type":"function","function":{"name":"a","arguments":"{}"}}]}]`)
	out, err := openaiToBlueship(req, "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 || out.Messages[2].Role != "user" {
		t.Fatalf("want synthetic trailing user turn, got %+v", out.Messages)
	}
	if b := blocks(t, out.Messages[2]); b[0].Type != "tool_result" || b[0].ToolUseID != "tu_9" {
		t.Fatalf("synthetic result wrong: %+v", b)
	}
}

// An orphan tool_result (its tool_use fell out of the window) is demoted
// to text instead of tripping the API.
func TestOrphanToolResultDemotedToText(t *testing.T) {
	req := chatReq(`[
	 {"role":"tool","tool_call_id":"tu_gone","content":"stale result"},
	 {"role":"user","content":"hi"}]`)
	out, err := openaiToBlueship(req, "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	first := blocks(t, out.Messages[0])
	for _, b := range first {
		if b.Type == "tool_result" {
			t.Fatalf("orphan tool_result must be demoted: %+v", first)
		}
	}
}
