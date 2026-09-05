package nodewire

type FileOp string

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
