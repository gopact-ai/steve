package nodewire

import "path"

type FileOp string

// ProjectsDir is where a machine keeps the projects it holds: one
// directory per project under the machine's workspace, apart from the
// worktrees and state the machine puts there too.
func ProjectsDir(workspaceRoot string) string {
	if workspaceRoot == "" {
		return ""
	}
	return path.Join(workspaceRoot, "projects")
}

const (
	FileMkdir       FileOp = "mkdir"
	FileClone       FileOp = "clone"
	FileImportSkill FileOp = "import_skill"
	FileSearchPath  FileOp = "search_path"
)

// FileRequest replaces the remaining fixed platform shell snippets. SearchPath
// reads only PATH; this is not an arbitrary environment or command interface.
type FileRequest struct {
	Op     FileOp `json:"op"`
	Path   string `json:"path,omitempty"`
	Source string `json:"source,omitempty"`
}

type FileReply struct {
	Data  string            `json:"data,omitempty"`
	Error *OperationFailure `json:"error,omitempty"`
}
