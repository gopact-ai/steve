//go:build !unix

package transfer

import (
	"errors"
	"os"
)

func syncTransferPath(string, func(*os.File) error) error {
	return errors.New("offline project migration requires Unix directory durability support")
}
