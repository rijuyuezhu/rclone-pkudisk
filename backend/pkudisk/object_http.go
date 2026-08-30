package pkudisk

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"fmt"
	"net/http"
	"sync"
)

// Some AnyShare object-storage endpoints omit the TrustAsia intermediate from
// their TLS chain. Bundle only that public CA certificate; no private material
// is stored here.
//
//go:embed certs/trustasia_ov_tls_rsa_ca_2024.pem
var trustAsiaIntermediate []byte

var (
	objectHTTPOnce   sync.Once
	objectHTTPClient *http.Client
	objectHTTPError  error
)

func newObjectHTTPClient() (*http.Client, error) {
	objectHTTPOnce.Do(func() {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if ok := roots.AppendCertsFromPEM(trustAsiaIntermediate); !ok {
			objectHTTPError = fmt.Errorf("parse bundled TrustAsia intermediate certificate")
			return
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		// Keep one transport for the process so small-file syncs can reuse idle
		// object-storage connections. There is deliberately no whole-transfer
		// wall-clock timeout; rclone cancels requests through their contexts.
		objectHTTPClient = &http.Client{Transport: transport}
	})
	return objectHTTPClient, objectHTTPError
}
