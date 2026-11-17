//go:build !linux

package app

import (
	"context"
	"errors"
)

func servicePlatformOperation(_ context.Context, _ serviceOptions) (interface{}, error) {
	return nil, errors.New("当前平台的原生服务管理尚未实现；本阶段仅支持 Linux systemd")
}
func installedServiceRuntime(_ string) (string, error) { return "", nil }
