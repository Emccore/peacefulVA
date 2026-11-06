//go:build !windows

package main

import "errors"

func runService(cfg *Config) error {
	return errors.New("-svc 仅支持 Windows，请直接运行服务")
}
