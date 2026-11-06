//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"golang.org/x/sys/windows/svc"
)

const svcName = "peacefulVA"

// runService 以 Windows 服务方式运行，由 install-windows.ps1 通过 New-Service 注册
func runService(cfg *Config) error {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !isSvc {
		return fmt.Errorf("不在服务上下文中，前台运行请去掉 -svc 参数")
	}
	return svc.Run(svcName, &pvaService{cfg: cfg})
}

type pvaService struct {
	cfg *Config
}

func (m *pvaService) Execute(args []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	srv, err := startServer(m.cfg)
	if err != nil {
		log.Printf("服务启动失败: %v", err)
		return false, 1
	}
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
loop:
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			s <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			s <- svc.Status{State: svc.StopPending}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = srv.Shutdown(ctx)
			cancel()
			RemoveLockFile(m.cfg.DataDir)
			break loop
		default:
		}
	}
	return false, 0
}
