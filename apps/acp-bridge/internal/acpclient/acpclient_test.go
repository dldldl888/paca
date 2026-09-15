package acpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

const testTimeout = 5 * time.Second

// fakePeer emulates the far end of the stdio connection under test: it
// reads the NDJSON lines the Client under test writes (its "stdin") and
// lets the test script responses/notifications back, which the Client
// reads as its "stdout". Standing in for a real ACP-speaking subprocess so
// these tests run fast and in-process — see newTestClient.
//
// A dedicated goroutine (started by newTestClient) owns the actual pipe
// reader and forwards each line onto lines; when the Client closes its
// stdin (e.g. via Close), that goroutine sees EOF and closes the peer's
// output too — mirroring a real subprocess that exits (and so closes its
// own stdout) once its stdin closes. Without this cascade, Close wouldn't
// actually unblock anything in a test the way it does against a real
// subprocess.
type fakePeer struct {
	t     *testing.T
	lines chan []byte
	w     io.WriteCloser
}

type inLine struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// next reads and decodes the next line the Client sent as a request or
// notification (i.e. it has a method).
func (p *fakePeer) next() inLine {
	p.t.Helper()
	raw := p.rawLine()
	var msg inLine
	if err := json.Unmarshal(raw, &msg); err != nil {
		p.t.Fatalf("fakePeer: decoding line %q: %v", raw, err)
	}
	return msg
}

func (p *fakePeer) rawLine() []byte {
	p.t.Helper()
	select {
	case raw, ok := <-p.lines:
		if !ok {
			p.t.Fatalf("fakePeer: no more input (client closed its stdin)")
		}
		return raw
	case <-time.After(testTimeout):
		p.t.Fatalf("fakePeer: timed out waiting for a line from the client")
		return nil
	}
}

func (p *fakePeer) respond(id json.RawMessage, result any) {
	p.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func (p *fakePeer) notify(method string, params any) {
	p.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (p *fakePeer) write(v any) {
	p.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		p.t.Fatalf("fakePeer: encoding: %v", err)
	}
	b = append(b, '\n')
	if _, err := p.w.Write(b); err != nil {
		p.t.Fatalf("fakePeer: writing: %v", err)
	}
}

func newTestClient(t *testing.T, onUpdate func(Update)) (*Client, *fakePeer) {
	t.Helper()
	clientStdoutR, peerStdoutW := io.Pipe()
	peerStdinR, clientStdinW := io.Pipe()

	lines := make(chan []byte, 64)
	go func() {
		defer close(lines)
		defer func() { _ = peerStdoutW.Close() }()
		scanner := bufio.NewScanner(peerStdinR)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			lines <- append([]byte(nil), scanner.Bytes()...)
		}
	}()

	peer := &fakePeer{t: t, lines: lines, w: peerStdoutW}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(clientStdinW, clientStdoutR, func() error { return nil }, func() {}, onUpdate, log)

	t.Cleanup(func() {
		_ = clientStdinW.Close()
		_ = peerStdoutW.Close()
	})
	return c, peer
}

func withTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

func TestInitializeSendsProtocolVersionAndDeclinesFsTerminal(t *testing.T) {
	c, peer := newTestClient(t, nil)
	ctx := withTimeout(t)

	errCh := make(chan error, 1)
	go func() { errCh <- c.Initialize(ctx) }()

	req := peer.next()
	if req.Method != "initialize" {
		t.Fatalf("method = %q, want %q", req.Method, "initialize")
	}
	var params struct {
		ProtocolVersion    int `json:"protocolVersion"`
		ClientCapabilities struct {
			FS struct {
				ReadTextFile  bool `json:"readTextFile"`
				WriteTextFile bool `json:"writeTextFile"`
			} `json:"fs"`
			Terminal bool `json:"terminal"`
		} `json:"clientCapabilities"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("decoding params: %v", err)
	}
	if params.ProtocolVersion != 1 {
		t.Errorf("protocolVersion = %d, want 1", params.ProtocolVersion)
	}
	if params.ClientCapabilities.FS.ReadTextFile || params.ClientCapabilities.FS.WriteTextFile || params.ClientCapabilities.Terminal {
		t.Errorf("expected fs/terminal capabilities to be declined, got %+v", params.ClientCapabilities)
	}

	peer.respond(req.ID, map[string]any{"protocolVersion": 1})

	if err := <-errCh; err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
}

func TestNewSessionReturnsSessionIDAndModes(t *testing.T) {
	c, peer := newTestClient(t, nil)
	ctx := withTimeout(t)

	type result struct {
		sessionID string
		modes     *SessionModeState
		err       error
	}
	resCh := make(chan result, 1)
	go func() {
		sessionID, modes, err := c.NewSession(ctx, "/workspace")
		resCh <- result{sessionID, modes, err}
	}()

	req := peer.next()
	if req.Method != "session/new" {
		t.Fatalf("method = %q, want session/new", req.Method)
	}
	var params struct {
		Cwd        string `json:"cwd"`
		MCPServers []any  `json:"mcpServers"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("decoding params: %v", err)
	}
	if params.Cwd != "/workspace" {
		t.Errorf("cwd = %q, want /workspace", params.Cwd)
	}
	if params.MCPServers == nil || len(params.MCPServers) != 0 {
		t.Errorf("mcpServers = %v, want an empty (but present) array", params.MCPServers)
	}

	peer.respond(req.ID, map[string]any{
		"sessionId": "sess-1",
		"modes": map[string]any{
			"currentModeId":  "default",
			"availableModes": []map[string]any{{"id": "default"}, {"id": "bypassPermissions"}},
		},
	})

	res := <-resCh
	if res.err != nil {
		t.Fatalf("NewSession returned error: %v", res.err)
	}
	if res.sessionID != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1", res.sessionID)
	}
	if !res.modes.Offers("bypassPermissions") {
		t.Errorf("expected modes to offer bypassPermissions, got %+v", res.modes)
	}
	if res.modes.Offers("agent-full-access") {
		t.Errorf("did not expect modes to offer agent-full-access")
	}
}

func TestPromptStreamsUpdatesInOrderThenReturnsStopReason(t *testing.T) {
	var got []Update
	c, peer := newTestClient(t, func(u Update) { got = append(got, u) })
	ctx := withTimeout(t)

	type result struct {
		stopReason string
		usage      *Usage
		err        error
	}
	resCh := make(chan result, 1)
	go func() {
		stopReason, usage, err := c.Prompt(ctx, "sess-1", "hello")
		resCh <- result{stopReason, usage, err}
	}()

	req := peer.next()
	if req.Method != "session/prompt" {
		t.Fatalf("method = %q, want session/prompt", req.Method)
	}
	var params struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("decoding params: %v", err)
	}
	if params.SessionID != "sess-1" || len(params.Prompt) != 1 || params.Prompt[0].Text != "hello" {
		t.Fatalf("unexpected prompt params: %+v", params)
	}

	peer.notify("session/update", map[string]any{
		"sessionId": "sess-1",
		"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "thinking..."}},
	})
	peer.notify("session/update", map[string]any{
		"sessionId": "sess-1",
		"update":    map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tc-1", "title": "Bash: ls"},
	})
	peer.respond(req.ID, map[string]any{
		"stopReason": "end_turn",
		"usage":      map[string]any{"totalTokens": 150, "inputTokens": 100, "outputTokens": 50},
	})

	res := <-resCh
	if res.err != nil {
		t.Fatalf("Prompt returned error: %v", res.err)
	}
	if res.stopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", res.stopReason)
	}
	if res.usage == nil || res.usage.TotalTokens != 150 || res.usage.InputTokens != 100 || res.usage.OutputTokens != 50 {
		t.Errorf("usage = %+v, want {150 100 50}", res.usage)
	}

	// onUpdate must observe both notifications, in order, before Prompt's
	// own response unblocks it — this is what lets the caller build a
	// paragraph-buffered "agent_message_chunk" event before a following
	// tool_call event without a race.
	if len(got) != 2 {
		t.Fatalf("got %d updates, want 2: %+v", len(got), got)
	}
	if got[0].Kind != "agent_message_chunk" || got[1].Kind != "tool_call" {
		t.Errorf("update kinds = [%q, %q], want [agent_message_chunk, tool_call]", got[0].Kind, got[1].Kind)
	}
	if got[0].SessionID != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1", got[0].SessionID)
	}
}

func TestPromptReturnsNilUsageWhenAgentReportsNone(t *testing.T) {
	c, peer := newTestClient(t, nil)
	ctx := withTimeout(t)

	type result struct {
		usage *Usage
		err   error
	}
	resCh := make(chan result, 1)
	go func() {
		_, usage, err := c.Prompt(ctx, "sess-1", "hello")
		resCh <- result{usage, err}
	}()

	req := peer.next()
	peer.respond(req.ID, map[string]any{"stopReason": "end_turn"})

	res := <-resCh
	if res.err != nil {
		t.Fatalf("Prompt returned error: %v", res.err)
	}
	if res.usage != nil {
		t.Errorf("usage = %+v, want nil", res.usage)
	}
}

func TestRequestPermissionRequestIsAutoApproved(t *testing.T) {
	c, peer := newTestClient(t, nil)
	_ = c

	// The agent sends this as a REQUEST (it expects a response), not a
	// notification — id 42 chosen arbitrarily by the "agent" side.
	peer.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      42,
		"method":  "session/request_permission",
		"params": map[string]any{
			"sessionId": "sess-1",
			"toolCall":  map[string]any{"toolCallId": "tc-1"},
			"options": []map[string]any{
				{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
				{"optionId": "reject-once", "name": "Reject once", "kind": "reject_once"},
			},
		},
	})

	raw := peer.rawLine()
	var body struct {
		ID     json.RawMessage `json:"id"`
		Result *struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if string(body.ID) != "42" {
		t.Fatalf("response id = %s, want 42", body.ID)
	}
	if body.Error != nil {
		t.Fatalf("got error response: %+v", body.Error)
	}
	if body.Result == nil || body.Result.Outcome.Outcome != "selected" || body.Result.Outcome.OptionID != "allow-once" {
		t.Fatalf("unexpected result: %+v", body.Result)
	}
}

func TestUnsupportedIncomingMethodGetsMethodNotFound(t *testing.T) {
	c, peer := newTestClient(t, nil)
	_ = c

	peer.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "fs/read_text_file",
		"params":  map[string]any{"path": "/etc/hosts", "sessionId": "sess-1"},
	})

	raw := peer.rawLine()
	var body struct {
		ID    json.RawMessage `json:"id"`
		Error *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Error == nil {
		t.Fatalf("expected an error response for an unsupported method, got none")
	}
	if body.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601 (method not found)", body.Error.Code)
	}
}

func TestCancelIsANotificationAndPromptReturnsOnSubsequentResponse(t *testing.T) {
	c, peer := newTestClient(t, nil)
	ctx := withTimeout(t)

	type result struct {
		stopReason string
		err        error
	}
	resCh := make(chan result, 1)
	go func() {
		stopReason, _, err := c.Prompt(ctx, "sess-1", "do something long-running")
		resCh <- result{stopReason, err}
	}()

	promptReq := peer.next()

	if err := c.Cancel("sess-1"); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}
	cancelMsg := peer.next()
	if cancelMsg.Method != "session/cancel" {
		t.Fatalf("method = %q, want session/cancel", cancelMsg.Method)
	}
	if len(cancelMsg.ID) != 0 {
		t.Errorf("session/cancel must be a notification (no id), got id=%s", cancelMsg.ID)
	}

	// Per the ACP spec, the agent is expected to answer the ORIGINAL
	// session/prompt request with stopReason "cancelled" rather than
	// dropping the connection.
	peer.respond(promptReq.ID, map[string]any{"stopReason": "cancelled"})

	res := <-resCh
	if res.err != nil {
		t.Fatalf("Prompt returned error: %v", res.err)
	}
	if res.stopReason != "cancelled" {
		t.Errorf("stopReason = %q, want cancelled", res.stopReason)
	}
}

func TestCloseUnblocksAPendingCall(t *testing.T) {
	c, _ := newTestClient(t, nil)
	ctx := withTimeout(t)

	errCh := make(chan error, 1)
	go func() { errCh <- c.Initialize(ctx) }()

	// Give the goroutine a moment to actually reach the blocking wait
	// before closing out from under it.
	time.Sleep(20 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("expected Initialize to fail once the connection closed")
		}
	case <-time.After(testTimeout):
		t.Fatal("Initialize did not unblock after Close")
	}
}

func initializeLoadClient(t *testing.T, c *Client, peer *fakePeer, result any) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- c.Initialize(withTimeout(t)) }()
	req := peer.next()
	if req.Method != "initialize" {
		t.Fatalf("method = %q, want initialize", req.Method)
	}
	peer.respond(req.ID, result)
	return <-errCh
}

func assertNoMoreRequests(t *testing.T, c *Client, peer *fakePeer) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for raw := range peer.lines {
		t.Errorf("unexpected request (load must never fall back to session/new): %s", raw)
	}
}

func TestUS011LoadProtocol(t *testing.T) {
	for _, withModes := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty_object", true: "modes"}[withModes], func(t *testing.T) {
			var updates []Update
			c, peer := newTestClient(t, func(u Update) { updates = append(updates, u) })
			if c.SupportsLoadSession() {
				t.Fatal("load supported before initialize")
			}
			if err := initializeLoadClient(t, c, peer, map[string]any{
				"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true},
			}); err != nil {
				t.Fatal(err)
			}
			if !c.SupportsLoadSession() {
				t.Fatal("initialize did not retain loadSession capability")
			}
			type result struct {
				modes *SessionModeState
				err   error
			}
			resCh := make(chan result, 1)
			go func() {
				modes, err := c.LoadSession(withTimeout(t), "sess-A", "/workspace")
				resCh <- result{modes, err}
			}()
			req := peer.next()
			if req.Method != "session/load" {
				t.Fatalf("method = %q, want session/load", req.Method)
			}
			var params struct {
				SessionID  string `json:"sessionId"`
				Cwd        string `json:"cwd"`
				MCPServers []any  `json:"mcpServers"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params.SessionID != "sess-A" || params.Cwd != "/workspace" || params.MCPServers == nil || len(params.MCPServers) != 0 {
				t.Fatalf("unexpected load params: %s", req.Params)
			}
			for _, kind := range []string{"user_message_chunk", "agent_message_chunk"} {
				peer.notify("session/update", map[string]any{
					"sessionId": "sess-A", "update": map[string]any{
						"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": "history"},
					},
				})
			}
			response := map[string]any{}
			if withModes {
				response["modes"] = map[string]any{"currentModeId": "default", "availableModes": []map[string]any{{"id": "default", "name": "Default"}}}
			}
			peer.respond(req.ID, response)
			res := <-resCh
			if res.err != nil {
				t.Fatal(res.err)
			}
			if withModes && (res.modes == nil || res.modes.CurrentModeID != "default" || !res.modes.Offers("default")) {
				t.Fatalf("modes = %+v, want default", res.modes)
			}
			if !withModes && res.modes != nil {
				t.Fatalf("empty load response modes = %+v, want nil", res.modes)
			}
			if len(updates) != 2 || updates[0].SessionID != "sess-A" || updates[1].SessionID != "sess-A" || updates[0].Kind != "user_message_chunk" || updates[1].Kind != "agent_message_chunk" {
				t.Fatalf("updates before load returned = %+v, want ordered history", updates)
			}
			assertNoMoreRequests(t, c, peer)
		})
	}
}

func TestUS011LoadFailures(t *testing.T) {
	for _, tc := range []struct {
		name            string
		result          any
		initializeError bool
	}{
		{"uninitialized", nil, false},
		{"capability_missing", map[string]any{"protocolVersion": 1}, false},
		{"capability_false", map[string]any{"agentCapabilities": map[string]any{"loadSession": false}}, false},
		{"capability_invalid", map[string]any{"agentCapabilities": map[string]any{"loadSession": "true"}}, true},
		{"initialize_scalar", true, true},
		{"initialize_null", json.RawMessage("null"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := newTestClient(t, nil)
			if tc.result != nil {
				err := initializeLoadClient(t, c, peer, tc.result)
				if (err != nil) != tc.initializeError {
					t.Fatalf("Initialize error = %v, want error %v", err, tc.initializeError)
				}
			}
			if c.SupportsLoadSession() {
				t.Fatal("invalid/missing capability must not permit load")
			}
			if _, err := c.LoadSession(withTimeout(t), "sess-A", "/workspace"); err == nil {
				t.Fatal("unsupported load succeeded")
			}
			assertNoMoreRequests(t, c, peer)
		})
	}
	for _, tc := range []struct{ name, sessionID, cwd string }{
		{"empty_session", "", "/workspace"}, {"blank_session", " \t", "/workspace"},
		{"empty_cwd", "sess-A", ""}, {"relative_cwd", "sess-A", "workspace"},
		{"nul_cwd", "sess-A", "/workspace\x00other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := newTestClient(t, nil)
			if err := initializeLoadClient(t, c, peer, map[string]any{"agentCapabilities": map[string]any{"loadSession": true}}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.LoadSession(withTimeout(t), tc.sessionID, tc.cwd); err == nil {
				t.Fatal("invalid load input succeeded")
			}
			assertNoMoreRequests(t, c, peer)
		})
	}
	for _, tc := range []struct {
		name   string
		result any
	}{
		{"null", nil}, {"scalar", "bad"}, {"array", []any{}},
		{"malformed_modes", map[string]any{"modes": true}},
		{"malformed_mode_id", map[string]any{"modes": map[string]any{"currentModeId": 42}}},
		{"rpc_error", nil}, {"missing_result", nil}, {"malformed_json", nil}, {"timeout", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := newTestClient(t, nil)
			if err := initializeLoadClient(t, c, peer, map[string]any{"agentCapabilities": map[string]any{"loadSession": true}}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			errCh := make(chan error, 1)
			go func() { _, err := c.LoadSession(ctx, "sess-A", "/workspace"); errCh <- err }()
			req := peer.next()
			if req.Method != "session/load" {
				t.Fatalf("method = %q, want session/load", req.Method)
			}
			switch tc.name {
			case "rpc_error":
				peer.write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": &RPCError{Code: -32001, Message: "session missing"}})
			case "missing_result":
				peer.write(map[string]any{"jsonrpc": "2.0", "id": req.ID})
			case "malformed_json":
				if _, err := peer.w.Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":" + string(req.ID) + ",\"result\":{\n")); err != nil {
					t.Fatal(err)
				}
			case "timeout":
			default:
				peer.respond(req.ID, tc.result)
			}
			err := <-errCh
			if err == nil {
				t.Fatal("invalid load response succeeded")
			}
			if (tc.name == "timeout" || tc.name == "malformed_json") && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want deadline exceeded", err)
			}
			if tc.name == "rpc_error" {
				var rpcErr *RPCError
				if !errors.As(err, &rpcErr) || rpcErr.Code != -32001 {
					t.Fatalf("error = %v, want original RPC error", err)
				}
			}
			assertNoMoreRequests(t, c, peer)
		})
	}
}

func TestUS011AliveTracksPeerExit(t *testing.T) {
	c, peer := newTestClient(t, nil)
	if !c.Alive() {
		t.Fatal("new connection is not alive")
	}
	if err := peer.w.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.closed:
	case <-time.After(testTimeout):
		t.Fatal("connection did not observe peer exit")
	}
	if c.Alive() {
		t.Fatal("connection is alive after peer exit")
	}
}
