//go:build !unix

package node

func diskOf(string) (uint64, uint64) { return 0, 0 }

func loadOne() float64 { return 0 }
