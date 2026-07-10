package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// agentEnvBlocklist names parent-process env vars that must never leak into a
// spawned agent subprocess. CLAUDECODE makes Claude Code refuse to launch
// ("cannot be launched inside another Claude Code session"), which breaks the
// daemon whenever it is itself started from within a Claude Code session.
var agentEnvBlocklist = map[string]bool{
	"CLAUDECODE": true,
}

// buildAgentEnv returns the parent environment minus any blocklisted vars, with
// the caller's overrides appended. cmd.Env is always set from this so agents get
// a sanitized environment even when no overrides are provided.
func buildAgentEnv(extra []string) []string {
	parent := os.Environ()
	out := make([]string, 0, len(parent)+len(extra))
	for _, kv := range parent {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if agentEnvBlocklist[key] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// AgentBridge manages a CLI agent subprocess communicating via stdio JSON-RPC.
// Designed to be reusable for Codex, Claude, Cursor, Copilot, etc.
type AgentBridge struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	processDone chan struct{}
	generation  uint64
	stdin       *json.Encoder
	requestID   int
	pending     map[agentRequestKey]chan agentResult
	initialized bool

	// writeMu serializes writes to the agent's stdin pipe. It is deliberately
	// separate from mu: a write can block indefinitely when the child stops
	// draining stdin (full pipe buffer), and holding mu across that write would
	// deadlock the whole bridge — the stdout reader (handleMessage), Respond,
	// Stop and the cmd.Wait cleanup goroutine all need mu, so responses could
	// never be delivered and the wedged child could never be killed.
	writeMu sync.Mutex

	// Callbacks set by the server
	OnNotification func(method string, params interface{})
	OnRequest      func(id interface{}, method string, params interface{})
}

type agentResult struct {
	Result interface{}
	Error  *RpcError
}

type agentRequestKey struct {
	Generation uint64
	ID         interface{}
}

type agentMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  interface{}      `json:"params,omitempty"`
	Result  interface{}      `json:"result,omitempty"`
	Error   *RpcError        `json:"error,omitempty"`
}

func NewAgentBridge() *AgentBridge {
	return &AgentBridge{
		pending: make(map[agentRequestKey]chan agentResult),
	}
}

func (b *AgentBridge) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cmd != nil && b.cmd.Process != nil && b.cmd.ProcessState == nil
}

// StartProcess spawns the agent subprocess and sets up stdio pipes without
// performing any protocol handshake. Use this when the caller needs custom init.
func (b *AgentBridge) StartProcess(command string, args []string, cwd string, env []string) error {
	b.mu.Lock()
	if b.cmd != nil && b.cmd.Process != nil && b.cmd.ProcessState == nil {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	cmd := exec.Command(command, args...)
	cmd.Dir = cwd
	cmd.Env = buildAgentEnv(env)
	// Own process group so Stop can kill the agent's whole tree, not just the
	// direct child (see agentSysProcAttr).
	cmd.SysProcAttr = agentSysProcAttr()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", command, err)
	}

	log.Printf("[agent] spawned: %s %v (pid=%d)", command, args, cmd.Process.Pid)

	b.mu.Lock()
	b.generation++
	generation := b.generation
	b.cmd = cmd
	b.processDone = make(chan struct{})
	b.stdin = json.NewEncoder(stdinPipe)
	b.pending = make(map[agentRequestKey]chan agentResult)
	b.initialized = false
	done := b.processDone
	b.mu.Unlock()

	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		for scanner.Scan() {
			log.Printf("[agent:stderr] %s", scanner.Text())
		}
	}()

	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var msg agentMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				log.Printf("[agent:stdout] %s", line)
				continue
			}
			b.handleMessage(generation, msg)
		}
		log.Printf("[agent] stdout reader ended")
	}()

	go func() {
		err := cmd.Wait()
		log.Printf("[agent] process exited: %v", err)
		b.mu.Lock()
		if b.generation == generation {
			for _, ch := range b.pending {
				ch <- agentResult{Error: &RpcError{Code: -1, Message: "agent process terminated"}}
			}
			b.pending = make(map[agentRequestKey]chan agentResult)
			b.cmd = nil
			b.stdin = nil
			b.initialized = false
		}
		b.mu.Unlock()
		close(done)
	}()

	return nil
}

// Start spawns the agent and performs the Codex-style initialize handshake.
// If the agent is already running and initialized, this is a no-op.
func (b *AgentBridge) Start(command string, args []string, cwd string) error {
	b.mu.Lock()
	alreadyInit := b.initialized && b.cmd != nil && b.cmd.Process != nil && b.cmd.ProcessState == nil
	b.mu.Unlock()
	if alreadyInit {
		return nil // already running and initialized
	}

	if err := b.StartProcess(command, args, cwd, nil); err != nil {
		return err
	}

	_, err := b.Send("initialize", map[string]interface{}{
		"clientInfo":   map[string]string{"name": "AnyCode", "version": Version},
		"capabilities": map[string]interface{}{},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	b.mu.Lock()
	b.initialized = true
	stdin := b.stdin
	b.mu.Unlock()

	if stdin != nil {
		b.writeMu.Lock()
		_ = stdin.Encode(map[string]interface{}{"jsonrpc": "2.0", "method": "initialized"})
		b.writeMu.Unlock()
	}

	return nil
}

func (b *AgentBridge) Stop() {
	b.mu.Lock()
	cmd := b.cmd
	done := b.processDone
	generation := b.generation
	b.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	_ = killAgentProcess(cmd.Process)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}

	b.mu.Lock()
	if b.generation == generation {
		for _, ch := range b.pending {
			ch <- agentResult{Error: &RpcError{Code: -1, Message: "agent process stopped"}}
		}
		b.pending = make(map[agentRequestKey]chan agentResult)
		b.cmd = nil
		b.stdin = nil
		b.initialized = false
		b.processDone = nil
	}
	b.mu.Unlock()
}

func (b *AgentBridge) Send(method string, params interface{}) (interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return b.SendContext(ctx, method, params)
}

func (b *AgentBridge) SendContext(ctx context.Context, method string, params interface{}) (interface{}, error) {
	log.Printf("[agent] SendContext method=%s", method)
	if !b.IsRunning() {
		return nil, fmt.Errorf("agent is not running")
	}

	b.mu.Lock()
	b.requestID++
	id := b.requestID
	generation := b.generation
	key := agentRequestKey{Generation: generation, ID: float64(id)}
	ch := make(chan agentResult, 1)
	b.pending[key] = ch
	stdin := b.stdin
	b.mu.Unlock()

	if stdin == nil {
		b.mu.Lock()
		delete(b.pending, key)
		b.mu.Unlock()
		return nil, fmt.Errorf("agent is not running")
	}

	msg := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	// Write outside mu (see writeMu doc). A wedged pipe blocks only this
	// request; Stop can still take mu and kill the child, which unblocks the
	// write with a pipe error.
	b.writeMu.Lock()
	err := stdin.Encode(msg)
	b.writeMu.Unlock()

	if err != nil {
		b.mu.Lock()
		delete(b.pending, key)
		b.mu.Unlock()
		return nil, fmt.Errorf("write to agent: %w", err)
	}

	select {
	case res := <-ch:
		log.Printf("[agent] SendContext method=%s returned", method)
		if res.Error != nil {
			return nil, formatAgentRPCError(res.Error)
		}
		return res.Result, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, key)
		b.mu.Unlock()
		return nil, fmt.Errorf("agent request failed: %s: %w", method, ctx.Err())
	}
}

func formatAgentRPCError(rpcErr *RpcError) error {
	if rpcErr == nil {
		return nil
	}
	message := rpcErr.Message
	if details := rpcErrorDetails(rpcErr.Data); details != "" {
		message = fmt.Sprintf("%s: %s", message, details)
	}
	// This bridge is shared by every stdio agent (codex/claude/cursor/trae),
	// so the error label must stay agent-neutral.
	return fmt.Errorf("agent error %d: %s", rpcErr.Code, message)
}

func rpcErrorDetails(data interface{}) string {
	payload, ok := data.(map[string]interface{})
	if !ok {
		return ""
	}
	if details, ok := payload["details"].(string); ok && details != "" {
		return details
	}
	if method, ok := payload["method"].(string); ok && method != "" {
		return method
	}
	return ""
}

func (b *AgentBridge) Respond(id interface{}, result interface{}) error {
	b.mu.Lock()
	stdin := b.stdin
	b.mu.Unlock()
	if stdin == nil {
		return nil
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return stdin.Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (b *AgentBridge) handleMessage(generation uint64, msg agentMessage) {
	b.mu.Lock()
	currentGeneration := b.generation
	b.mu.Unlock()
	if generation != currentGeneration {
		return
	}

	// Response to our request
	if msg.ID != nil && (msg.Result != nil || msg.Error != nil) {
		id := parseID(*msg.ID)
		key := agentRequestKey{Generation: generation, ID: id}
		b.mu.Lock()
		ch, ok := b.pending[key]
		if ok {
			delete(b.pending, key)
		}
		b.mu.Unlock()
		if ok {
			ch <- agentResult{Result: msg.Result, Error: msg.Error}
		}
		return
	}

	// Request from agent (needs response from client)
	if msg.Method != "" && msg.ID != nil {
		if b.OnRequest != nil {
			id := parseID(*msg.ID)
			b.OnRequest(id, msg.Method, msg.Params)
		}
		return
	}

	// Notification
	if msg.Method != "" {
		if b.OnNotification != nil {
			b.OnNotification(msg.Method, msg.Params)
		}
		return
	}
}
