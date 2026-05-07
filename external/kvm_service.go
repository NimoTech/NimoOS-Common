package external

import (
	"path/filepath"
)

const KVMServiceAddressFilename = "kvm-service.url"

func GetKVMServiceAddress(runtimePath string) (string, error) {
	return getAddress(filepath.Join(runtimePath, KVMServiceAddressFilename))
}