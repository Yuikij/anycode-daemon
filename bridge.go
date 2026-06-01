package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// AgentBridge manages a CLI agent subprocess communicating via stdio JSON-RPC.
// Designed to be reusable for Codex, Gemini CLI, Copilot, etc.
type AgentBridge struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	stdin       *json.Encoder
	requestID   int
	pending     map[interface{}]chan agentResult
	initialized bool

	// Callbacks set by the server
	OnNotification func(method string, params interface{})
	OnRequest      func(id interface{}, method string, params interface{})
}

type agentResult struct {
	Result interface{}
	Error  *RpcError
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
		pending: make(map[interface{}]chan agentResult),
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
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

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
	b.cmd = cmd
	b.stdin = json.NewEncoder(stdinPipe)
	b.pending = make(map[interface{}]chan agentResult)
	b.initialized = false
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
			b.handleMessage(msg)
		}
		log.Printf("[agent] stdout reader ended")
	}()

	go func() {
		err := cmd.Wait()
		log.Printf("[agent] process exited: %v", err)
		b.mu.Lock()
		for _, ch := range b.pending {
			ch <- agentResult{Error: &RpcError{Code: -1, Message: "agent process terminated"}}
		}
		b.pending = make(map[interface{}]chan agentResult)
		b.mu.Unlock()
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
		"clientInfo": map[string]string{"name": "AnyCode", "version": Version},
		"capabilities": map[string]interface{}{
			"experimentalApi": true,
		},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	b.initialized = true

	b.mu.Lock()
	_ = b.stdin.Encode(map[string]interface{}{"jsonrpc": "2.0", "method": "initialized"})
	b.mu.Unlock()

	return nil
}

func (b *AgentBridge) Stop() {
	b.mu.Lock()
	cmd := b.cmd
	b.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	_ = cmd.Process.Kill()
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}

	b.mu.Lock()
	b.cmd = nil
	b.stdin = nil
	b.initialized = false
	b.mu.Unlock()
}

func (b *AgentBridge) Send(method string, params interface{}) (interface{}, error) {
	if !b.IsRunning() {
		return nil, fmt.Errorf("agent is not running")
	}

	b.mu.Lock()
	b.requestID++
	id := b.requestID
	ch := make(chan agentResult, 1)
	b.pending[float64(id)] = ch
	msg := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	err := b.stdin.Encode(msg)
	b.mu.Unlock()

	if err != nil {
		b.mu.Lock()
		delete(b.pending, float64(id))
		b.mu.Unlock()
		return nil, fmt.Errorf("write to agent: %w", err)
	}

	select {
	case res := <-ch:
		if res.Error != nil {
			return nil, fmt.Errorf("Codex error %d: %s", res.Error.Code, res.Error.Message)
		}
		return res.Result, nil
	case <-time.After(10 * time.Minute):
		b.mu.Lock()
		delete(b.pending, float64(id))
		b.mu.Unlock()
		return nil, fmt.Errorf("agent request timed out: %s", method)
	}
}

func (b *AgentBridge) Respond(id interface{}, result interface{}) error {
	if !b.IsRunning() {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stdin.Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (b *AgentBridge) handleMessage(msg agentMessage) {
	// Response to our request
	if msg.ID != nil && (msg.Result != nil || msg.Error != nil) {
		id := parseID(*msg.ID)
		b.mu.Lock()
		ch, ok := b.pending[id]
		if ok {
			delete(b.pending, id)
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
