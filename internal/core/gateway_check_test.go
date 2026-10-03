package core

import (
	"context"
	"net"
	"strings"
	"testing"
)

func startTestGateway(t *testing.T, s *Service, connectionID string, readOnly bool) (Gateway, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	g, password, err := s.AddGateway(Gateway{Name: "self-check", ConnectionID: connectionID, Prefix: "Documents", Username: "check-user", Port: port, ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartGateway(g.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.StopGateway(g.ID) })
	return g, password
}

func assertNoGatewayProbeObjects(t *testing.T, s *Service, connectionID string) {
	t.Helper()
	store, err := s.store(connectionID)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(context.Background(), "Documents")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Path, ".tamiops-check-") {
			t.Errorf("gateway self-check left probe object %q", entry.Path)
		}
	}
}

func TestGatewayCheckExercisesAndCleansWriteEntry(t *testing.T) {
	s, connectionID := testService(t)
	g, _ := startTestGateway(t, s, connectionID, false)
	result, err := s.CheckGateway(context.Background(), g.ID)
	if err != nil || !result.Passed || result.ReadOnly {
		t.Fatalf("check result = %#v, err = %v", result, err)
	}
	for _, name := range []string{"拒绝未认证请求", "认证后入口", "创建", "读取", "覆盖", "移动", "移动后读取", "删除", "核对删除", "清理"} {
		if !gatewayCheckHasStep(result.Steps, name) {
			t.Errorf("missing check step %q: %#v", name, result.Steps)
		}
	}
	if len(result.Residual) != 0 {
		t.Fatalf("unexpected residual probes: %#v", result.Residual)
	}
	assertNoGatewayProbeObjects(t, s, connectionID)
}

func TestGatewayCheckReadOnlyRefusesWritesWithoutResidual(t *testing.T) {
	s, connectionID := testService(t)
	g, _ := startTestGateway(t, s, connectionID, true)
	result, err := s.CheckGateway(context.Background(), g.ID)
	if err != nil || !result.Passed || !result.ReadOnly {
		t.Fatalf("check result = %#v, err = %v", result, err)
	}
	for _, name := range []string{"拒绝未认证请求", "认证后入口", "只读访问", "拒绝写入", "核对只读探针", "清理"} {
		if !gatewayCheckHasStep(result.Steps, name) {
			t.Errorf("missing check step %q: %#v", name, result.Steps)
		}
	}
	if len(result.Residual) != 0 {
		t.Fatalf("unexpected residual probes: %#v", result.Residual)
	}
	assertNoGatewayProbeObjects(t, s, connectionID)
}
