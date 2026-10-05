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
	// +default="1.26.6"
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

// BuildAndTestBinary runs the unit tests, builds the binary and runs an
// integration test against Redis: serve with a profile whose only check
// cannot complete offline, then health, readiness, findings ingest, the
// check's could-not-check finding, acknowledge and run --dry-run.
func (m *Dagger) BuildAndTestBinary(
	ctx context.Context,
	source *dagger.Directory,
	// +optional
	// +default="1.26.6"
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

	unit := dag.Container().
		From("golang:"+goVersion).
		WithDirectory("/src", source).
		WithWorkdir("/src").
		WithExec([]string{"sh", "-c", "go test " + testPath + " 2>&1 | tee /tmp/unit-tests.log"})
	if _, err := unit.Sync(ctx); err != nil {
		return unit.File("/tmp/unit-tests.log"), fmt.Errorf("unit tests failed: %w", err)
	}

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

	profile := `apiVersion: homerun2.sthings.io/v1alpha1
kind: SchedulePitcherProfile
metadata:
  name: ci
spec:
  checks:
    - id: unreachable
      type: tls-endpoint
      target: unreachable.invalid:443
      timeout: 2s
`

	testCmd := fmt.Sprintf(`
exec > /app/test-output.log 2>&1
set -e
BASE=http://localhost:%[2]d
AUTH="Authorization: Bearer test-token-12345"
fail() { echo "FAIL: $1"; kill $BIN_PID 2>/dev/null || true; exit 1; }

echo "=== Starting binary ==="
./%[1]s serve --profile /app/profile.yaml &
BIN_PID=$!

echo "=== Waiting for readiness ==="
for i in $(seq 1 30); do curl -sf $BASE/ready >/dev/null && break; sleep 1; done
curl -sf $BASE/health || fail "health"
curl -sf $BASE/ready || fail "ready"

echo ""
echo "=== POST /findings ==="
RES=$(curl -sf -X POST $BASE/findings -H "$AUTH" -d '{"source":"ci","findings":[
  {"key":"vm1/disk:/var","title":"/var at 74%%","severity":"info","value":74,"threshold":70,"host":"vm1"}]}') || fail "ingest"
echo "$RES"
echo "$RES" | grep -q '"new":1' || fail "ingest result"

echo ""
echo "=== The check's could-not-check finding ==="
for i in $(seq 1 20); do
  OPEN=$(curl -sf "$BASE/api/findings?status=open" -H "$AUTH") || fail "list"
  echo "$OPEN" | grep -q 'unreachable:could-not-check' && break
  sleep 1
done
echo "$OPEN"
echo "$OPEN" | grep -q 'unreachable:could-not-check' || fail "check finding missing"
echo "$OPEN" | grep -q 'vm1/disk:/var' || fail "ingested finding missing"

echo ""
echo "=== Acknowledge ==="
curl -sf -X POST $BASE/api/findings/ack -H "$AUTH" \
  -d '{"source":"ci","key":"vm1/disk:/var","by":"ci","note":"test"}' | grep -q '"acknowledged"' || fail "ack"

echo ""
echo "=== Unauthenticated ingest is rejected ==="
CODE=$(curl -s -o /dev/null -w '%%{http_code}' -X POST $BASE/findings -d '{}')
[ "$CODE" = "401" ] || fail "expected 401, got $CODE"

echo ""
echo "=== run --dry-run ==="
./%[1]s run --profile /app/profile.yaml --dry-run || true

echo ""
echo "=== All tests passed! ==="
kill $BIN_PID 2>/dev/null || true
exit 0
`, binName, port)

	result := dag.Container().
		From("alpine:latest").
		WithExec([]string{"apk", "add", "--no-cache", "curl"}, dagger.ContainerWithExecOpts{}).
		WithDirectory("/app", binDir).
		WithNewFile("/app/profile.yaml", profile).
		WithWorkdir("/app").
		WithServiceBinding("redis", redisService).
		WithEnvVariable("REDIS_ADDR", "redis").
		WithEnvVariable("REDIS_PORT", "6379").
		WithEnvVariable("AUTH_TOKEN", "test-token-12345").
		WithEnvVariable("PITCH_TARGET", "stdout").
		WithEnvVariable("PORT", fmt.Sprint(port)).
		WithEnvVariable("LOG_FORMAT", "text").
		WithExec([]string{"sh", "-c", testCmd}, dagger.ContainerWithExecOpts{})

	_, err := result.Sync(ctx)
	if err != nil {
		testLog := result.File("/app/test-output.log")
		return testLog, fmt.Errorf("tests failed - check test-output.log for details: %w", err)
	}

	testLog := result.File("/app/test-output.log")
	return testLog, nil
}

// SmokeTest sends findings reports (a JSON array of POST /findings bodies) to
// a deployed central instance one by one, verifies the responses, resolves
// the test findings again with an empty report per source, and returns a
// test report.
func (m *Dagger) SmokeTest(
	ctx context.Context,
	// The base URL of the deployed pitcher (e.g., https://homerun2-schedule-pitcher.example.com)
	endpoint string,
	// Bearer token for authentication
	authToken *dagger.Secret,
	// JSON array of findings reports ({"source": ..., "findings": [...]})
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
echo "Reports:  $TOTAL"
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

# Send reports one by one
i=0
while [ "$i" -lt "$TOTAL" ]; do
  MSG=$(jq -c ".[$i]" /tmp/messages.json)
  TITLE=$(echo "$MSG" | jq -r '.source')
  SEVERITY=$(echo "$MSG" | jq -r '.findings | length | tostring + " findings"')

  echo "--- Message $((i + 1))/$TOTAL: $TITLE (severity=$SEVERITY) ---"

  HTTP_CODE=$(curl -sk -o /tmp/pitch-response.json -w "%%{http_code}" \
    -X POST "$ENDPOINT/findings" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "$MSG")

  if [ "$HTTP_CODE" = "200" ] || [ "$HTTP_CODE" = "201" ]; then
    SOURCE=$(jq -r '.source' /tmp/pitch-response.json 2>/dev/null)
    OPEN=$(jq -r '.open' /tmp/pitch-response.json 2>/dev/null)
    if [ "$SOURCE" != "null" ] && [ -n "$SOURCE" ]; then
      echo "PASS: HTTP $HTTP_CODE, source=$SOURCE, open=$OPEN"
      PASSED=$((PASSED + 1))
    else
      echo "FAIL: HTTP $HTTP_CODE but no source in the response"
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

# Resolve the test findings again
for SRC in $(jq -r '.[].source' /tmp/messages.json | sort -u); do
  curl -sk -o /dev/null -X POST "$ENDPOINT/findings" -H "Authorization: Bearer $TOKEN" \
    -d "{\"source\":\"$SRC\",\"findings\":[]}" && echo "cleanup: resolved findings of $SRC"
done
echo ""

# Summary
echo "============================================"
echo "SUMMARY"
echo "============================================"
echo "Total:  $((TOTAL + 1)) (health + $TOTAL reports)"
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
