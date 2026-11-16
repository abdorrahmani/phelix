package cmd

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
	"go.yaml.in/yaml/v3"
)

// RemoteAppConfigCommand manages configuration in the registered application directory.
func RemoteAppConfigCommand(req *pb.MonitorCommandRequest) {
	if !phelixgrpc.IsAppConfigCommand(req.GetType()) {
		logs.Warning("grpc", "ignoring unknown configuration command %q", req.GetType())
		return
	}

	task, err := phelixgrpc.TakeAppConfigTask(req.GetRequestId())
	if err != nil {
		logs.Warning("grpc", "configuration task %s unavailable: %v", req.GetRequestId(), err)
		return
	}
	if task.RequestID != req.GetRequestId() || task.Command != req.GetType() || task.AppName != req.GetAppName() {
		logs.Warning("grpc", "configuration task %s does not match the wake-up command", req.GetRequestId())
		return
	}

	info, err := resolveConfigAppInfo(task.AppName, task.LookupByName)
	if err != nil {
		submitAppConfigResult(appConfigFailure(task, err), req.GetRequestId())
		return
	}
	result := executeManagedAppConfigTask(info, task)
	submitAppConfigResult(result, req.GetRequestId())
}

func executeManagedAppConfigTask(info *app.AppInfo, task *phelixgrpc.AppConfigTask) phelixgrpc.AppConfigResult {
	if err := checkSharedConfigAccess(info, task.OwnerAccess); err != nil {
		return appConfigFailure(task, err)
	}
	name, directory, publicPort := info.Name, info.Directory, info.Port
	if task.WorkloadType == "" {
		if state, err := deploy.Load(name); err == nil && state.Mode == deploy.ModeWorker {
			task.WorkloadType = project.TypeWorker
		}
	}
	initial := project.InitialConfig(name, task.WorkloadType, publicPort)
	if initial.Port == 0 && initial.Type != project.TypeWorker && initial.Type != project.TypeJob {
		initial.Port = defaultInitPort
	}
	task.InitialContent, _ = yaml.Marshal(initial)
	result := executeAppConfigTask(directory, task)
	if task.Command == phelixgrpc.AppConfigCommandRead && result.Status == "success" {
		result.InitialContent = string(task.InitialContent)
	}

	if directory != "" {
		result.Error = strings.ReplaceAll(result.Error, directory, "[application]")
		for i := range result.Problems {
			result.Problems[i].Message = strings.ReplaceAll(result.Problems[i].Message, directory, "[application]")
		}
	}
	return result
}

func submitAppConfigResult(result phelixgrpc.AppConfigResult, requestID string) {
	if err := phelixgrpc.SubmitAppConfigResult(result); err != nil {
		logs.Warning("grpc", "failed to submit configuration result %s: %v", requestID, err)
	}
}

func executeAppConfigTask(dir string, task *phelixgrpc.AppConfigTask) (result phelixgrpc.AppConfigResult) {
	defer func() {
		if result.Status == "success" {
			result.PoliciesApplied = true
		}
	}()
	switch task.Command {
	case phelixgrpc.AppConfigCommandRead:
		return readAppConfig(dir, task)
	case phelixgrpc.AppConfigCommandValidate:
		return validateAppConfig(dir, task)
	case phelixgrpc.AppConfigCommandCreate:
		return writeAppConfig(dir, task, project.SaveOptions{MustNotExist: true, AccessPolicy: appConfigAccessPolicy(task)})
	case phelixgrpc.AppConfigCommandSave:
		if task.ExpectedRevision == "" {
			return appConfigFailure(task, phelixerr.New(phelixerr.CodeInvalidArgument,
				"expected_revision is required to update phelix.yaml"))
		}
		return writeAppConfig(dir, task, project.SaveOptions{ExpectedRevision: task.ExpectedRevision, AccessPolicy: appConfigAccessPolicy(task)})
	default:
		return appConfigFailure(task, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"unknown configuration command %q", task.Command))
	}
}

func readAppConfig(dir string, task *phelixgrpc.AppConfigTask) phelixgrpc.AppConfigResult {
	content, err := project.ReadContent(dir)
	if err != nil {
		return appConfigFailure(task, err)
	}
	if err := project.CheckConfigReadAccess(content.Content, task.ReadDeniedPaths); err != nil {
		return appConfigFailure(task, err)
	}
	schema, err := project.EditorSchemaJSONForAccess(task.WriteDeniedPaths, content.Content)
	if err != nil {
		return appConfigFailure(task, err)
	}
	result := phelixgrpc.AppConfigResult{
		RequestID: task.RequestID,
		Status:    "success",
		Exists:    content.Exists,
		Content:   string(content.Content),
		Revision:  content.Revision,
		Schema:    string(schema),
	}
	if content.Exists {
		result.UpdatedAt = content.UpdatedAt.Format(time.RFC3339Nano)
		result.Valid = true
		if _, err := project.ValidateContent(dir, content.Content); err != nil {
			result.Valid = false
			result.Problems = configProblems(content.Content, err)
		}
	}
	return result
}

func validateAppConfig(dir string, task *phelixgrpc.AppConfigTask) phelixgrpc.AppConfigResult {
	result := phelixgrpc.AppConfigResult{RequestID: task.RequestID, Status: "success"}
	if _, err := project.ValidateRemoteContent(dir, []byte(task.Content), appConfigAccessPolicy(task)); err != nil {
		if phelixerr.CodeOf(err) == phelixerr.CodeFilesystem {
			return appConfigFailure(task, err)
		}
		result.Valid = false
		result.Problems = configProblems([]byte(task.Content), err)
		return result
	}
	result.Valid = true
	return result
}

func writeAppConfig(dir string, task *phelixgrpc.AppConfigTask, opts project.SaveOptions) phelixgrpc.AppConfigResult {
	saved, err := project.SaveContent(dir, []byte(task.Content), opts)
	if err != nil {
		result := appConfigFailure(task, err)
		if phelixerr.CodeOf(err) != phelixerr.CodeFilesystem && !errors.Is(err, os.ErrPermission) {
			result.Problems = configProblems([]byte(task.Content), err)
		}
		result.Content = string(saved.Content)
		result.Exists = saved.Exists
		result.Revision = saved.Revision
		return result
	}
	result := phelixgrpc.AppConfigResult{
		RequestID: task.RequestID,
		Status:    "success",
		Exists:    true,
		Content:   string(saved.Content),
		Revision:  saved.Revision,
		Changed:   saved.Changed,
		UpdatedAt: saved.UpdatedAt.Format(time.RFC3339Nano),
		Valid:     true,
	}
	if schema, schemaErr := project.EditorSchemaJSONForAccess(task.WriteDeniedPaths, saved.Content); schemaErr == nil {
		result.Schema = string(schema)
	}
	return result
}

func appConfigFailure(task *phelixgrpc.AppConfigTask, err error) phelixgrpc.AppConfigResult {
	result := phelixgrpc.AppConfigResult{
		RequestID: task.RequestID,
		Status:    "error",
		Error:     configDiagnosticMessage(err),
		ErrorCode: string(phelixerr.CodeOf(err)),
	}
	if errors.Is(err, project.ErrConfigConflict) {
		result.ErrorCode = string(phelixerr.CodeIdempotencyConflict)
	}
	if errors.Is(err, os.ErrPermission) {
		result.ErrorCode = string(phelixerr.CodePermissionDenied)
		result.Error = "permission denied while accessing phelix.yaml"
	} else if phelixerr.CodeOf(err) == phelixerr.CodeFilesystem {
		result.Error = "phelix.yaml could not be accessed safely; check application directory and file permissions"
	}
	result.Problems = []phelixgrpc.AppConfigProblem{{Message: result.Error, Severity: "error"}}
	return result
}

var configProblemPath = regexp.MustCompile(`(?:health|deploy|process|schedule|job|matrix|webhook|security|secrets|hooks|resources)(?:\.[a-z_]+|\[[0-9]+\])*|\b(?:port|watching|name|type)\b`)
var configProblemPosition = regexp.MustCompile(`(?:phelix\.yaml:|line )([0-9]+)(?::([0-9]+))?`)

func configProblems(raw []byte, err error) []phelixgrpc.AppConfigProblem {
	message := configDiagnosticMessage(err)
	problem := phelixgrpc.AppConfigProblem{Message: message, Severity: "error"}
	if match := configProblemPosition.FindStringSubmatch(message); match != nil {
		problem.Line, _ = strconv.Atoi(match[1])
		problem.Column, _ = strconv.Atoi(match[2])
	}
	problem.Path = configProblemPath.FindString(message)
	if problem.Path != "" && problem.Line == 0 {
		var doc yaml.Node
		if yaml.Unmarshal(raw, &doc) == nil && len(doc.Content) > 0 {
			node := doc.Content[0]
			path := strings.ReplaceAll(strings.ReplaceAll(problem.Path, "[", "."), "]", "")
			for _, segment := range strings.Split(path, ".") {
				if node == nil {
					break
				}
				if node.Kind == yaml.MappingNode {
					node = configMappingValue(node, segment)
				} else if node.Kind == yaml.SequenceNode {
					index, err := strconv.Atoi(segment)
					if err != nil || index < 0 || index >= len(node.Content) {
						node = nil
					} else {
						node = node.Content[index]
					}
				} else {
					node = nil
				}
			}
			if node != nil {
				problem.Line, problem.Column = node.Line, node.Column
			}
		}
	}
	return []phelixgrpc.AppConfigProblem{problem}
}

func configMappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func appConfigAccessPolicy(task *phelixgrpc.AppConfigTask) project.ConfigAccessPolicy {
	return project.ConfigAccessPolicy{ReadDeniedPaths: task.ReadDeniedPaths, WriteDeniedPaths: task.WriteDeniedPaths, InitialContent: task.InitialContent}
}

func configDiagnosticMessage(err error) string {
	message := phelixerr.Redact(err.Error())
	if len(message) > 4096 {
		return strings.ToValidUTF8(message[:4096], "") + "..."
	}
	return message
}

func resolveConfigAppInfo(identifier string, lookupByName bool) (*app.AppInfo, error) {
	manager, ok := app.Manager.(*app.AppManager)
	if !ok {
		return nil, phelixerr.New(phelixerr.CodeServer, "application registry is unavailable")
	}
	manager.Lock.Lock()
	defer manager.Lock.Unlock()
	snapshot := func(key string, info *app.AppInfo) *app.AppInfo {
		id := info.ID
		if id == "" {
			id = key
		}
		return &app.AppInfo{ID: id, Name: info.Name, Directory: info.Directory, Port: info.Port}
	}
	if !lookupByName {
		if info, exists := manager.Apps[identifier]; exists && info != nil {
			return snapshot(identifier, info), nil
		}
		return nil, phelixerr.New(phelixerr.CodeNotFound, "application was not found on the selected server")
	}
	var matched *app.AppInfo
	for key, info := range manager.Apps {
		if info == nil || info.Name != identifier {
			continue
		}
		if matched != nil {
			return nil, phelixerr.New(phelixerr.CodePermissionDenied, "application name is ambiguous; target its stable application ID")
		}
		matched = snapshot(key, info)
	}
	if matched == nil {
		return nil, phelixerr.New(phelixerr.CodeNotFound, "application was not found on the selected server")
	}
	return matched, nil
}

func checkSharedConfigAccess(info *app.AppInfo, owner bool) error {
	if owner {
		return nil
	}
	manager, ok := app.Manager.(*app.AppManager)
	if !ok {
		return phelixerr.New(phelixerr.CodeServer, "application registry is unavailable")
	}
	manager.Lock.Lock()
	directories := make([]string, 0, len(manager.Apps))
	for key, other := range manager.Apps {
		if other == nil {
			continue
		}
		id := other.ID
		if id == "" {
			id = key
		}
		if id != info.ID {
			directories = append(directories, other.Directory)
		}
	}
	manager.Lock.Unlock()
	target, err := os.Stat(info.Directory)
	if err != nil {
		return nil
	}
	for _, directory := range directories {
		other, err := os.Stat(directory)
		if err != nil {
			continue
		}
		if os.SameFile(target, other) {
			return phelixerr.New(phelixerr.CodePermissionDenied, "this project directory is shared by applications; only its owner can manage the shared configuration")
		}
	}
	return nil
}
