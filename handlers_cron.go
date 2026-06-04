package main

import "fmt"

func (s *Server) handleCronList(req RpcRequest, client *wsClient) (interface{}, error) {
	return map[string]interface{}{"ok": true, "crons": s.cron.ListJobs()}, nil

}

func (s *Server) handleCronCreate(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	name := getParamString(params, "name")
	agent := getParamString(params, "agent")
	sessionId := getParamString(params, "sessionId")
	prompt := getParamString(params, "prompt")
	expression := getParamString(params, "expression")
	enabled := getParamBool(params, "enabled")

	if name == "" || agent == "" || prompt == "" || expression == "" {
		return nil, fmt.Errorf("name, agent, prompt and expression are required")
	}

	job, err := s.cron.CreateJob(name, agent, sessionId, prompt, expression, enabled)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true, "cron": job}, nil

}

func (s *Server) handleCronUpdate(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	id := getParamString(params, "id")
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	name := getParamString(params, "name")
	agent := getParamString(params, "agent")
	sessionId := getParamString(params, "sessionId")
	prompt := getParamString(params, "prompt")
	expression := getParamString(params, "expression")
	enabled := getParamBool(params, "enabled")

	job, err := s.cron.UpdateJob(id, name, agent, sessionId, prompt, expression, enabled)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true, "cron": job}, nil

}

func (s *Server) handleCronDelete(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	id := getParamString(params, "id")
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	if err := s.cron.DeleteJob(id); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil

}
