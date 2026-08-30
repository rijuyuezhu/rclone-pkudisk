package pkudisk

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"fmt"
	"net/http"
	"time"
)

// Some AnyShare object-storage endpoints omit the TrustAsia intermediate from
// their TLS chain. Bundle only that public CA certificate; no private material
// is stored here.
//
//go:embed certs/trustasia_ov_tls_rsa_ca_2024.pem
var trustAsiaIntermediate []byte

func newObjectHTTPClient() (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if ok := roots.AppendCertsFromPEM(trustAsiaIntermediate); !ok {
		return nil, fmt.Errorf("parse bundled TrustAsia intermediate certificate")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute}, nil
}
