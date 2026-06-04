package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type protocolProjectSample struct {
	Root       string `json:"root"`
	ProjectID  string `json:"projectId"`
	Generation uint64 `json:"generation"`
	Name       string `json:"name"`
	IsGit      bool   `json:"isGit"`
	FileCount  int    `json:"fileCount"`
}

type protocolResumeSnapshotSample struct {
	LatestSeq uint64                            `json:"latestSeq"`
	Project   protocolProjectSample             `json:"project"`
	Agents    map[string]map[string]interface{} `json:"agents"`
}

type protocolResumeResponseSample struct {
	OK            bool                          `json:"ok"`
	Events        []eventEnvelope               `json:"events"`
	LatestSeq     uint64                        `json:"latestSeq"`
	Project       protocolProjectSample         `json:"project"`
	CursorExpired bool                          `json:"cursorExpired"`
	Snapshot      *protocolResumeSnapshotSample `json:"snapshot,omitempty"`
}

type protocolHelloSample struct {
	ProtocolVersion int                               `json:"protocolVersion"`
	DaemonVersion   string                            `json:"daemonVersion"`
	Role            string                            `json:"role"`
	Capabilities    []string                          `json:"capabilities"`
	Project         protocolHelloProjectSample        `json:"project"`
	Agents          map[string]map[string]interface{} `json:"agents"`
	LatestSeq       uint64                            `json:"latestSeq"`
	Resume          protocolHelloResumeResponseSample `json:"resume"`
}

type protocolHelloProjectSample struct {
	Root       string  `json:"root"`
	ProjectID  string  `json:"projectId"`
	Generation *uint64 `json:"generation,omitempty"`
	Name       string  `json:"name"`
	IsGit      bool    `json:"isGit"`
	FileCount  int     `json:"fileCount"`
}

type protocolHelloResumeSnapshotSample struct {
	LatestSeq uint64                            `json:"latestSeq"`
	Project   protocolHelloProjectSample        `json:"project"`
	Agents    map[string]map[string]interface{} `json:"agents"`
}

type protocolHelloEventSample struct {
	Seq               uint64                 `json:"seq"`
	Agent             string                 `json:"agent"`
	Method            string                 `json:"method"`
	Params            map[string]interface{} `json:"params,omitempty"`
	ProjectID         string                 `json:"projectId"`
	ProjectGeneration *uint64                `json:"projectGeneration,omitempty"`
	OperationID       string                 `json:"operationId,omitempty"`
	Timestamp         int64                  `json:"ts"`
}

type protocolHelloResumeResponseSample struct {
	OK            bool                               `json:"ok"`
	Events        []protocolHelloEventSample         `json:"events"`
	LatestSeq     uint64                             `json:"latestSeq"`
	Project       protocolHelloProjectSample         `json:"project"`
	CursorExpired bool                               `json:"cursorExpired"`
	Snapshot      *protocolHelloResumeSnapshotSample `json:"snapshot,omitempty"`
}

func loadProtocolSample(t *testing.T, name string) protocolResumeResponseSample {
	t.Helper()
	path := filepath.Join("..", "protocol", "samples", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sample %s: %v", name, err)
	}
	var sample protocolResumeResponseSample
	if err := json.Unmarshal(data, &sample); err != nil {
		t.Fatalf("decode sample %s: %v", name, err)
	}
	return sample
}

func loadHelloSample(t *testing.T, name string) protocolHelloSample {
	t.Helper()
	path := filepath.Join("..", "protocol", "samples", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sample %s: %v", name, err)
	}
	var sample protocolHelloSample
	if err := json.Unmarshal(data, &sample); err != nil {
		t.Fatalf("decode sample %s: %v", name, err)
	}
	return sample
}

func assertProjectSample(t *testing.T, project protocolProjectSample) {
	t.Helper()
	if project.Root == "" || project.ProjectID == "" || project.Name == "" {
		t.Fatalf("expected populated project sample, got %#v", project)
	}
	if project.Generation == 0 {
		t.Fatalf("expected project generation > 0, got %#v", project)
	}
}

func assertHelloProjectSample(t *testing.T, project protocolHelloProjectSample, expectGeneration bool) {
	t.Helper()
	if project.Root == "" || project.ProjectID == "" || project.Name == "" {
		t.Fatalf("expected populated hello project sample, got %#v", project)
	}
	if expectGeneration {
		if project.Generation == nil || *project.Generation == 0 {
			t.Fatalf("expected hello project generation > 0, got %#v", project)
		}
		return
	}
	if project.Generation != nil {
		t.Fatalf("expected hello project generation to be omitted, got %#v", project)
	}
}

func assertHelloSample(t *testing.T, sample protocolHelloSample, expectGeneration bool) {
	t.Helper()
	if sample.ProtocolVersion != 1 {
		t.Fatalf("expected protocolVersion=1, got %#v", sample.ProtocolVersion)
	}
	if sample.DaemonVersion == "" {
		t.Fatalf("expected daemonVersion, got %#v", sample)
	}
	if sample.Role != "client" {
		t.Fatalf("expected role=client, got %#v", sample.Role)
	}
	if len(sample.Capabilities) == 0 {
		t.Fatalf("expected hello capabilities, got %#v", sample.Capabilities)
	}
	if sample.Capabilities[0] != "client.hello" {
		t.Fatalf("expected client.hello capability, got %#v", sample.Capabilities)
	}
	if expectGeneration {
		if len(sample.Capabilities) != 2 || sample.Capabilities[1] != "project.generation" {
			t.Fatalf("expected project.generation capability, got %#v", sample.Capabilities)
		}
	} else if len(sample.Capabilities) != 1 {
		t.Fatalf("expected only client.hello capability, got %#v", sample.Capabilities)
	}
	assertHelloProjectSample(t, sample.Project, expectGeneration)
	assertHelloProjectSample(t, sample.Resume.Project, expectGeneration)
	if sample.LatestSeq != sample.Resume.LatestSeq {
		t.Fatalf("expected hello latestSeq to match nested resume latestSeq, got hello=%d resume=%d", sample.LatestSeq, sample.Resume.LatestSeq)
	}
	for _, agent := range []string{"codex", "claude", "gemini"} {
		if _, ok := sample.Agents[agent]; !ok {
			t.Fatalf("expected %s hello agent payload, got %#v", agent, sample.Agents)
		}
	}
	for _, event := range sample.Resume.Events {
		if event.Seq == 0 || event.Agent == "" || event.Method == "" || event.ProjectID == "" || event.Timestamp == 0 {
			t.Fatalf("expected full hello event metadata, got %#v", event)
		}
		if expectGeneration {
			if event.ProjectGeneration == nil || *event.ProjectGeneration == 0 {
				t.Fatalf("expected hello event generation metadata, got %#v", event)
			}
		} else if event.ProjectGeneration != nil {
			t.Fatalf("expected hello event generation metadata to be omitted, got %#v", event)
		}
	}
	if sample.Resume.Snapshot != nil {
		assertHelloProjectSample(t, sample.Resume.Snapshot.Project, expectGeneration)
	}
}

func TestProtocolSampleReplayResponseShape(t *testing.T) {
	sample := loadProtocolSample(t, "events.resume.replay.v1.json")
	if !sample.OK {
		t.Fatalf("expected ok=true, got %#v", sample)
	}
	if sample.CursorExpired {
		t.Fatalf("expected replay sample to have cursorExpired=false, got %#v", sample)
	}
	if sample.Snapshot != nil {
		t.Fatalf("expected replay sample to omit snapshot, got %#v", sample.Snapshot)
	}
	if len(sample.Events) == 0 {
		t.Fatalf("expected replay sample to include events, got %#v", sample)
	}
	assertProjectSample(t, sample.Project)
	for _, event := range sample.Events {
		if event.Seq == 0 || event.Agent == "" || event.Method == "" || event.ProjectID == "" || event.ProjectGeneration == 0 || event.Timestamp == 0 {
			t.Fatalf("expected full event envelope metadata, got %#v", event)
		}
	}
}

func TestProtocolSampleExpiredResponseShape(t *testing.T) {
	sample := loadProtocolSample(t, "events.resume.cursor-expired.v1.json")
	if !sample.OK {
		t.Fatalf("expected ok=true, got %#v", sample)
	}
	if !sample.CursorExpired {
		t.Fatalf("expected expired sample to have cursorExpired=true, got %#v", sample)
	}
	if len(sample.Events) != 0 {
		t.Fatalf("expected expired sample to have no partial events, got %#v", sample.Events)
	}
	if sample.Snapshot == nil {
		t.Fatalf("expected expired sample to include snapshot, got %#v", sample)
	}
	assertProjectSample(t, sample.Project)
	assertProjectSample(t, sample.Snapshot.Project)
	if sample.Snapshot.LatestSeq != sample.LatestSeq {
		t.Fatalf("expected snapshot latestSeq to match top-level latestSeq, got snapshot=%d latest=%d", sample.Snapshot.LatestSeq, sample.LatestSeq)
	}
	for _, agent := range []string{"codex", "claude", "gemini"} {
		payload, ok := sample.Snapshot.Agents[agent]
		if !ok {
			t.Fatalf("expected %s snapshot payload, got %#v", agent, sample.Snapshot.Agents)
		}
		if latest, ok := payload["latestSeq"].(float64); !ok || uint64(latest) != sample.LatestSeq {
			t.Fatalf("expected %s latestSeq=%d, got %#v", agent, sample.LatestSeq, payload["latestSeq"])
		}
	}
}

func TestProtocolSampleServerHelloReplayShape(t *testing.T) {
	sample := loadHelloSample(t, "server.hello.replay.v1.json")
	assertHelloSample(t, sample, true)
	if sample.Resume.CursorExpired {
		t.Fatalf("expected replay hello sample to avoid snapshot fallback, got %#v", sample.Resume)
	}
	if len(sample.Resume.Events) == 0 {
		t.Fatalf("expected replay hello sample to include replayed events, got %#v", sample.Resume)
	}
}

func TestProtocolSampleServerHelloNoCursorShape(t *testing.T) {
	sample := loadHelloSample(t, "server.hello.no-cursor.v1.json")
	assertHelloSample(t, sample, true)
	if sample.Resume.CursorExpired {
		t.Fatalf("expected no-cursor hello sample to avoid snapshot fallback, got %#v", sample.Resume)
	}
	if len(sample.Resume.Events) != 0 {
		t.Fatalf("expected no-cursor hello sample to skip replay, got %#v", sample.Resume.Events)
	}
}

func TestProtocolSampleServerHelloCursorExpiredShape(t *testing.T) {
	sample := loadHelloSample(t, "server.hello.cursor-expired.v1.json")
	assertHelloSample(t, sample, true)
	if !sample.Resume.CursorExpired {
		t.Fatalf("expected cursor-expired hello sample to trigger snapshot fallback, got %#v", sample.Resume)
	}
	if len(sample.Resume.Events) != 0 {
		t.Fatalf("expected cursor-expired hello sample to omit partial replay, got %#v", sample.Resume.Events)
	}
	if sample.Resume.Snapshot == nil {
		t.Fatalf("expected cursor-expired hello sample to include snapshot, got %#v", sample.Resume)
	}
	if sample.LatestSeq != sample.Resume.LatestSeq || sample.LatestSeq != sample.Resume.Snapshot.LatestSeq {
		t.Fatalf("expected hello/sample latestSeq values to align, got hello=%d resume=%d snapshot=%d", sample.LatestSeq, sample.Resume.LatestSeq, sample.Resume.Snapshot.LatestSeq)
	}
	assertHelloProjectSample(t, sample.Resume.Snapshot.Project, true)
}

func TestProtocolSampleServerHelloWithoutProjectGenerationShape(t *testing.T) {
	sample := loadHelloSample(t, "server.hello.no-project-generation.v1.json")
	assertHelloSample(t, sample, false)
	if sample.Resume.CursorExpired {
		t.Fatalf("expected no-project-generation hello sample to avoid snapshot fallback, got %#v", sample.Resume)
	}
	if len(sample.Resume.Events) != 0 {
		t.Fatalf("expected no-project-generation hello sample to skip replay, got %#v", sample.Resume.Events)
	}
}
