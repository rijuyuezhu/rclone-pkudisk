package pkudisk

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"fmt"
	"net/http"

	"github.com/rclone/rclone/fs/fshttp"
)

// Some AnyShare object-storage endpoints omit the TrustAsia intermediate from
// their TLS chain. Bundle only that public CA certificate; no private material
// is stored here.
//
//go:embed certs/trustasia_ov_tls_rsa_ca_2024.pem
var trustAsiaIntermediate []byte

func newObjectHTTPClient(ctx context.Context) (*http.Client, error) {
	// Validate the embedded certificate before constructing the client. The
	// fshttp customization callback cannot return an error.
	probe := x509.NewCertPool()
	if ok := probe.AppendCertsFromPEM(trustAsiaIntermediate); !ok {
		return nil, fmt.Errorf("parse bundled TrustAsia intermediate certificate")
	}

	client := fshttp.NewClientCustom(ctx, func(transport *http.Transport) {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = new(tls.Config)
		}
		roots := transport.TLSClientConfig.RootCAs
		if roots == nil {
			var err error
			roots, err = x509.SystemCertPool()
			if err != nil || roots == nil {
				roots = x509.NewCertPool()
			}
		} else {
			// Do not mutate the pool owned by fshttp. In particular, preserve
			// --ca-cert's replacement pool and append the PKU-specific public
			// intermediate to that pool rather than falling back to system roots.
			roots = roots.Clone()
		}
		_ = roots.AppendCertsFromPEM(trustAsiaIntermediate) // prevalidated above
		transport.TLSClientConfig.RootCAs = roots
		if transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
			transport.TLSClientConfig.MinVersion = tls.VersionTLS12
		}
	})
	// There is deliberately no whole-transfer wall-clock timeout; rclone
	// cancels object-storage requests through their contexts. fshttp still
	// supplies rclone's proxy, connect/inactivity timeout, TLS, logging,
	// user-agent, custom-header, and TPS behavior.
	return client, nil
}
