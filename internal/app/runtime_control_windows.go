//go:build windows

package app

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func runtimeSecurity() (string, error) {
	token := windows.GetCurrentProcessToken()
	user, e := token.GetTokenUser()
	if e != nil {
		return "", e
	}
	return "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + user.User.Sid.String() + ")", nil
}
func secureRuntimeDirectory(dir string, create bool) error {
	if create {
		if e := os.Mkdir(dir, 0700); e != nil {
			if os.IsExist(e) {
				return secureRuntimeDirectory(dir, false)
			}
			return e
		}
	}
	info, e := os.Lstat(dir)
	if e != nil {
		return e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runtime-dir 必须是普通私有目录")
	}
	if !create {
		sd, e := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
		if e != nil {
			return e
		}
		owner, _, e := sd.Owner()
		if e != nil {
			return e
		}
		sddl := sd.String()
		at := strings.Index(sddl, "D:")
		if at < 0 {
			return errors.New("runtime-dir 缺少明确 DACL")
		}
		d := sddl[at+2:]
		if !strings.Contains(strings.SplitN(d, "(", 2)[0], "P") {
			return errors.New("runtime-dir DACL 必须禁止宽泛继承")
		}
		allowed := map[string]bool{"SY": true, "BA": true, "S-1-5-18": true, "S-1-5-32-544": true, owner.String(): true}
		count := 0
		for _, part := range strings.Split(d, "(")[1:] {
			end := strings.Index(part, ")")
			if end < 0 {
				return errors.New("runtime-dir DACL 无效")
			}
			fields := strings.Split(part[:end], ";")
			if len(fields) != 6 || fields[0] != "A" || !allowed[fields[5]] {
				return errors.New("runtime-dir 授权超出所有者与系统管理员")
			}
			count++
		}
		if count == 0 {
			return errors.New("runtime-dir DACL 不能为空")
		}
		return nil
	}
	sddl, e := runtimeSecurity()
	if e != nil {
		return e
	}
	sd, e := windows.SecurityDescriptorFromString(sddl)
	if e != nil {
		return e
	}
	dacl, _, e := sd.DACL()
	if e != nil {
		return e
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
func runtimePipe(id string) string { return `\\.\pipe\webhook-control-` + id }
func listenRuntimeControl(_ string, id string) (net.Listener, error) {
	sddl, e := runtimeSecurity()
	if e != nil {
		return nil, e
	}
	return winio.ListenPipe(runtimePipe(id), &winio.PipeConfig{SecurityDescriptor: sddl})
}
func dialRuntimeControl(ctx context.Context, _ string, id string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, runtimePipe(id))
}
func removeRuntimeEndpoint(_, _ string) {}
func runtimeEndpointAbsent(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED)
}
