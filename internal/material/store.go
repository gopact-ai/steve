package material

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/ledger"
)

const materialKind = "material"
const annotationKind = "material-annotation"

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type Store struct {
	root *os.Root
	book *ledger.Ledger
}

func Open(dir string, book *ledger.Ledger) (*Store, error) {
	if book == nil {
		return nil, fmt.Errorf("%w: ledger required", ErrInvalid)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: blob root is not a directory", ErrInvalid)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Store{root: root, book: book}, nil
}
func (s *Store) Close() error { return s.root.Close() }
func sum(data []byte) string  { digest := sha256.Sum256(data); return hex.EncodeToString(digest[:]) }
func identity(m Material) string {
	m.ID = ""
	m.CreatedAt = time.Time{}
	raw, _ := json.Marshal(m)
	return "m-" + sum(raw)
}
func validName(value string) bool {
	return value != "" && len(value) <= 512 && !strings.ContainsAny(value, "\x00\r\n")
}
func validateSource(source Source) error {
	switch source.Kind {
	case "reply":
		if !validName(source.Conversation) || !validName(source.ReplyID) {
			return fmt.Errorf("%w: reply provenance required", ErrInvalid)
		}
	case "snapshot-file":
		if !validName(source.Attempt) || !commitPattern.MatchString(source.Commit) || source.Path == "" || source.Path == "." || len(source.Path) > 4096 || path.IsAbs(source.Path) || path.Clean(source.Path) != source.Path || strings.HasPrefix(source.Path, "../") || source.Path == ".." || strings.ContainsAny(source.Path, "\x00\\") {
			return fmt.Errorf("%w: invalid snapshot provenance", ErrInvalid)
		}
	case "upload", "attachment":
	default:
		return fmt.Errorf("%w: unknown source kind", ErrInvalid)
	}
	return nil
}
func inspect(data []byte, declared string) (kind, mimeType string, width, height int, err error) {
	if len(data) > MaxBlobBytes {
		return "", "", 0, 0, ErrTooLarge
	}
	mimeType = declared
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	mimeType, _, err = mime.ParseMediaType(mimeType)
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("%w: invalid MIME", ErrInvalid)
	}
	if strings.HasPrefix(mimeType, "image/") {
		if len(data) > MaxImageBytes {
			return "", "", 0, 0, ErrTooLarge
		}
		config, format, e := image.DecodeConfig(bytes.NewReader(data))
		expected := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif"}[format]
		if e != nil || expected == "" || expected != mimeType {
			return "", "", 0, 0, fmt.Errorf("%w: unsupported or mismatched image", ErrInvalid)
		}
		if config.Width <= 0 || config.Height <= 0 || int64(config.Width) > MaxImagePixels/int64(config.Height) {
			return "", "", 0, 0, fmt.Errorf("%w: image pixel limit", ErrTooLarge)
		}
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			return "", "", 0, 0, fmt.Errorf("%w: incomplete image content", ErrInvalid)
		}
		return "image", mimeType, config.Width, config.Height, nil
	}
	if strings.HasPrefix(mimeType, "text/") || mimeType == "application/json" || mimeType == "application/xml" || mimeType == "application/yaml" {
		if len(data) > MaxTextBytes {
			return "", "", 0, 0, ErrTooLarge
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return "", "", 0, 0, fmt.Errorf("%w: text must be UTF-8 without NUL", ErrInvalid)
		}
		return "text", mimeType, 0, 0, nil
	}
	return "binary", mimeType, 0, 0, nil
}
func (s *Store) Capture(ctx context.Context, in CaptureInput) (Material, error) {
	if !validName(in.Project) || !validName(in.Title) {
		return Material{}, fmt.Errorf("%w: project and title required", ErrInvalid)
	}
	if err := validateSource(in.Source); err != nil {
		return Material{}, err
	}
	kind, mimeType, width, height, err := inspect(in.Data, in.MIME)
	if err != nil {
		return Material{}, err
	}
	m := Material{Project: in.Project, Title: in.Title, Kind: kind, MIME: mimeType, Size: int64(len(in.Data)), Digest: sum(in.Data), Source: in.Source, Width: width, Height: height, CreatedAt: time.Now().UTC()}
	m.ID = identity(m)
	if err := s.writeBlob(m.Digest, in.Data); err != nil {
		return Material{}, err
	}
	err = s.book.Update(ctx, func(tx *ledger.Tx) error {
		var old Material
		ok, err := binding(tx, materialKind, m.ID, &old)
		if err != nil {
			return err
		}
		if ok {
			m = old
			return nil
		}
		return tx.PutBinding(materialKind, m.ID, m)
	})
	return m, err
}
func (s *Store) Upload(ctx context.Context, project, title, mimeType string, r io.Reader) (Material, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBlobBytes+1))
	if err != nil {
		return Material{}, err
	}
	return s.Capture(ctx, CaptureInput{Project: project, Title: title, MIME: mimeType, Source: Source{Kind: "upload"}, Data: data})
}
func (s *Store) Get(ctx context.Context, project, id string) (Material, error) {
	var m Material
	ok, err := s.book.GetBinding(ctx, materialKind, id, &m)
	if err != nil {
		return m, err
	}
	if !ok {
		return m, ErrNotFound
	}
	if m.Project != project {
		return Material{}, ErrScope
	}
	if m.ID != id || identity(m) != id || !digestPattern.MatchString(m.Digest) {
		return Material{}, fmt.Errorf("%w: stored identity mismatch", ErrInvalid)
	}
	return m, nil
}
func (s *Store) List(ctx context.Context, project string) ([]Material, error) {
	records, err := s.book.Bindings(ctx, materialKind)
	if err != nil {
		return nil, err
	}
	out := []Material{}
	for _, raw := range records {
		var m Material
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		if m.Project == project {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}
func (s *Store) Content(ctx context.Context, project, id string) (Material, []byte, error) {
	m, err := s.Get(ctx, project, id)
	if err != nil {
		return m, nil, err
	}
	data, err := s.readBlob(m.Digest)
	if err == nil && int64(len(data)) != m.Size {
		err = fmt.Errorf("%w: blob size mismatch", ErrInvalid)
	}
	return m, data, err
}
func (s *Store) readBlob(digest string) ([]byte, error) {
	if !digestPattern.MatchString(digest) {
		return nil, ErrInvalid
	}
	info, err := s.root.Lstat(digest)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxBlobBytes {
		return nil, fmt.Errorf("%w: blob is not a bounded regular file", ErrInvalid)
	}
	f, err := s.root.Open(digest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxBlobBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBlobBytes || sum(data) != digest {
		return nil, fmt.Errorf("%w: blob digest mismatch", ErrInvalid)
	}
	return data, nil
}
func (s *Store) writeBlob(digest string, data []byte) error {
	if !digestPattern.MatchString(digest) || len(data) > MaxBlobBytes || sum(data) != digest {
		return ErrInvalid
	}
	if _, err := s.root.Lstat(digest); err == nil {
		_, err = s.readBlob(digest)
		return err
	} else if !os.IsNotExist(err) {
		return err
	}
	name := ".capture-" + rand.Text()
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer s.root.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Link publishes without replacing another capture's immutable object.
	if err = s.root.Link(name, digest); os.IsExist(err) {
		_, err = s.readBlob(digest)
	}
	if err != nil {
		return err
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) resolveRef(ctx context.Context, project string, ref Ref) (Frozen, error) {
	m, data, err := s.Content(ctx, project, ref.ID)
	if err != nil {
		return Frozen{}, err
	}
	f := Frozen{Ref: ref, Material: m}
	if m.Kind == "text" {
		f.Text, err = selectText(string(data), ref.Selector)
	} else if m.Kind == "image" {
		var selected []byte
		selected, err = selectImage(data, ref.Selector)
		f.Media = &Media{MIME: m.MIME, Digest: m.Digest, Data: selected}
		if ref.Selector != nil {
			f.Media.MIME = "image/png"
			f.Media.Digest = sum(selected)
		}
	} else if ref.Selector != nil {
		err = fmt.Errorf("%w: binary attachment has no selector", ErrInvalid)
	} else {
		f.Media = &Media{MIME: m.MIME, Digest: m.Digest, Data: data}
	}
	return f, err
}

func (s *Store) ResolveRefs(ctx context.Context, project string, refs []Ref) ([]Frozen, error) {
	if len(refs) > MaxRefs {
		return nil, fmt.Errorf("%w: at most %d refs", ErrTooLarge, MaxRefs)
	}
	out := make([]Frozen, 0, len(refs))
	total, mediaBytes := 0, 0
	for _, ref := range refs {
		f, err := s.resolveRef(ctx, project, ref)
		if err != nil {
			return nil, err
		}
		total += len(f.Text)
		if total > MaxResolvedTextBytes {
			return nil, fmt.Errorf("%w: select a smaller text range", ErrTooLarge)
		}
		if f.Media != nil {
			mediaBytes += len(f.Media.Data)
			if mediaBytes > MaxResolvedMediaBytes {
				return nil, fmt.Errorf("%w: attached media exceeds prompt limit", ErrTooLarge)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func selectText(text string, selector *Selector) (string, error) {
	if selector == nil {
		return text, nil
	}
	switch selector.Kind {
	case "lines":
		if selector.Quote != "" || selector.Rect != nil || selector.Start < 1 || selector.End < selector.Start {
			return "", ErrInvalid
		}
		lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
		if selector.End > len(lines) {
			return "", fmt.Errorf("%w: line range outside captured text", ErrInvalid)
		}
		return strings.Join(lines[selector.Start-1:selector.End], "\n"), nil
	case "quote":
		if selector.Start != 0 || selector.End != 0 || selector.Rect != nil || selector.Quote == "" || !utf8.ValidString(selector.Quote) || !strings.Contains(text, selector.Quote) {
			return "", fmt.Errorf("%w: quote is absent from captured text", ErrInvalid)
		}
		return selector.Quote, nil
	default:
		return "", ErrInvalid
	}
}
func selectImage(data []byte, selector *Selector) ([]byte, error) {
	if selector == nil {
		return data, nil
	}
	r := selector.Rect
	if selector.Kind != "rect" || selector.Start != 0 || selector.End != 0 || selector.Quote != "" || r == nil || math.IsNaN(r.X+r.Y+r.Width+r.Height) || r.X < 0 || r.Y < 0 || r.Width <= 0 || r.Height <= 0 || r.X+r.Width > 1 || r.Y+r.Height > 1 {
		return nil, fmt.Errorf("%w: image rectangle outside original bounds", ErrInvalid)
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid image content", ErrInvalid)
	}
	bounds := source.Bounds()
	x0 := int(math.Floor(r.X * float64(bounds.Dx())))
	y0 := int(math.Floor(r.Y * float64(bounds.Dy())))
	x1 := int(math.Ceil((r.X + r.Width) * float64(bounds.Dx())))
	y1 := int(math.Ceil((r.Y + r.Height) * float64(bounds.Dy())))
	cropped := image.NewRGBA(image.Rect(0, 0, x1-x0, y1-y0))
	draw.Draw(cropped, cropped.Bounds(), source, bounds.Min.Add(image.Pt(x0, y0)), draw.Src)
	var out bytes.Buffer
	err = png.Encode(&out, cropped)
	if err != nil {
		return nil, err
	}
	if out.Len() > MaxImageBytes {
		return nil, ErrTooLarge
	}
	return out.Bytes(), nil
}
