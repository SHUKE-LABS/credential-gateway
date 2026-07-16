//go:build !linux

package config

// aclGroupClassAccess reports no extended ACL on non-Linux platforms, so
// checkPermissions falls back to the raw mode-bit check. POSIX ACLs and the
// system.posix_acl_access xattr are a Linux feature; the gateway ships on
// Linux, and this stub only keeps the package building elsewhere for tests.
func aclGroupClassAccess(path string) (present bool, offending string, err error) {
	return false, "", nil
}
