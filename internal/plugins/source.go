package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var commitShape = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

// Resolve reads a local snapshot or a full Git commit. No checkout is used:
// attributes, filters, LFS smudge and repository hooks are never executed.
func Resolve(ctx context.Context, source Source) (Bundle, Source, error) {
	var err error
	source, err = normalizeSource(source)
	if err != nil {
		return Bundle{}, source, err
	}
	if source.Kind == "directory" {
		bundle, err := ReadDirectory(ctx, source.Location)
		return bundle, source, err
	}
	bundle, err := readGit(ctx, source)
	return bundle, source, err
}

func validateGitSource(source Source) error {
	if !commitShape.MatchString(source.Commit) || source.Subdir != "" && !packagePath(source.Subdir) {
		return fmt.Errorf("%w: Git requires a full lowercase commit ID and a package-relative subdirectory", ErrInvalid)
	}
	if filepath.IsAbs(source.Location) {
		return nil
	}
	u, err := url.Parse(source.Location)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(source.Location, "\x00\r\n") {
		return fmt.Errorf("%w: Git source must be an absolute local repository or credential-free HTTPS URL", ErrInvalid)
	}
	return nil
}

func readGit(ctx context.Context, source Source) (Bundle, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "steve-plugin-git-")
	if err != nil {
		return Bundle{}, err
	}
	defer removeStaging(dir)
	git := gitReader{dir: dir}
	args := []string{"init", "--bare", "--quiet", "--template="}
	if len(source.Commit) == 64 {
		args = append(args, "--object-format=sha256")
	}
	if _, err := git.run(ctx, 4096, args...); err != nil {
		return Bundle{}, err
	}
	if _, err := git.run(ctx, 4096, "fetch", "--quiet", "--depth=1", "--no-tags", "--no-recurse-submodules", source.Location, source.Commit); err != nil {
		return Bundle{}, err
	}
	actual, err := git.run(ctx, 256, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return Bundle{}, err
	}
	if strings.TrimSpace(string(actual)) != source.Commit {
		return Bundle{}, fmt.Errorf("%w: fetched Git commit differs", ErrIntegrity)
	}
	tree := source.Commit
	if source.Subdir != "" {
		tree += ":" + source.Subdir
	}
	entries, err := git.run(ctx, MaxPackageBytes, "ls-tree", "-rz", "--full-tree", tree)
	if err != nil {
		return Bundle{}, err
	}
	files, err := git.readFiles(ctx, entries)
	if err != nil {
		return Bundle{}, err
	}
	return packFiles(files)
}

type gitReader struct{ dir string }

func (g gitReader) readFiles(ctx context.Context, raw []byte) (map[string]packageFile, error) {
	files := map[string]packageFile{}
	total := 0
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		metadata, name, ok := bytes.Cut(entry, []byte{'\t'})
		fields := strings.Fields(string(metadata))
		if !ok || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || !packagePath(string(name)) {
			return nil, fmt.Errorf("%w: Git package requires regular files; links and submodules are not supported", ErrInvalid)
		}
		if len(files) >= MaxFiles {
			return nil, fmt.Errorf("%w: too many package files", ErrInvalid)
		}
		data, err := g.run(ctx, MaxFileBytes, "cat-file", "blob", fields[2])
		if err != nil {
			return nil, err
		}
		total += len(data)
		if total > MaxPackageBytes {
			return nil, fmt.Errorf("%w: package size limit", ErrInvalid)
		}
		files[string(name)] = packageFile{data: data, executable: fields[0] == "100755"}
	}
	return files, nil
}

func (g gitReader) run(ctx context.Context, limit int, args ...string) ([]byte, error) {
	prefix := []string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "protocol.https.allow=always", "-c", "http.followRedirects=false"}
	command := exec.CommandContext(ctx, "git", append(prefix, args...)...)
	command.Dir = g.dir
	command.Env = gitEnvironment(g.dir)
	command.WaitDelay = time.Second
	out := &limitedBuffer{limit: limit}
	diagnostic := &limitedBuffer{limit: 4096}
	command.Stdout = out
	command.Stderr = diagnostic
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if out.exceeded {
			return nil, fmt.Errorf("%w: Git object exceeds size limit", ErrInvalid)
		}
		return nil, fmt.Errorf("read plugin Git source: %w: %s", err, strings.TrimSpace(diagnostic.String()))
	}
	return out.Bytes(), nil
}

func gitEnvironment(dir string) []string {
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "GIT_") || key == "HOME" || key == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, value)
	}
	return append(env, "HOME="+dir, "XDG_CONFIG_HOME="+dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(raw []byte) (int, error) {
	if len(raw) > b.limit-b.Len() {
		b.exceeded = true
		return 0, errors.New("output exceeds limit")
	}
	return b.buffer.Write(raw)
}

func (b *limitedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *limitedBuffer) String() string { return b.buffer.String() }
func (b *limitedBuffer) Len() int       { return b.buffer.Len() }
