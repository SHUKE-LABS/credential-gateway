//go:build linux

package config

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
)

// POSIX ACL entry tags, from uapi/linux/posix_acl.h. The kernel stores an
// extended ACL in the system.posix_acl_access xattr as a 4-byte version header
// (POSIX_ACL_XATTR_VERSION) followed by fixed 8-byte records.
const (
	aclUserObj  = 0x01 // owner
	aclUser     = 0x02 // named user
	aclGroupObj = 0x04 // owning group
	aclGroup    = 0x08 // named group
	aclMask     = 0x10 // effective-rights mask
	aclOther    = 0x20 // everyone else
)

const (
	aclXattrName    = "system.posix_acl_access"
	aclXattrVersion = 0x0002 // POSIX_ACL_XATTR_VERSION
	aclEntrySize    = 8      // uint16 tag + uint16 perm + uint32 id
	aclHeaderSize   = 4      // uint32 version
)

// aclGroupClassAccess inspects the file's POSIX ACL for group-level access.
//
// present is false when the file carries no extended ACL (a minimal ACL is
// represented by the mode bits alone, not stored as an xattr); the caller then
// falls back to the raw mode-bit check. When present, offending names the
// group-class entry that grants access — the owning group (group::), a named
// group, or other:: — or is "" when none does.
//
// Named-user entries and the ACL mask are intentionally ignored: st_mode's
// group bits reflect the mask, not the owning group's real permissions, so the
// mask cannot be trusted, and named-user grants (the admin UI's cg-admin ACL)
// are permitted by design. Entry perms are read raw, not mask-adjusted, so
// group access is judged conservatively.
func aclGroupClassAccess(path string) (present bool, offending string, err error) {
	size, err := syscall.Getxattr(path, aclXattrName, nil)
	if err != nil {
		if errors.Is(err, syscall.ENODATA) || errors.Is(err, syscall.ENOTSUP) {
			return false, "", nil
		}
		return false, "", err
	}
	buf := make([]byte, size)
	n, err := syscall.Getxattr(path, aclXattrName, buf)
	if err != nil {
		if errors.Is(err, syscall.ENODATA) || errors.Is(err, syscall.ENOTSUP) {
			return false, "", nil
		}
		return false, "", err
	}
	buf = buf[:n]

	if len(buf) < aclHeaderSize {
		return false, "", fmt.Errorf("%s too short: %d bytes", aclXattrName, len(buf))
	}
	// The xattr is stored in native byte order.
	if v := binary.NativeEndian.Uint32(buf[:aclHeaderSize]); v != aclXattrVersion {
		return false, "", fmt.Errorf("unsupported %s version %#x", aclXattrName, v)
	}

	body := buf[aclHeaderSize:]
	if len(body)%aclEntrySize != 0 {
		return false, "", fmt.Errorf("%s malformed: %d trailing bytes", aclXattrName, len(body)%aclEntrySize)
	}
	for off := 0; off < len(body); off += aclEntrySize {
		tag := binary.NativeEndian.Uint16(body[off : off+2])
		perm := binary.NativeEndian.Uint16(body[off+2 : off+4])
		if perm == 0 {
			continue
		}
		switch tag {
		case aclGroupObj:
			return true, "owning group (group::)", nil
		case aclGroup:
			return true, "a named group", nil
		case aclOther:
			return true, "other", nil
		}
	}
	return true, "", nil
}
