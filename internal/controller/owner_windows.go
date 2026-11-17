//go:build windows

package controller

import "fmt"

type Owner struct{}

func Acquire(directory string) (*Owner, error) {
	return nil, fmt.Errorf("Controller Unix socket mode requires Unix")
}
func (o *Owner) Close() error { return nil }
