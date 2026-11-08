package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"regexp"
)

const HostURIPrefix = "urn:monitor:host:"

// MaxMessageBytes bounds snapshots in both directions.
const MaxMessageBytes = 32 << 20

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func ValidHostID(value string) bool {
	return idPattern.MatchString(value)
}

func TLSConfig(cert, key, ca string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}
	pem, err := os.ReadFile(ca)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("CA file contains no certificates")
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: roots, ClientCAs: roots, MinVersion: tls.VersionTLS12}, nil
}
