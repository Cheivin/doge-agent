package tools

import "testing"

// TestOperationsSessionIsolation verifies §8.5.1: session identity and the
// code-tool caller ride on per-session Operations and survive WithCwd
// wrapping, so two concurrent sessions never leak them into each other.
func TestOperationsSessionIsolation(t *testing.T) {
	opsA := LocalOperations()
	opsA.SessionID = "sess-A"
	opsA.SessionFile = "/x/sess-A.jsonl"
	opsA.CodeCaller = func(name string, args map[string]any) (string, bool, error) { return "A", false, nil }

	opsB := LocalOperations()
	opsB.SessionID = "sess-B"
	opsB.CodeCaller = func(name string, args map[string]any) (string, bool, error) { return "B", false, nil }

	cell := NewCwdCell("/tmp")
	wa := WithCwd(opsA, cell)
	wb := WithCwd(opsB, cell)

	if wa.SessionID != "sess-A" || wb.SessionID != "sess-B" {
		t.Fatalf("session id leaked across wrapper: A=%q B=%q", wa.SessionID, wb.SessionID)
	}
	if wa.SessionFile != "/x/sess-A.jsonl" {
		t.Fatalf("session file not carried: %q", wa.SessionFile)
	}
	if wa.CodeCaller == nil || wb.CodeCaller == nil {
		t.Fatal("code caller not carried through WithCwd")
	}
	out, isErr, err := wa.CodeCaller("read", nil)
	if err != nil || isErr || out != "A" {
		t.Fatalf("A caller routed wrong: %q isErr=%v err=%v", out, isErr, err)
	}
	out, _, _ = wb.CodeCaller("read", nil)
	if out != "B" {
		t.Fatalf("B caller routed wrong: %q", out)
	}
}
