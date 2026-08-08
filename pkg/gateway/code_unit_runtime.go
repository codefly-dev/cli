package gateway

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codefly-dev/cli/pkg/engine"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	selectioncontract "github.com/codefly-dev/core/runners/testselection"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	maxGatewayCodeUnits            = 32
	maxCodeUnitAggregateOutputSize = 512 << 10
)

// normalizedCodeUnitTarget is the execution-box view of an ingestion-declared
// boundary. The caller supplies only identity and path; Codefly resolves the
// physical root and selects the runtime plugin from evidence inside that root.
type normalizedCodeUnitTarget struct {
	id   string
	path string
	root string
}

type codeUnitTestRun struct {
	target  normalizedCodeUnitTarget
	request *runtimev0.TestRequest
}

type codeUnitTestResult struct {
	target   normalizedCodeUnitTarget
	response *runtimev0.TestResponse
}

func (s *Server) normalizeCodeUnitTargets(targets []*gatewayv1.CodeUnitTarget) ([]normalizedCodeUnitTarget, error) {
	if len(targets) > maxGatewayCodeUnits {
		return nil, fmt.Errorf("code-unit target count %d exceeds limit %d", len(targets), maxGatewayCodeUnits)
	}
	seenIDs := make(map[string]struct{}, len(targets))
	seenPaths := make(map[string]struct{}, len(targets))
	out := make([]normalizedCodeUnitTarget, 0, len(targets))
	for index, target := range targets {
		if target == nil {
			return nil, fmt.Errorf("code unit %d is nil", index)
		}
		id := strings.TrimSpace(target.GetId())
		if id == "" {
			return nil, fmt.Errorf("code unit %d has no id", index)
		}
		if _, duplicate := seenIDs[id]; duplicate {
			return nil, fmt.Errorf("duplicate code-unit id %q", id)
		}
		unitPath, err := canonicalCodeUnitPath(target.GetPath())
		if err != nil {
			return nil, fmt.Errorf("code unit %q: %w", id, err)
		}
		if _, duplicate := seenPaths[unitPath]; duplicate {
			return nil, fmt.Errorf("duplicate code-unit path %q", unitPath)
		}
		root, err := s.resolveCommandDir(unitPath)
		if err != nil {
			return nil, fmt.Errorf("code unit %q path %q: %w", id, unitPath, err)
		}
		seenIDs[id] = struct{}{}
		seenPaths[unitPath] = struct{}{}
		out = append(out, normalizedCodeUnitTarget{id: id, path: unitPath, root: root})
	}
	return out, nil
}

func canonicalCodeUnitPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." {
		return ".", nil
	}
	if strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("path contains NUL byte")
	}
	if strings.Contains(value, "\\") {
		return "", fmt.Errorf("path must use canonical forward slashes: %q", value)
	}
	if path.IsAbs(value) {
		return "", fmt.Errorf("path must be repository-relative: %q", value)
	}
	clean := path.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes repository: %q", value)
	}
	if clean == "." {
		return ".", nil
	}
	return strings.TrimPrefix(clean, "./"), nil
}

// serviceBehaviorForCodeUnit binds an exact source root. It never reuses the
// root behavior: that cache is precisely what made a heterogeneous project
// route every unit through the first detected plugin. agentOverride is typed
// Codefly policy (for example a runtime formula), never a native command.
func (s *Server) serviceBehaviorForCodeUnit(target normalizedCodeUnitTarget, agentOverride string) (serviceExecution, error) {
	if s.host == nil {
		return nil, fmt.Errorf("workspace host is unavailable")
	}
	agentName, err := engine.DetectSourceAgent(target.root)
	if strings.TrimSpace(agentOverride) != "" {
		agentName = strings.TrimSpace(agentOverride)
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("detect agent at code unit %q (%s): %w", target.id, target.path, err)
	}
	key := target.id + "\x00" + target.path + "\x00" + agentName
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	if service := s.codeUnitServices[key]; service != nil {
		return service, nil
	}
	service, err := s.host.Service(engine.ServiceTarget{
		Root:        target.root,
		Agent:       agentName,
		ForceSource: true,
	})
	if err != nil {
		return nil, fmt.Errorf("bind code unit %q (%s): %w", target.id, target.path, err)
	}
	if s.codeUnitServices == nil {
		s.codeUnitServices = make(map[string]serviceExecution)
	}
	s.codeUnitServices[key] = service
	return service, nil
}

func (s *Server) executionServiceBehaviorForCodeUnit(target normalizedCodeUnitTarget, request *runtimev0.TestRequest) (serviceExecution, error) {
	agentOverride := ""
	if request != nil && request.GetFormula() != nil {
		agentOverride = engine.DetectFormulaAgent(request.GetFormula().GetCommand())
	}
	return s.serviceBehaviorForCodeUnit(target, agentOverride)
}

func (s *Server) testCodeUnits(ctx context.Context, request *runtimev0.TestRequest, targets []normalizedCodeUnitTarget) (*runtimev0.TestResponse, error) {
	runs, err := routeCodeUnitTestRequest(request, targets)
	if err != nil {
		return codeUnitTestError(request, err.Error()), nil
	}
	results := make([]codeUnitTestResult, len(runs))
	var wait sync.WaitGroup
	for index := range runs {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			run := runs[index]
			result := codeUnitTestResult{target: run.target}
			service, bindErr := s.executionServiceBehaviorForCodeUnit(run.target, run.request)
			if bindErr != nil {
				result.response = codeUnitTestError(run.request, bindErr.Error())
				results[index] = result
				return
			}
			response, testErr := service.Test(ctx, run.request)
			if testErr != nil {
				result.response = codeUnitTestError(run.request, fmt.Sprintf("plugin test RPC failed for code unit %q (%s): %v", run.target.id, run.target.path, testErr))
				results[index] = result
				return
			}
			if err := selectioncontract.VerifyAcknowledgement(run.request, response); err != nil {
				result.response = codeUnitTestError(run.request, fmt.Sprintf("code unit %q did not honor the authoritative selection: %v", run.target.id, err))
				results[index] = result
				return
			}
			result.response = rebaseCodeUnitTestResponse(run.target, request, response)
			results[index] = result
		}()
	}
	wait.Wait()
	if len(results) == 1 {
		return results[0].response, nil
	}
	return aggregateCodeUnitTestResponses(request, results), nil
}

func routeCodeUnitTestRequest(request *runtimev0.TestRequest, targets []normalizedCodeUnitTarget) ([]codeUnitTestRun, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("code-unit test request has no targets")
	}
	base := &runtimev0.TestRequest{}
	if request != nil {
		base = proto.Clone(request).(*runtimev0.TestRequest)
	}
	selection := base.GetSelection()
	if selection == nil {
		return testRunsForAllCodeUnits(base, targets), nil
	}

	selectionPath, packageID, routeByBoundary, err := selectionBoundary(selection)
	if err != nil {
		return nil, err
	}
	if !routeByBoundary {
		// A named suite is intentionally project-wide: each declared unit asks
		// its own plugin to honor that typed suite identity.
		return testRunsForAllCodeUnits(base, targets), nil
	}
	owner, ok := owningCodeUnit(targets, selectionPath, packageID)
	if !ok {
		return nil, fmt.Errorf("authoritative test selection cannot be assigned to one declared code unit (path=%q package=%q)", selectionPath, packageID)
	}
	forwarded := proto.Clone(base).(*runtimev0.TestRequest)
	if selectionPath != "" {
		relative, err := selectionPathWithinCodeUnit(selectionPath, owner.path, len(targets) == 1)
		if err != nil {
			return nil, err
		}
		rewriteSelectionPath(forwarded.GetSelection(), relative)
	}
	return []codeUnitTestRun{{target: owner, request: forwarded}}, nil
}

func testRunsForAllCodeUnits(request *runtimev0.TestRequest, targets []normalizedCodeUnitTarget) []codeUnitTestRun {
	runs := make([]codeUnitTestRun, 0, len(targets))
	for _, target := range targets {
		runs = append(runs, codeUnitTestRun{
			target:  target,
			request: proto.Clone(request).(*runtimev0.TestRequest),
		})
	}
	return runs
}

func selectionBoundary(selection *runtimev0.TestSelection) (selectionPath, packageID string, route bool, err error) {
	if selection == nil {
		return "", "", false, nil
	}
	switch scope := selection.GetScope().(type) {
	case *runtimev0.TestSelection_File:
		selectionPath, err = canonicalSelectionPath(scope.File.GetPath())
		return selectionPath, "", true, err
	case *runtimev0.TestSelection_TestCase:
		if scope.TestCase == nil {
			return "", "", true, fmt.Errorf("test-case selection is nil")
		}
		packageID = strings.TrimSpace(scope.TestCase.GetPackage())
		if scope.TestCase.GetPath() != "" {
			selectionPath, err = canonicalSelectionPath(scope.TestCase.GetPath())
			return selectionPath, packageID, true, err
		}
		if packageID == "" {
			return "", "", true, fmt.Errorf("test-case selection has neither path nor package")
		}
		return "", packageID, true, nil
	case *runtimev0.TestSelection_Package:
		if scope.Package == nil || strings.TrimSpace(scope.Package.GetPackage()) == "" {
			return "", "", true, fmt.Errorf("package selection is empty")
		}
		return "", strings.TrimSpace(scope.Package.GetPackage()), true, nil
	case *runtimev0.TestSelection_Suite:
		if scope.Suite == nil || strings.TrimSpace(scope.Suite.GetName()) == "" {
			return "", "", false, fmt.Errorf("suite selection is empty")
		}
		return "", "", false, nil
	default:
		return "", "", true, fmt.Errorf("test selection has no scope")
	}
}

func canonicalSelectionPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("test selection path is empty")
	}
	if strings.Contains(value, "\\") || path.IsAbs(value) {
		return "", fmt.Errorf("test selection path must be canonical and repository-relative: %q", value)
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("test selection path is outside a source file boundary: %q", value)
	}
	return strings.TrimPrefix(clean, "./"), nil
}

func owningCodeUnit(targets []normalizedCodeUnitTarget, selectionPath, packageID string) (normalizedCodeUnitTarget, bool) {
	if len(targets) == 1 {
		return targets[0], true
	}
	best := -1
	bestDepth := -1
	for index, target := range targets {
		matched := false
		if selectionPath != "" {
			matched = target.path == "." || selectionPath == target.path || strings.HasPrefix(selectionPath, target.path+"/")
		}
		if !matched && packageID != "" {
			matched = packageID == target.id || strings.HasPrefix(packageID, target.id+"/")
		}
		if !matched {
			continue
		}
		depth := 0
		if target.path != "." {
			depth = strings.Count(target.path, "/") + 1
		}
		if depth > bestDepth {
			best = index
			bestDepth = depth
		}
	}
	if best < 0 {
		return normalizedCodeUnitTarget{}, false
	}
	return targets[best], true
}

func selectionPathWithinCodeUnit(selectionPath, unitPath string, soleUnit bool) (string, error) {
	if unitPath == "." {
		return selectionPath, nil
	}
	if selectionPath == unitPath {
		return ".", nil
	}
	if strings.HasPrefix(selectionPath, unitPath+"/") {
		return strings.TrimPrefix(selectionPath, unitPath+"/"), nil
	}
	if soleUnit {
		// Callers already scoped to one nested code unit commonly express test
		// paths relative to that unit. With no competing boundary, this is
		// unambiguous and remains fail-closed.
		return selectionPath, nil
	}
	return "", fmt.Errorf("test selection path %q is outside code unit %q", selectionPath, unitPath)
}

func rewriteSelectionPath(selection *runtimev0.TestSelection, value string) {
	if selection == nil {
		return
	}
	switch scope := selection.GetScope().(type) {
	case *runtimev0.TestSelection_File:
		if scope.File != nil {
			scope.File.Path = value
		}
	case *runtimev0.TestSelection_TestCase:
		if scope.TestCase != nil {
			scope.TestCase.Path = value
		}
	}
}

func codeUnitTestError(request *runtimev0.TestRequest, message string) *runtimev0.TestResponse {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "code-unit runtime failed"
	}
	return &runtimev0.TestResponse{
		Status: &runtimev0.TestStatus{State: runtimev0.TestStatus_ERROR, Message: message},
		Run:    &runtimev0.TestRun{Runner: "codefly-gateway"},
		Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_ERRORED, Message: message},
		Counts: &runtimev0.TestCounts{},
		Output: message,
	}
}

func rebaseCodeUnitTestResponse(target normalizedCodeUnitTarget, originalRequest *runtimev0.TestRequest, response *runtimev0.TestResponse) *runtimev0.TestResponse {
	if response == nil {
		return codeUnitTestError(originalRequest, fmt.Sprintf("code unit %q returned no runtime response", target.id))
	}
	out := proto.Clone(response).(*runtimev0.TestResponse)
	for _, suite := range out.GetSuites() {
		rebaseCodeUnitSuite(target.path, suite)
	}
	if originalRequest != nil && originalRequest.GetSelection() != nil {
		if out.Run == nil {
			out.Run = &runtimev0.TestRun{}
		}
		out.Run.RequestedSelection = proto.Clone(originalRequest.GetSelection()).(*runtimev0.TestSelection)
		out.Run.SelectionId = originalRequest.GetSelectionId()
	}
	return out
}

func rebaseCodeUnitSuite(unitPath string, suite *runtimev0.TestSuite) {
	if suite == nil {
		return
	}
	suite.File = rebaseCodeUnitFile(unitPath, suite.GetFile())
	for _, testCase := range suite.GetCases() {
		if location := testCase.GetLocation(); location != nil {
			location.File = rebaseCodeUnitFile(unitPath, location.GetFile())
		}
		if failure := testCase.GetFailure(); failure != nil && failure.GetSourceLocation() != nil {
			failure.SourceLocation.File = rebaseCodeUnitFile(unitPath, failure.GetSourceLocation().GetFile())
		}
	}
	for _, child := range suite.GetSuites() {
		rebaseCodeUnitSuite(unitPath, child)
	}
}

func rebaseCodeUnitFile(unitPath, file string) string {
	file = filepath.ToSlash(strings.TrimSpace(file))
	if file == "" || unitPath == "." || path.IsAbs(file) {
		return file
	}
	clean := path.Clean(file)
	if clean == unitPath || strings.HasPrefix(clean, unitPath+"/") {
		return clean
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return file
	}
	return path.Join(unitPath, clean)
}

func aggregateCodeUnitTestResponses(request *runtimev0.TestRequest, results []codeUnitTestResult) *runtimev0.TestResponse {
	counts := &runtimev0.TestCounts{}
	truncation := &runtimev0.TestTruncation{}
	state := runtimev0.TestRunResult_PASSED
	var duration time.Duration
	var suites []*runtimev0.TestSuite
	var summaries []string
	var output strings.Builder
	for _, result := range results {
		response := result.response
		if response == nil {
			response = codeUnitTestError(request, fmt.Sprintf("code unit %q returned no runtime response", result.target.id))
		}
		unitCounts := response.GetCounts()
		if unitCounts == nil {
			total, passed, failed, skipped := runtimeTestCounts(response)
			unitCounts = &runtimev0.TestCounts{Total: total, Passed: passed, Failed: failed, Skipped: skipped}
		}
		counts.Total += unitCounts.GetTotal()
		counts.Passed += unitCounts.GetPassed()
		counts.Failed += unitCounts.GetFailed()
		counts.Skipped += unitCounts.GetSkipped()
		counts.Errored += unitCounts.GetErrored()
		counts.Flaky += unitCounts.GetFlaky()
		unitState := effectiveRuntimeTestState(response)
		state = dominantTestState(state, unitState)
		summary := unitState.String()
		if message := strings.TrimSpace(response.GetResult().GetMessage()); message != "" {
			summary += ": " + message
		}
		summaries = append(summaries, result.target.id+"="+summary)
		if run := response.GetRun(); run != nil && run.GetDuration() != nil {
			if err := run.GetDuration().CheckValid(); err == nil {
				duration += run.GetDuration().AsDuration()
			}
		}
		if unitTruncation := response.GetTruncation(); unitTruncation != nil {
			truncation.Happened = truncation.Happened || unitTruncation.GetHappened()
			truncation.MaxPerCaseBytes = smallestPositive(truncation.GetMaxPerCaseBytes(), unitTruncation.GetMaxPerCaseBytes())
			truncation.MaxTotalBytes = smallestPositive(truncation.GetMaxTotalBytes(), unitTruncation.GetMaxTotalBytes())
			truncation.TruncatedCases += unitTruncation.GetTruncatedCases()
			truncation.DroppedCases += unitTruncation.GetDroppedCases()
		}
		suites = append(suites, &runtimev0.TestSuite{
			Name:   "code-unit:" + result.target.id,
			File:   result.target.path,
			Counts: proto.Clone(unitCounts).(*runtimev0.TestCounts),
			Suites: cloneTestSuites(response.GetSuites()),
		})
		appendBoundedCodeUnitOutput(&output, result.target, response)
	}

	message := fmt.Sprintf("code-unit tests %s across %d units", strings.ToLower(state.String()), len(results))
	if state != runtimev0.TestRunResult_PASSED {
		message += ": " + strings.Join(summaries, "; ")
	}
	statusState := runtimev0.TestStatus_SUCCESS
	if state == runtimev0.TestRunResult_ERRORED || state == runtimev0.TestRunResult_TIMED_OUT || state == runtimev0.TestRunResult_UNKNOWN {
		statusState = runtimev0.TestStatus_ERROR
	}
	run := &runtimev0.TestRun{Runner: "codefly-multi-unit"}
	if request != nil {
		run.SuiteName = request.GetSuite()
		if request.GetSelection() != nil {
			run.RequestedSelection = proto.Clone(request.GetSelection()).(*runtimev0.TestSelection)
			run.SelectionId = request.GetSelectionId()
		}
	}
	if duration > 0 {
		run.Duration = durationpb.New(duration)
	}
	response := &runtimev0.TestResponse{
		Status:       &runtimev0.TestStatus{State: statusState, Message: message},
		Run:          run,
		Result:       &runtimev0.TestRunResult{State: state, Message: message},
		Counts:       counts,
		Suites:       suites,
		Output:       output.String(),
		TestsRun:     counts.GetTotal(),
		TestsPassed:  counts.GetPassed(),
		TestsFailed:  counts.GetFailed() + counts.GetErrored(),
		TestsSkipped: counts.GetSkipped(),
	}
	if truncation.GetHappened() || truncation.GetTruncatedCases() > 0 || truncation.GetDroppedCases() > 0 {
		response.Truncation = truncation
	}
	return response
}

func dominantTestState(current, candidate runtimev0.TestRunResult_State) runtimev0.TestRunResult_State {
	rank := func(state runtimev0.TestRunResult_State) int {
		switch state {
		case runtimev0.TestRunResult_ERRORED:
			return 5
		case runtimev0.TestRunResult_TIMED_OUT:
			return 4
		case runtimev0.TestRunResult_UNKNOWN:
			return 3
		case runtimev0.TestRunResult_FAILED:
			return 2
		case runtimev0.TestRunResult_PASSED:
			return 1
		default:
			return 3
		}
	}
	if rank(candidate) > rank(current) {
		return candidate
	}
	return current
}

func effectiveRuntimeTestState(response *runtimev0.TestResponse) runtimev0.TestRunResult_State {
	if response == nil {
		return runtimev0.TestRunResult_ERRORED
	}
	if state := response.GetResult().GetState(); state != runtimev0.TestRunResult_UNKNOWN {
		return state
	}
	// Some production agents still expose a successful composite invocation
	// through typed status/counts while leaving the additive run-result enum at
	// UNKNOWN. Match the gateway's established success interpretation so an
	// aggregate does not turn real passing evidence into a false error.
	if runtimeTestSuccess(response) {
		return runtimev0.TestRunResult_PASSED
	}
	if response.GetStatus().GetState() == runtimev0.TestStatus_ERROR {
		return runtimev0.TestRunResult_ERRORED
	}
	return runtimev0.TestRunResult_FAILED
}

func smallestPositive(current, candidate int32) int32 {
	if current <= 0 {
		return candidate
	}
	if candidate > 0 && candidate < current {
		return candidate
	}
	return current
}

func cloneTestSuites(suites []*runtimev0.TestSuite) []*runtimev0.TestSuite {
	out := make([]*runtimev0.TestSuite, 0, len(suites))
	for _, suite := range suites {
		if suite != nil {
			out = append(out, proto.Clone(suite).(*runtimev0.TestSuite))
		}
	}
	return out
}

func appendBoundedCodeUnitOutput(output *strings.Builder, target normalizedCodeUnitTarget, response *runtimev0.TestResponse) {
	if output.Len() >= maxCodeUnitAggregateOutputSize {
		return
	}
	body := strings.TrimSpace(response.GetOutput())
	if body == "" {
		body = strings.TrimSpace(response.GetResult().GetMessage())
	}
	section := fmt.Sprintf("[%s @ %s]\n%s\n", target.id, target.path, body)
	remaining := maxCodeUnitAggregateOutputSize - output.Len()
	if len(section) > remaining {
		section = truncateToUTF8Boundary(section, remaining)
	}
	output.WriteString(section)
}

// truncateToUTF8Boundary returns the longest prefix of s no longer than limit
// bytes that ends on a UTF-8 rune boundary. The aggregate Output is a proto3
// string field, which must be valid UTF-8: a plain byte-boundary cut can split
// a multibyte rune, and the resulting TestResponse then fails to marshal —
// turning a bounded-output safeguard into a hard RPC error.
func truncateToUTF8Boundary(s string, limit int) string {
	if limit >= len(s) {
		return s
	}
	truncated := s[:limit]
	for len(truncated) > 0 {
		if r, size := utf8.DecodeLastRuneInString(truncated); r == utf8.RuneError && size <= 1 {
			truncated = truncated[:len(truncated)-1]
			continue
		}
		break
	}
	return truncated
}
