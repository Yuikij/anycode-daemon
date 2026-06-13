package main

import "fmt"

func (s *Server) handleCronList(req RpcRequest, client *wsClient) (interface{}, error) {
	return map[string]interface{}{"ok": true, "crons": s.cron.ListJobs()}, nil

}

func (s *Server) handleCronCreate(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[cronCreateParams](req)
	if err != nil {
		return nil, err
	}
	if p.Name == "" || p.Agent == "" || p.Prompt == "" || p.Expression == "" {
		return nil, fmt.Errorf("name, agent, prompt and expression are required")
	}

	job, err := s.cron.CreateJob(p.Name, p.Agent, p.SessionID, p.Prompt, p.Expression, p.Enabled)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true, "cron": job}, nil

}

func (s *Server) handleCronUpdate(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[cronUpdateParams](req)
	if err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, fmt.Errorf("id is required")
	}

	job, err := s.cron.UpdateJob(p.ID, p.Name, p.Agent, p.SessionID, p.Prompt, p.Expression, p.Enabled)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"ok": true, "cron": job}, nil

}

func (s *Server) handleCronDelete(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[cronDeleteParams](req)
	if err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	if err := s.cron.DeleteJob(p.ID); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil

}
