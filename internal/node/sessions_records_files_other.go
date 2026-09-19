//go:build !unix

package node

import "errors"

type sessionRecordFiles struct{ created bool }

func prepareSessionRecordFiles(string) (*sessionRecordFiles, error) {
	return nil, errors.New("node records require private-file ownership and exclusive locking support")
}
func (*sessionRecordFiles) validateIdentity() error { return nil }
func (*sessionRecordFiles) close() error            { return nil }
