//go:build windows

package trust

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// storeNames are the certificate stores worth reading. ROOT holds the trust
// anchors; CA holds the intermediates, which a corporate PKI needs for a
// container to build a chain from the appliance's leaf certificate up to the
// root.
var storeNames = []string{"ROOT", "CA"}

// storeLocations are read in order and merged.
//
// LOCAL_MACHINE is where a corporate root lands when it is deployed by policy
// or installed for the whole machine, and it is what matters here: the agent
// runs as a service under SYSTEM. CURRENT_USER is read as well so a root
// installed only for the operator's account is still passed on — Windows
// presents that store as a collection that also includes the machine's, so the
// two overlap heavily and buildPEM deduplicates the result.
var storeLocations = []uint32{
	windows.CERT_SYSTEM_STORE_LOCAL_MACHINE,
	windows.CERT_SYSTEM_STORE_CURRENT_USER,
}

// hostRootDERs enumerates the machine's certificate stores.
//
// Go's x509.SystemCertPool cannot be enumerated on Windows — it defers to the
// platform verifier and hands back an opaque pool — so the stores are read
// directly.
func hostRootDERs() ([][]byte, error) {
	var ders [][]byte

	for _, location := range storeLocations {
		for _, storeName := range storeNames {
			name, err := windows.UTF16PtrFromString(storeName)
			if err != nil {
				continue
			}

			store, err := windows.CertOpenStore(
				windows.CERT_STORE_PROV_SYSTEM,
				0,
				0,
				location|windows.CERT_STORE_READONLY_FLAG,
				uintptr(unsafe.Pointer(name)), //nolint:gosec // G103: Win32 certificate store API calls
			)
			if err != nil {
				continue
			}

			ders = append(ders, enumerateStore(store)...)
			_ = windows.CertCloseStore(store, 0)
		}
	}

	if len(ders) == 0 {
		return nil, errNoHostStore
	}
	return ders, nil
}

func enumerateStore(store windows.Handle) [][]byte {
	var (
		ders        [][]byte
		certContext *windows.CertContext
		err         error
	)

	for {
		certContext, err = windows.CertEnumCertificatesInStore(store, certContext)
		if err != nil || certContext == nil {
			// Enumeration ends with CRYPT_E_NOT_FOUND and a nil context,
			// which also releases the last one for us.
			return ders
		}

		der := make([]byte, certContext.Length)
		copy(der, unsafe.Slice(certContext.EncodedCert, certContext.Length)) //nolint:gosec // G103: Win32 certificate store API calls
		ders = append(ders, der)
	}
}
