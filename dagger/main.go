// Dagger CI module for homerun2-schedule-pitcher
//
// Provides build, lint, test, image build, and security scanning functions.
// Delegates to external stuttgart-things Dagger modules where possible.

package main

import (
	"context"
	"dagger/dagger/internal/dagger"
	"fmt"
	"strings"
)

type Dagger struct{}

// Lint runs golangci-lint on the source code
func (m *Dagger) Lint(
	ctx context.Context,
	src *dagger.Directory,
	// +optional
	// +default="500s"
	timeout string,
) *dagger.Container {
	return dag.Go().Lint(src, dagger.GoLintOpts{
		Timeout: timeout,
	})
}

// Build compiles the Go binary
func (m *Dagger) Build(
	ctx context.Context,
	src *dagger.Directory,
	// +optional
	// +default="main"
	binName string,
	// +optional
	// +default=""
	ldflags string,
	// +optional
	// +default="1.25.5"
	goVersion string,
	// +optional
	// +default="linux"
	os string,
	// +optional
	// +default="amd64"
	arch string,
) *dagger.Directory {
	return dag.Go().BuildBinary(src, dagger.GoBuildBinaryOpts{
		GoVersion:  goVersion,
		Os:         os,
		Arch:       arch,
		BinName:    binName,
		Ldflags:    ldflags,
		GoMainFile: "main.go",
	})
}

// BuildImage builds a container image using ko and optionally pushes it
func (m *Dagger) BuildImage(
	ctx context.Context,
	src *dagger.Directory,
	// +optional
	// +default="ko.local/homerun2-schedule-pitcher"
	repo string,
	// +optional
	// +default="false"
	push string,
) (string, error) {
	return dag.Go().KoBuild(ctx, src, dagger.GoKoBuildOpts{
		Repo: repo,
		Push: push,
	})
}

// ScanImage scans a container image for vulnerabilities using Trivy
func (m *Dagger) ScanImage(
	ctx context.Context,
	imageRef string,
	// +optional
	// +default="HIGH,CRITICAL"
	severity string,
) *dagger.File {
	return dag.Trivy().ScanImage(imageRef, dagger.TrivyScanImageOpts{
		Severity: severity,
	})
}

// BuildAndTestBinary builds the binary and runs integration tests with Redis
func (m *Dagger) BuildAndTestBinary(
	ctx context.Context,
	source *dagger.Directory,
	// +optional
	// +default="1.25.5"
	goVersion string,
	// +optional
	// +default="linux"
	os string,
	// +optional
	// +default="amd64"
	arch string,
	// +optional
	// +default="main.go"
	goMainFile string,
	// +optional
	// +default="main"
	binName string,
	// +optional
	// +default=""
	ldflags string,
	// +optional
	// +default="."
	packageName string,
	// +optional
	// +default="./..."
	testPath string,
	// +optional
	// +default="8080"
	port int,
) (*dagger.File, error) {

	binDir := dag.Go().BuildBinary(
		source,
		dagger.GoBuildBinaryOpts{
			GoVersion:   goVersion,
			Os:          os,
			Arch:        arch,
			GoMainFile:  goMainFile,
			BinName:     binName,
			Ldflags:     ldflags,
			PackageName: packageName,
		})

	redisService := dag.Homerun().RedisService(dagger.HomerunRedisServiceOpts{
		Version:  "7.2.0-v18",
		Password: "",
	})

	testCmd := fmt.Sprintf(`
exec > /app/test-output.log 2>&1
set -e

echo "=== Starting binary ==="
./%s &
BIN_PID=$!
sleep 3

echo ""
echo "=== Testing health endpoint ==="
curl -f -v http://localhost:%d/health || {
  echo "Health check failed!"
  kill $BIN_PID 2>/dev/null || true
  exit 1
}

echo ""
echo "=== Testing pitch endpoint ==="
curl -f -v -X POST http://localhost:%d/pitch \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test-token-12345" \
  -d '{
    "title": "Test Notification",
    "message": "Testing Redis integration",
    "severity": "info",
    "author": "dagger-test",
    "system": "test-system",
    "tags": "test",
    "assigneeaddress": "test@example.com",
    "assigneename": "Test User"
  }' || {
  echo "Pitch endpoint failed!"
  kill $BIN_PID 2>/dev/null || true
  exit 1
}

echo ""
echo "=== All tests passed! ==="
kill $BIN_PID 2>/dev/null || true
exit 0
`, binName, port, port)

	result := dag.Container().
		From("alpine:latest").
		WithExec([]string{"apk", "add", "--no-cache", "curl"}, dagger.ContainerWithExecOpts{}).
		WithDirectory("/app", binDir).
		WithWorkdir("/app").
		WithServiceBinding("redis", redisService).
		WithEnvVariable("REDIS_ADDR", "redis").
		WithEnvVariable("REDIS_PORT", "6379").
		WithEnvVariable("REDIS_STREAM", "messages").
		WithEnvVariable("AUTH_TOKEN", "test-token-12345").
		WithExec([]string{"sh", "-c", testCmd}, dagger.ContainerWithExecOpts{})

	_, err := result.Sync(ctx)
	if err != nil {
		testLog := result.File("/app/test-output.log")
		return testLog, fmt.Errorf("tests failed - check test-output.log for details: %w", err)
	}

	testLog := result.File("/app/test-output.log")
	return testLog, nil
}

// SmokeTest sends test messages to a deployed pitcher instance sequentially
// with a delay between each, verifies HTTP responses, and returns a test report.
func (m *Dagger) SmokeTest(
	ctx context.Context,
	// The base URL of the deployed pitcher (e.g., https://homerun2-schedule-pitcher.example.com)
	endpoint string,
	// Bearer token for authentication
	authToken *dagger.Secret,
	// JSON file containing test messages
	messagesFile *dagger.File,
	// +optional
	// +default=2
	// Delay in seconds between each message
	delaySec int,
) (*dagger.File, error) {

	testScript := fmt.Sprintf(`#!/bin/sh

ENDPOINT="%s"
TOKEN=$(cat /tmp/auth-token)
DELAY=%d
TOTAL=$(jq length /tmp/messages.json)
PASSED=0
FAILED=0

{
echo "============================================"
echo "SMOKE TEST REPORT"
echo "============================================"
echo "Endpoint: $ENDPOINT"
echo "Messages: $TOTAL"
echo "Delay:    ${DELAY}s between messages"
echo "Started:  $(date -u '+%%Y-%%m-%%dT%%H:%%M:%%SZ')"
echo "============================================"
echo ""

# Health check first
echo "--- Health Check ---"
HTTP_CODE=$(curl -sk -o /tmp/health-response.json -w "%%{http_code}" "$ENDPOINT/health")
if [ "$HTTP_CODE" = "200" ]; then
  echo "PASS: /health returned $HTTP_CODE"
  jq -r '. | "  version=\(.version) commit=\(.commit)"' /tmp/health-response.json 2>/dev/null || true
  PASSED=$((PASSED + 1))
else
  echo "FAIL: /health returned $HTTP_CODE"
  cat /tmp/health-response.json 2>/dev/null || true
  FAILED=$((FAILED + 1))
fi
echo ""

# Send messages one by one
i=0
while [ "$i" -lt "$TOTAL" ]; do
  MSG=$(jq -c ".[$i]" /tmp/messages.json)
  TITLE=$(echo "$MSG" | jq -r '.title')
  SEVERITY=$(echo "$MSG" | jq -r '.severity // "info"')

  echo "--- Message $((i + 1))/$TOTAL: $TITLE (severity=$SEVERITY) ---"

  HTTP_CODE=$(curl -sk -o /tmp/pitch-response.json -w "%%{http_code}" \
    -X POST "$ENDPOINT/pitch" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "$MSG")

  if [ "$HTTP_CODE" = "200" ] || [ "$HTTP_CODE" = "201" ]; then
    STATUS=$(jq -r '.status' /tmp/pitch-response.json 2>/dev/null)
    OBJECT_ID=$(jq -r '.objectId' /tmp/pitch-response.json 2>/dev/null)
    STREAM_ID=$(jq -r '.streamId' /tmp/pitch-response.json 2>/dev/null)
    if [ "$STATUS" = "success" ]; then
      echo "PASS: HTTP $HTTP_CODE, status=$STATUS, objectId=$OBJECT_ID, stream=$STREAM_ID"
      PASSED=$((PASSED + 1))
    else
      echo "FAIL: HTTP $HTTP_CODE but status=$STATUS"
      cat /tmp/pitch-response.json
      FAILED=$((FAILED + 1))
    fi
  else
    echo "FAIL: HTTP $HTTP_CODE"
    cat /tmp/pitch-response.json 2>/dev/null || true
    FAILED=$((FAILED + 1))
  fi

  i=$((i + 1))
  if [ "$i" -lt "$TOTAL" ]; then
    echo "  waiting ${DELAY}s..."
    sleep "$DELAY"
  fi
  echo ""
done

# Summary
echo "============================================"
echo "SUMMARY"
echo "============================================"
echo "Total:  $((TOTAL + 1)) (health + $TOTAL messages)"
echo "Passed: $PASSED"
echo "Failed: $FAILED"
echo "Ended:  $(date -u '+%%Y-%%m-%%dT%%H:%%M:%%SZ')"
echo "============================================"

if [ "$FAILED" -gt 0 ]; then
  echo "RESULT: FAIL"
else
  echo "RESULT: PASS"
fi
} > /tmp/smoke-test-report.txt 2>&1

# Always write result for Go to check
echo "$FAILED" > /tmp/smoke-test-failed-count.txt
`, endpoint, delaySec)

	result := dag.Container().
		From("alpine:latest").
		WithExec([]string{"apk", "add", "--no-cache", "curl", "jq"}).
		WithMountedFile("/tmp/messages.json", messagesFile).
		WithMountedSecret("/tmp/auth-token", authToken).
		WithExec([]string{"sh", "-c", testScript})

	// Read the failed count to determine pass/fail
	failedCount, err := result.File("/tmp/smoke-test-failed-count.txt").Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("smoke test script error: %w", err)
	}

	report := result.File("/tmp/smoke-test-report.txt")

	// Print report to stdout for visibility
	reportContent, _ := report.Contents(ctx)
	fmt.Println(reportContent)

	failedCount = strings.TrimSpace(failedCount)
	if failedCount != "" && failedCount != "0" {
		return report, fmt.Errorf("smoke test completed with %s failure(s)", failedCount)
	}

	return report, nil
}
