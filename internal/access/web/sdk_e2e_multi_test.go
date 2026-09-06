package web_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/sdkgen"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/campus"
)

// assertJourneysResult 解析 SDK 返回的行程结果并断言顺序（多语言共用）。
func assertJourneysResult(t *testing.T, output []byte) {
	t.Helper()
	var result struct {
		Journeys []struct {
			TripID string `json:"trip_id"`
		} `json:"journeys"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("解析 SDK 返回失败: %v\n%s", err, output)
	}
	if len(result.Journeys) != 2 || result.Journeys[0].TripID != "trip-early" || result.Journeys[1].TripID != "trip-late" {
		t.Fatalf("journeys = %#v, want trip-early then trip-late", result.Journeys)
	}
}

// TestGeneratedPythonSDKInvokesRealCapability 端到端：生成的 Python SDK 经真实
// HTTP 端点调用真实 hosted campus capability（与 Go 版本同装配，验证运行时行为）。
func TestGeneratedPythonSDKInvokesRealCapability(t *testing.T) {
	testServer, capabilitiesJSON := newCampusE2E(t)
	defer testServer.Close()

	files, err := sdkgen.Generate(capabilitiesJSON, sdkgen.Options{Language: sdkgen.LanguagePython, PackageID: campus.PackageID})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	dir := t.TempDir()
	for _, f := range files {
		path := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, f.Code, 0644); err != nil {
			t.Fatal(err)
		}
	}
	main := `import json
import sys
from datetime import datetime, timedelta

sys.path.insert(0, sys.argv[1])
from campus import Client, BusJourneysSearchInput, bus_journeys_search

client = Client(sys.argv[2], headers={"Authorization": "Bearer sdk-test"})
result = bus_journeys_search(client, BusJourneysSearchInput(
    origin_stop_id="stop-a",
    destination_stop_id="stop-b",
    depart_after=datetime.now() - timedelta(days=400),
))
print(json.dumps(result))
`
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(main), 0644); err != nil {
		t.Fatal(err)
	}
	// 外部解释器可能挂住：绑定超时上下文，超时后子进程被杀而不是拖死整个测试。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "python", "main.py", dir, testServer.URL)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("python 运行生成的 SDK 失败: %v\n%s", err, output)
	}
	assertJourneysResult(t, output)
}

// TestGeneratedTypeScriptSDKInvokesRealCapability 端到端：生成的 TypeScript SDK
// 经 tsx 运行调真实端点（仅 npx 不可用时跳过）。
func TestGeneratedTypeScriptSDKInvokesRealCapability(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx 不可用，跳过 TS 端到端")
	}
	testServer, capabilitiesJSON := newCampusE2E(t)
	defer testServer.Close()

	files, err := sdkgen.Generate(capabilitiesJSON, sdkgen.Options{Language: sdkgen.LanguageTypeScript, PackageID: campus.PackageID})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "campus"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "campus", "client.ts"), files[0].Code, 0644); err != nil {
		t.Fatal(err)
	}
	main := `import { CampusClient } from "./campus/client";

async function main(): Promise<void> {
  const client = new CampusClient(process.argv[2], { headers: { "Authorization": "Bearer sdk-test" } });
  const result = await client.busJourneysSearch({
    origin_stop_id: "stop-a",
    destination_stop_id: "stop-b",
    depart_after: new Date(Date.now() - 400 * 24 * 3600 * 1000),
  });
  console.log(JSON.stringify(result));
}
void main();
`
	if err := os.WriteFile(filepath.Join(dir, "main.ts"), []byte(main), 0644); err != nil {
		t.Fatal(err)
	}
	// npx 首次运行需下载 tsx，超时给足；工具链存在但运行失败必须让测试失败。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "npx", "--yes", "tsx@4.20.5", "main.ts", testServer.URL)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("tsx 运行生成的 SDK 失败: %v\n%s", err, output)
	}
	assertJourneysResult(t, output)
}
