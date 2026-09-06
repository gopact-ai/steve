package material

import "fmt"

// StageBlobs writes only verified content-addressed files. The transfer
// coordinator commits their already-validated metadata with all other domains.
func (s *Store) StageBlobs(in ProjectExport) error {
	for digest, data := range in.Blobs {
		if !digestPattern.MatchString(digest) || sum(data) != digest {
			return fmt.Errorf("%w: blob digest mismatch", ErrInvalid)
		}
		if err := s.writeBlob(digest, data); err != nil {
			return err
		}
	}
	return nil
}
