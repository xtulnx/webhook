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
		user, e := windows.GetCurrentProcessToken().GetTokenUser()
		if e != nil {
			return e
		}
		return validateRuntimeDirectoryDACL(sd, user.User.Sid)
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

func validateRuntimeDirectoryDACL(sd *windows.SECURITY_DESCRIPTOR, currentUser *windows.SID) error {
	control, _, e := sd.Control()
	if e != nil {
		return e
	}
	if control&windows.SE_DACL_PRESENT == 0 {
		return errors.New("runtime-dir 缺少明确 DACL")
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("runtime-dir DACL 必须禁止宽泛继承")
	}
	owner, _, e := sd.Owner()
	if e != nil {
		return e
	}
	// Elevated tokens can default to Administrators as the directory owner,
	// while the DACL created by runtimeSecurity grants the process user's SID.
	adminOwned := owner.IsWellKnown(windows.WinBuiltinAdministratorsSid)
	_, d, _ := strings.Cut(sd.String(), "D:")
	entries := strings.Split(d, "(")[1:]
	if len(entries) == 0 {
		return errors.New("runtime-dir DACL 不能为空")
	}
	for _, entry := range entries {
		ace, _, ok := strings.Cut(entry, ")")
		fields := strings.Split(ace, ";")
		if !ok || len(fields) != 6 || fields[0] != "A" {
			return errors.New("runtime-dir DACL 包含不受支持的授权条目")
		}
		// Resolve both numeric SIDs and the aliases Windows emits in SDDL.
		trustee, e := windows.SecurityDescriptorFromString("O:" + fields[5])
		if e != nil {
			return e
		}
		sid, _, e := trustee.Owner()
		if e != nil {
			return e
		}
		if !sid.Equals(owner) && !sid.IsWellKnown(windows.WinLocalSystemSid) &&
			!sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) && !(adminOwned && sid.Equals(currentUser)) {
			return errors.New("runtime-dir DACL 包含未授权账户")
		}
	}
	return nil
}

func runtimePipe(id string) string { return `\\.\pipe\webhook-control-` + id }
func listenRuntimeControl(_ string, id string) (net.Listener, error) {
	sddl, e := runtimeSecurity()
	if e != nil {
		return nil, e
	}
	// Keep the JSON round trip framed by writes and allow short requests and
	// replies to queue even when the other end has not started reading yet.
	return winio.ListenPipe(runtimePipe(id), &winio.PipeConfig{
		SecurityDescriptor: sddl,
		MessageMode:        true,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
}
func dialRuntimeControl(ctx context.Context, _ string, id string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, runtimePipe(id))
}
func removeRuntimeEndpoint(_, _ string) {}
func runtimeEndpointAbsent(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED)
}
