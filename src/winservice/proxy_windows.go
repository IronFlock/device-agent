//go:build windows

package winservice

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// fileReadAccess is what SYSTEM and Administrators must keep on the store:
// the agent reads it as SYSTEM, `service configure` as an administrator.
const fileReadAccess = windows.FILE_GENERIC_READ

// WriteProtectedFile writes data to path atomically, with ProxyStoreSDDL as
// the file's DACL from its first byte on: a temporary file next to it is
// created with that security descriptor, written and flushed, then renamed
// over path (the rename keeps the file's own DACL). The result is checked and
// repaired once more (ProtectFile), so a path that existed with a looser DACL
// cannot keep it.
func WriteProtectedFile(path string, data []byte) error {
	sd, err := windows.SecurityDescriptorFromString(ProxyStoreSDDL)
	if err != nil {
		return fmt.Errorf("building the security descriptor: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing a leftover %s: %w", tmp, err)
	}
	tmpName, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	handle, err := windows.CreateFile(tmpName, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("creating %s: %w", tmp, err)
	}
	file := os.NewFile(uintptr(handle), tmp)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	pathName, err := windows.UTF16PtrFromString(path)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := windows.MoveFileEx(tmpName, pathName, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming %s to %s: %w", tmp, path, err)
	}
	_, err = ProtectFile(path)
	return err
}

// FileProtected reports whether path is owned by SYSTEM or Administrators,
// does not inherit (protected DACL), and grants access to SYSTEM and
// Administrators alone, both of whom can read it. Only allow entries are
// expected; anything else counts as not protected, and ProtectFile replaces
// it.
func FileProtected(path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, err
	}
	if owner == nil || !(owner.IsWellKnown(windows.WinLocalSystemSid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid)) {
		return false, nil
	}
	control, _, err := sd.Control()
	if err != nil {
		return false, err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false, nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		// No DACL at all (ERROR_OBJECT_NOT_FOUND): everyone has access.
		return false, nil //nolint:nilerr // a missing DACL is an answer, not an error
	}
	if dacl == nil {
		return false, nil
	}
	var system, admins windows.ACCESS_MASK
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false, nil
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) //nolint:gosec // G103: the SID follows the ACE header (Win32 ACE layout)
		switch {
		case sid.IsWellKnown(windows.WinLocalSystemSid):
			system |= ace.Mask
		case sid.IsWellKnown(windows.WinBuiltinAdministratorsSid):
			admins |= ace.Mask
		default:
			return false, nil
		}
	}
	return system&fileReadAccess == fileReadAccess && admins&fileReadAccess == fileReadAccess, nil
}

// ProtectFile checks path (FileProtected) and, when it is not protected,
// sets ProxyStoreSDDL's DACL (inheritance off) and, if needed, the
// Administrators group as owner. repaired reports whether it changed
// anything.
func ProtectFile(path string) (repaired bool, err error) {
	ok, err := FileProtected(path)
	if err == nil && ok {
		return false, nil
	}
	sd, err := windows.SecurityDescriptorFromString(ProxyStoreSDDL)
	if err != nil {
		return false, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, err
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var owner *windows.SID
	if current, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION); err == nil {
		if sid, _, err := current.Owner(); err == nil && sid != nil && !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			owner, err = windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
			if err != nil {
				return false, err
			}
			info |= windows.OWNER_SECURITY_INFORMATION
		}
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil); err != nil {
		return false, fmt.Errorf("restricting %s to SYSTEM and Administrators: %w", path, err)
	}
	ok, err = FileProtected(path)
	if err != nil {
		return true, err
	}
	if !ok {
		return true, fmt.Errorf("%s is still readable by others after restricting it", path)
	}
	return true, nil
}

func parametersKey(name string) string {
	return serviceKey(name) + `\` + ParametersKey
}

// SetProxyConfigured writes the service's non-secret ProxyConfigured value
// (Parameters subkey), which every user can read.
func SetProxyConfigured(name string, configured bool) error {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, parametersKey(name), registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("opening the parameters of service %s: %w", name, err)
	}
	defer func() { _ = key.Close() }()
	value := uint32(0)
	if configured {
		value = 1
	}
	return key.SetDWordValue(ProxyConfiguredValue, value)
}

// ProxyConfigured reads ProxyConfigured; known is false when it is not there.
func ProxyConfigured(name string) (configured bool, known bool, err error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, parametersKey(name), registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	defer func() { _ = key.Close() }()
	value, _, err := key.GetIntegerValue(ProxyConfiguredValue)
	if errors.Is(err, registry.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return value != 0, true, nil
}

// ServiceKeyExists reports whether the registry holds the service's key,
// which every user can read: whether the service is installed, without the
// service manager.
func ServiceKeyExists(name string) bool {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKey(name), registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	_ = key.Close()
	return true
}
