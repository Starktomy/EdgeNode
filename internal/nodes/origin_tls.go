package nodes

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"

	"github.com/TeaOSLab/EdgeCommon/pkg/rpcauth"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
)

// originTLSConfig authenticates the origin independently of HTTP Host rewrites.
// Private roots and the temporary compatibility override are operator settings,
// rather than values an untrusted request can supply.
func originTLSConfig(origin *serverconfigs.OriginConfig, serverName string) (*tls.Config, error) {
	if origin == nil || origin.Addr == nil {
		return nil, errors.New("missing TLS origin address")
	}
	if serverName == "" {
		serverName = origin.Addr.Host
	}
	if origin.Cert != nil && origin.Cert.ServerName != "" {
		serverName = origin.Cert.ServerName
	}
	config, err := rpcauth.VerifiedTLSConfig(serverName, os.Getenv("EDGE_ORIGIN_CA_FILE"))
	if err != nil {
		return nil, err
	}
	if os.Getenv("EDGE_ALLOW_INSECURE_ORIGIN_TLS") == "true" {
		config.InsecureSkipVerify = true // Explicit, operator-controlled migration mode.
	}
	if origin.Cert != nil {
		if origin.Cert.IsCA {
			if config.RootCAs == nil {
				config.RootCAs, err = x509.SystemCertPool()
				if err != nil {
					config.RootCAs = x509.NewCertPool()
				}
			}
			if !config.RootCAs.AppendCertsFromPEM(origin.Cert.CertData) {
				return nil, errors.New("origin CA contains no certificates")
			}
		} else if cert := origin.Cert.CertObject(); cert != nil {
			config.Certificates = []tls.Certificate{*cert}
		}
	}
	return config, nil
}
