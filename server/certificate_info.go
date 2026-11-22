package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"time"
)

type certificateInfo struct {
	Usage       string    `json:"usage"`
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	Fingerprint string    `json:"sha256"`
}

func describeCertificate(usage string, cert *x509.Certificate) certificateInfo {
	hash := sha256.Sum256(cert.Raw)
	return certificateInfo{usage, cert.Subject.String(), cert.Issuer.String(), cert.NotBefore, cert.NotAfter, hex.EncodeToString(hash[:])}
}

func loadedServerCertificates(grpcTLS bool, cert, key string, webTLS bool, webCert, webKey string, issuer *agentIssuer) ([]certificateInfo, error) {
	var result []certificateInfo
	for _, pair := range []struct {
		enabled          bool
		usage, cert, key string
	}{{grpcTLS, "Server gRPC", cert, key}, {webTLS && webCert != "", "Server Web HTTPS", webCert, webKey}} {
		if !pair.enabled {
			continue
		}
		loaded, err := tls.LoadX509KeyPair(pair.cert, pair.key)
		if err != nil {
			return nil, err
		}
		result = append(result, describeCertificate(pair.usage, loaded.Leaf))
	}
	if issuer != nil {
		result = append(result, describeCertificate("Agent 签发 CA", issuer.certificate))
	}
	return result, nil
}

func (s *store) saveHostCertificate(ctx context.Context, id string, cert *certificateInfo) error {
	if cert == nil {
		_, err := s.exec(ctx, "DELETE FROM host_certificates WHERE host_id=?", id)
		return err
	}
	payload, err := json.Marshal(cert)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `INSERT INTO host_certificates(host_id,payload) VALUES(?,?)
		ON CONFLICT(host_id) DO UPDATE SET payload=excluded.payload`, id, payload)
	return err
}
